package main

import (
	"context"

	"errors"

	"fmt"

	"io"

	"os"

	"os/signal"

	"strings"

	"syscall"

	"github.com/liuzhixin405/cove/internal/api"

	"github.com/liuzhixin405/cove/internal/command"

	"github.com/liuzhixin405/cove/internal/config"

	ctxt "github.com/liuzhixin405/cove/internal/context"

	"github.com/liuzhixin405/cove/internal/engine"

	"github.com/liuzhixin405/cove/internal/mcp"

	"github.com/liuzhixin405/cove/internal/memory"

	"github.com/liuzhixin405/cove/internal/permission"

	"github.com/liuzhixin405/cove/internal/plugin"
	"github.com/liuzhixin405/cove/internal/termui"

	"github.com/liuzhixin405/cove/internal/session"

	"github.com/liuzhixin405/cove/internal/skills"

	"github.com/liuzhixin405/cove/internal/state"
)

type providerReloader interface {
	ReloadProvider(provider, model, baseURL, apiKey string) error
}

// chatRunner is the interface needed for runChatInteraction.

type chatRunner interface {
	RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error)
}

var (
	Version = "11.0.0"

	BuildTime = "pro"

	GitCommit = "go go go!"

	dumpPrompt = false

	noAuto = false

	tuiMode = false

	noTUI = false

	profileName = ""

	recordDir = ""

	replayDir = ""
)

func main() {

	opts, err := parseCLIArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(2)
	}

	// Set first: --doctor reports the config of "--profile x --doctor".
	profileName = opts.profile

	switch opts.action {
	case actionVersion:
		outf("cove %s (built %s, commit %s)\n", Version, BuildTime, GitCommit)
		return
	case actionHelp:
		printCLIHelp()
		return
	case actionDoctor:
		runDoctor()
		return
	case actionConfig:
		showConfig()
		return
	case actionListSessions:
		listSessions(opts.listAll)
		return
	}

	debugMode := opts.debug
	dumpPrompt, noAuto, tuiMode, noTUI = opts.dumpPrompt, opts.noAuto, opts.tui, opts.noTUI
	recordDir, replayDir = opts.recordDir, opts.replayDir

	printPrompt := opts.printPrompt
	if opts.printMode {
		// `cat app.log | cove -p "解释"` used to drop the log: -p never read
		// stdin, and without a prompt it fell through to the interactive shell.
		if stdinIsPiped() {
			piped, truncated, err := readPipedStdin(os.Stdin, stdinFirstDataTimeout, maxPipedStdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: 读取 stdin 失败: %v\n", err)
				os.Exit(1)
			}
			if truncated {
				fmt.Fprintf(os.Stderr, "⚠ stdin 超过 %dMB，只发送了前 %dMB\n", maxPipedStdin>>20, maxPipedStdin>>20)
			}
			printPrompt = combinePromptAndStdin(printPrompt, piped)
		}
		if strings.TrimSpace(printPrompt) == "" {
			fmt.Fprintln(os.Stderr, "Error: -p 需要提示内容：cove -p \"提示\"，或通过管道传入，例如 cat app.log | cove -p \"解释\"")
			os.Exit(2)
		}
	}

	app, err := bootstrapApp(debugMode, profileName, recordDir, replayDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Engine start error: %v\n", err)
		os.Exit(1)
	}

	cfg := app.cfg
	eng := app.eng
	permMgr := app.permMgr
	appState := app.appState
	mcpPool := app.mcpPool
	skillMgr := app.skillMgr
	memStore := app.memStore
	pluginMgr := app.pluginMgr
	projCtx := app.projCtx
	toolReg := app.toolReg

	cmdReg := registerAllCommands()

	// Disable background API features if --no-auto flag is set
	if noAuto {
		eng.SetAutoExtract(false)
	}

	if opts.resumeID != "" {
		// Notices go to stderr so "cove -r <id> -p ..." keeps stdout for the
		// answer alone.
		r, warning, err := resumeStartupSession(eng.Store(), opts.resumeID, currentProjectDir(), eng.ResumeSession)
		if err != nil {
			reason := err.Error()
			if errors.Is(err, os.ErrNotExist) {
				reason = "没有这个会话"
			}
			fmt.Fprintf(os.Stderr, "Error: 无法恢复会话 %s: %s\n用 cove --list-sessions all 查看可用会话。\n", opts.resumeID, reason)
			mcpPool.DisconnectAll()
			os.Exit(1)
		}
		if warning != "" {
			fmt.Fprint(os.Stderr, warning)
		}
		fmt.Fprintf(os.Stderr, "已恢复会话: %s (%d 条消息)\n", shortDesc(effectiveHistoryTitle(*r)), len(r.Messages))
		appState.Messages = len(r.Messages)
	}

	// Set up interactive permission prompt for the REPL

	// Startup diagnostic: quick check for critical issues

	runStartupDiagnostics(cfg, debugMode)

	bannerText := termui.Banner(Version, cfg.Model, eng.ProviderName(), string(permMgr.Mode()), projCtx.Cwd, projCtx.GitBranch, "", len(eng.Registry().All()), projCtx.IsGitRepo)

	if dumpPrompt {

		outln(eng.SystemPrompt())

		mcpPool.DisconnectAll()

		return

	}

	if opts.printMode {

		code := runPrintMode(eng, opts.printPrompt, printPrompt, debugMode, opts.attachments, cfg)

		finishSession(eng, mcpPool)

		os.Exit(code)

	}

	if useInteractiveShell() {

		// The interactive shell builds its own command catalogue from the
		// registry (buildCommandList), so nothing has to be assembled here.
		runREPL(bannerText, eng, cmdReg, toolReg, permMgr, appState, cfg, mcpPool, skillMgr, memStore, pluginMgr, projCtx)

		// The shell's /exit and Ctrl+D paths already saved the session and
		// recorded its cost (autoSaveSession); only MCP is left.
		mcpPool.DisconnectAll()

		return

	}

	// Non-TTY (pipes/redirects) or TUI explicitly disabled: use the headless
	// frontend. The classic line REPL has been removed; its behavior lives in
	// the Bubble Tea TUI (interactive) and here (non-interactive).
	runHeadless(bannerText, eng, cmdReg, toolReg, permMgr, appState, cfg, mcpPool, skillMgr, memStore, pluginMgr, projCtx)

	finishSession(eng, mcpPool)

}

func withInterrupt(f func(ctx context.Context)) {

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	sigCh := make(chan os.Signal, 1)

	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(sigCh)

	go func() {

		select {

		case sig := <-sigCh:

			outf("\r\n[中断 - 收到信号 %v]\r\n", sig)

			cancel()

		case <-ctx.Done():

		}

	}()

	f(ctx)

}

func shouldAutoSwitchToVision(warnings []string) bool {

	for _, w := range warnings {

		if strings.Contains(w, nonVisionImageWarning) {

			return true

		}

	}

	return false

}

func preferredVisionModelForProvider(providerName, currentModel string) string {

	if api.IsVisionCapableModel(currentModel) {

		return currentModel

	}

	switch api.NormalizeProviderName(providerName) {

	case "deepseek":

		return "deepseek-v4-flash"

	case "openai", "openai-compatible":

		return "gpt-4o"

	case "anthropic":

		return "claude-sonnet-4-20250514"

	default:

		return ""

	}

}

// shortDesc extracts a one-line short description (max 50 chars) from potentially

// multi-line text. Used to show concise hints in Tab completion.

func shortDesc(s string) string {

	// Take only the first line

	if idx := strings.IndexAny(s, "\n\r"); idx >= 0 {

		s = s[:idx]

	}

	s = strings.TrimSpace(s)

	// Truncate to max length

	const maxLen = 50

	runes := []rune(s)

	if len(runes) > maxLen {

		s = string(runes[:maxLen-1]) + "..."

	}

	return s

}

func isPositiveNumber(input string) bool {

	var idx int

	if _, err := fmt.Sscanf(strings.TrimSpace(input), "%d", &idx); err != nil {

		return false

	}

	return idx > 0

}

// runNeedsAPIKey reports whether a run must stop for a missing API key. -p
// used to send the request anyway, so the user got a 401 or a connection
// error instead of the setup guidance; a --replay run never calls the API.
func runNeedsAPIKey(apiKey string, replaying bool) bool {
	return strings.TrimSpace(apiKey) == "" && !replaying
}

// printPromptIsSlashCommand reports whether the prompt typed after -p is a
// slash command. Only that argument is checked: piped stdin is content, and a
// log starting with "/usr/bin/foo: error" must not be refused as a command.
func printPromptIsSlashCommand(argPrompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(argPrompt), "/")
}

// runPrintMode runs one -p turn and returns the process exit code. argPrompt
// is what the user typed after -p; prompt is the full message, which also
// carries piped stdin. It returns instead of calling os.Exit so main can save
// the session, record its cost and disconnect MCP servers on every outcome.
func runPrintMode(eng *engine.Engine, argPrompt, prompt string, debug bool, attachmentPaths []string, cfg *config.Config) int {
	if printPromptIsSlashCommand(argPrompt) {
		if strings.EqualFold(strings.TrimSpace(argPrompt), "/history clean") {
			handleHistoryClean()
			return 0
		}
		fmt.Fprintln(os.Stderr, "Error: -p 模式不执行 slash 命令（避免命令文本被当作对话写入历史）。请使用交互模式执行该命令。")
		return 1
	}

	if runNeedsAPIKey(cfg.EffectiveProvider().APIKey, replayDir != "") {
		fmt.Fprintln(os.Stderr, missingAPIKeyMessage(cfg.EffectiveProvider().Name))
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())

	defer cancel()

	sigCh := make(chan os.Signal, 1)

	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	defer signal.Stop(sigCh)

	go func() { <-sigCh; cancel() }()

	cwd, _ := os.Getwd()

	userMsg, warnings, err := buildUserMessage(prompt, cwd, attachmentPaths, cfg.Model)

	if err != nil {

		fmt.Fprintf(os.Stderr, "Error: %v\n", err)

		return 1

	}

	if shouldAutoSwitchToVision(warnings) {

		pc := cfg.EffectiveProvider()

		if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {

			if err := applyProviderConfigChange(cfg, eng, func() error {

				cfg.Model = visionModel

				return nil

			}); err == nil {

				// stderr: stdout carries only the answer, so `cove -p ... > out.txt`
				// must not capture this notice.
				fmt.Fprintf(os.Stderr, "[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)

				userMsg, warnings, err = buildUserMessage(prompt, cwd, attachmentPaths, cfg.Model)

				if err != nil {

					fmt.Fprintf(os.Stderr, "Error: %v\n", err)

					return 1

				}

			}

		}

	}

	for _, w := range warnings {

		fmt.Fprintln(os.Stderr, w) // the warning carries its own ⚠

	}

	resp, err := eng.RunMessageWithStream(ctx, userMsg, nil, nil)

	if err != nil {
		return printModeFailure(os.Stderr, ctx.Err() != nil, err)
	}

	outln(resp)

	return 0

}

// printModeFailure reports a failed -p turn on w and returns the exit code.
// Ctrl+C used to print "Error: context canceled" and exit 1, which a script
// cannot tell from an API failure; 130 is the shell convention for SIGINT.
func printModeFailure(w io.Writer, canceled bool, err error) int {
	if canceled {
		fmt.Fprintln(w, "[已取消] 当前任务已终止")
		return 130
	}
	fmt.Fprintf(w, "Error: %v\n", err)
	return 1
}

type replEngineAdapter struct {
	eng *engine.Engine
}

func (a replEngineAdapter) Messages() []api.Message { return a.eng.Messages() }

func (a replEngineAdapter) LoadMessages(msgs []api.Message) { a.eng.LoadMessages(msgs) }

func (a replEngineAdapter) SetSystemOverride(prompt string) { a.eng.SetSystemOverride(prompt) }

func (a replEngineAdapter) SystemPrompt() string { return a.eng.SystemPrompt() }

func (a replEngineAdapter) CostTracker() command.CostTrackerView { return a.eng.CostTracker() }

// The commands look for these by type assertion on the view they are given.
// The adapter used to forward only the five EngineView methods, so /undo,
// /checkpoints and /ratelimit always answered "不可用" in the real program
// while their tests, which hand the command a fake engine, passed.

func (a replEngineAdapter) ListCheckpoints() []string { return a.eng.ListCheckpoints() }

func (a replEngineAdapter) RestoreCheckpoint(commitHash string) (string, error) {
	return a.eng.RestoreCheckpoint(commitHash)
}

func (a replEngineAdapter) RateLimitInfo() api.RateLimitInfo { return a.eng.RateLimitInfo() }

func (a replEngineAdapter) ReloadProvider(provider, model, baseURL, apiKey string) error {
	return a.eng.ReloadProvider(provider, model, baseURL, apiKey)
}

func (a replEngineAdapter) SetPermissionMode(mode permission.Mode) { a.eng.SetPermissionMode(mode) }

func (a replEngineAdapter) SetMaxBudget(maxBudget float64) { a.eng.SetMaxBudget(maxBudget) }

// SetCustomInstructions and SetWorkingDir report true: the engine applies
// both to the running session (/system without replacing the whole prompt,
// /cd moving checkpoints and the session's project along).

func (a replEngineAdapter) SetCustomInstructions(ci string) bool {
	a.eng.SetCustomInstructions(ci)
	return true
}

func (a replEngineAdapter) SetWorkingDir(dir string) bool {
	a.eng.SetWorkingDir(dir)
	return true
}

func (a replEngineAdapter) ResumeSession(r *session.Record) { a.eng.ResumeSession(r) }

func handleCommand(ctx context.Context, input string, reg *command.Registry, cfg *config.Config, eng *engine.Engine, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, permMgr *permission.Manager, projCtx *ctxt.ProjectContext, appState *state.AppState) {

	parts := strings.Fields(input)

	name := strings.TrimPrefix(parts[0], "/")

	c, ok := reg.Find(name)

	if !ok {

		termui.PrintAbove(fmt.Sprintf("未找到命令 /%s。请使用 /help 查看可用命令。\r\n", name))

		return

	}

	cwd, _ := os.Getwd()

	out, err := c.Execute(ctx, command.Input{

		Args: parts[1:],

		Cwd: cwd,

		Config: cfg,

		SaveConfig: config.Save,

		Engine: replEngineAdapter{eng: eng},

		SessionStore: eng.Store(),

		PluginManager: pluginMgr,

		SkillManager: skillMgr,

		MemoryStore: memStore,

		PermissionManager: permMgr,

		MCPPool: mcpPool,

		ProjectContext: projCtx,

		AppState: appState,
	})

	if err != nil {

		termui.PrintAbove(fmt.Sprintf("Error: %v\r\n", err))

		return

	}

	if out.Message != "" {

		termui.PrintAbove(fmt.Sprintf("[%s] %s\r\n", c.Name(), out.Message))

	}

	if out.Data != "" {

		termui.PrintAbove(out.Data + "\r\n")

	}

}

func runDoctor() {

	cwd, _ := os.Getwd()

	c := command.NewDoctorCmd()

	// The config (never the key itself) lets the report say whether an API key
	// is set, the most common first-run problem.
	cfg, _ := config.LoadWithProfile(profileName)

	out, _ := c.Execute(context.Background(), command.Input{Cwd: cwd, Config: cfg})

	outln(out.Message)

}

func handleSkill(input string, eng *engine.Engine) {

	rt := eng.Runtime()

	prompts := rt.SkillPrompts

	parts := strings.Fields(input)

	if len(parts) == 1 || (len(parts) >= 2 && parts[1] == "list") {

		termui.PrintSafe("\n已安装的技能 (%d):\n", len(prompts))

		for name, prompt := range prompts {

			desc := strings.SplitN(prompt, "\n", 2)

			d := ""

			if len(desc) > 0 {

				d = strings.TrimSpace(desc[0])

				if len(d) > 60 {

					d = d[:57] + "..."

				}

			}

			termui.PrintSafe("  %-16s %s\n", name, d)

		}

		return

	}

	switch parts[1] {

	case "marketplace", "registry", "search":

		entries, err := skills.FetchRegistry()

		if err != nil {

			termui.PrintSafe("获取技能市场列表失败: %v\n", err)

			return

		}

		termui.PrintSafe("\n技能市场 (%d 个可用技能):\n", len(entries))

		for _, e := range entries {

			installed := ""

			if _, ok := prompts[e.Name]; ok {

				installed = " [installed]"

			}

			termui.PrintSafe("  %-16s %s%s\n", e.Name, truncateDesc(e.Description, 48), installed)

		}

		termui.PrintSafe("\n使用 /skill install <name> 安装技能\n")

	case "install":

		if len(parts) < 3 {

			termui.PrintSafe("Usage: /skill install <name>\n")

			return

		}

		name := parts[2]

		if _, ok := prompts[name]; ok {

			termui.PrintSafe("Skill '%s' already installed\n", name)

			return

		}

		entries, _ := skills.FetchRegistry()

		for _, e := range entries {

			if e.Name == name {

				_ = skills.InstallSkill(name, "url", e.URL)

				termui.PrintSafe("成功安装技能 %s！现在可以使用 /skill %s 调用它。\n", name, name)

				return

			}

		}

		_ = skills.InstallSkill(name, "local", "")

		termui.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.cove/skills/%s/SKILL.md\n", name, name)

	case "create":

		if len(parts) < 3 {

			termui.PrintSafe("用法: /skill create <name>\n")

			return

		}

		name := parts[2]

		_ = skills.InstallSkill(name, "local", "")

		termui.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.cove/skills/%s/SKILL.md\n", name, name)

	default:

		name := parts[1]

		prompt, ok := prompts[name]

		if !ok {

			termui.PrintSafe("\n未找到技能 %s。您可以使用 /skill marketplace 浏览可用技能，或自建技能。\n", name)

			return

		}

		termui.PrintSafe("\n[Skill %s]\n\n%s\n\n", name, prompt)

	}

}

func applyProviderConfigChange(cfg *config.Config, reloader providerReloader, mutate func() error) error {

	before := cfg.EffectiveProvider()

	beforeModel := strings.TrimSpace(cfg.Model)

	if err := mutate(); err != nil {

		return err

	}

	cfg.Model = strings.TrimSpace(cfg.Model)

	cfg.Provider.Name = strings.TrimSpace(cfg.Provider.Name)

	cfg.Provider.APIKey = strings.TrimSpace(cfg.Provider.APIKey)

	cfg.Provider.BaseURL = strings.TrimSpace(cfg.Provider.BaseURL)

	cfg.Model = config.ResolveModelForProvider(cfg.Model, cfg.Provider.Name)

	if reloader == nil {

		return nil

	}

	after := cfg.EffectiveProvider()

	if beforeModel == cfg.Model && before.Name == after.Name && before.BaseURL == after.BaseURL && before.APIKey == after.APIKey {

		return nil

	}

	return reloader.ReloadProvider(after.Name, cfg.Model, after.BaseURL, after.APIKey)

}

func listSessions(all bool) {

	s, _ := session.NewStore()

	records, _ := s.List()

	hidden := 0

	if !all {

		project := session.FilterByProject(records, currentProjectDir())

		hidden = len(records) - len(project)

		records = project

	}

	if len(records) == 0 {

		outln("没有找到任何会话记录。")

	} else {

		outf("%d 条会话记录:\n", len(records))

		for _, r := range records {

			outf("  %s  %s  (%dt)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("2006-01-02 15:04"))

		}

	}

	if hidden > 0 {

		outf("另有 %d 条其他项目或旧版本的会话未显示，使用 cove --list-sessions all 查看全部。\n", hidden)

	}

}

func printCLIHelp() {

	outln(`cove 是一款基于 Go 的 AI 终端代理工具。


用法:


 cove                       启动交互式 REPL（默认全屏 TUI 界面）


 cove --no-tui              使用 headless 无 UI 模式（适合脚本/管道）

 cove --tui                 即使 stdin/stdout 不是终端也强制使用交互界面


 cove -p, --print <prompt>  执行单次询问：答案输出到 stdout，提示与错误输出到 stderr
                            管道输入会附在提示后: cat app.log | cove -p "解释"
                            退出码: 0 成功, 1 失败, 2 参数错误, 130 被 Ctrl+C 中断


 cove -p <prompt> --image <path> 执行单次带有图片的询问


 cove -p <prompt> --file <path>  执行单次带有文件的询问


 cove -r, --resume <id>     恢复之前的会话记录（可与 -p 连用，继续该会话）

 cove --dump-system-prompt  打印系统提示词后退出

 cove --no-auto             禁用后台自学习功能（记忆提取等额外 API 调用）


 cove --profile <name>      使用指定 profile 启动

 cove --record <dir>        开启会话录制并写入目录

 cove --replay <dir>        使用录制数据回放（不调用真实 API）

 cove --list-sessions [all] 列出当前目录的会话记录（all: 所有项目）


 cove -d, --debug           开启调试模式并打印日志


 cove --doctor              运行环境自检


 cove --config              查看配置文件


 cove -v, --version         输出版本信息


 cove -h, --help            查看帮助信息





插件与技能指令:


 /skill [name]               执行某个技能


 /skill marketplace          查看技能市场


 /skill install <name>       安装技能


 /skill create <name>        创建新的本地技能





REPL 内置命令:


 /model, /profile, /provider, /api-key, /base-url, /record, /mode, /budget


 /cost, /config, /system, /context, /compact


 /attach <path...>, /attach list, /attach remove <id>, /attach clear


 /commit, /review, /diff, /export


 /resume, /memory, /mcp, /plugin


 /doctor, /status, /stats, /permissions


 /cd, /help, /exit`)

	outln("\n提示: 在 prompt 中可以使用 @文件路径 的形式来附带文件。例如: 帮我分析这段日志 @logs/app.log")

}

func truncateDesc(s string, n int) string {

	if len(s) <= n {

		return s

	}

	return s[:n-3] + "..."

}
