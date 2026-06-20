package main

import (
	"context"

	"fmt"

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

	"github.com/liuzhixin405/cove/internal/repl"

	"github.com/liuzhixin405/cove/internal/session"

	"github.com/liuzhixin405/cove/internal/skills"

	"github.com/liuzhixin405/cove/internal/state"

	"github.com/liuzhixin405/cove/internal/tui"
)

type providerReloader interface {
	ReloadProvider(provider, model, baseURL, apiKey string) error
}

// chatRunner is the interface needed for runChatInteraction.

type chatRunner interface {
	RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error)
}

var (
	Version = "5.0.0"

	BuildTime = "pro"

	GitCommit = "unknown"

	resumeMode = false

	resumeID = ""

	dumpPrompt = false

	noAuto = false

	tuiMode = false

	noTUI = false
)

func main() {

	args := os.Args[1:]

	debugMode, printMode := false, false

	var printPrompt string

	var printAttachments []string

	for i := 0; i < len(args); i++ {

		switch args[i] {

		case "-v", "--version":

			fmt.Printf("cove %s (built %s, commit %s)\n", Version, BuildTime, GitCommit)

			return

		case "--help", "-h":

			printCLIHelp()

			return

		case "--doctor":

			runDoctor()

			return

		case "--config":

			showConfig()

			return

		case "--list-sessions":

			listSessions()

			return

		case "--dump-system-prompt":

			dumpPrompt = true

		case "--no-auto":

			noAuto = true

		case "-d", "--debug":

			debugMode = true

		case "-p", "--print":

			if i+1 < len(args) {

				i++

				printPrompt = args[i]

			}

			printMode = true

		case "--image", "--file":

			if i+1 < len(args) {

				i++

				printAttachments = append(printAttachments, args[i])

			}

		case "-r", "--resume":

			if i+1 < len(args) {

				i++

				resumeID = args[i]

			}

			resumeMode = true

		case "--tui":

			tuiMode = true

		case "--no-tui":

			noTUI = true

		default:

			if printMode && printPrompt == "" {

				printPrompt = args[i]

			}

		}

	}

	app, err := bootstrapApp(debugMode)
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

	// Set up interactive permission prompt for the REPL

	configurePermissionPrompt(eng)

	if cfg.SystemPrompt != "" {

		eng.SetSystemOverride(cfg.SystemPrompt)

	}

	if resumeMode && resumeID != "" {

		store := eng.Store()

		if r, err := store.Load(resumeID); err == nil {

			eng.LoadMessages(r.Messages)

			repl.PrintSafe("已恢复会话: %s (%d 条消息)\n", r.Title, len(r.Messages))

		}

	}

	// Startup diagnostic: quick check for critical issues

	runStartupDiagnostics(cfg, debugMode)

	bannerText := repl.Banner(Version, cfg.Model, eng.ProviderName(), string(permMgr.Mode()), projCtx.Cwd, projCtx.GitBranch, "", len(eng.Registry().All()), projCtx.IsGitRepo)

	if dumpPrompt {

		fmt.Println(eng.SystemPrompt())

		return

	}

	if printMode && printPrompt != "" {

		runPrintMode(eng, printPrompt, debugMode, printAttachments, cfg)

		return

	}

	if useTUI() {

		// Build the slash-command catalog for the palette and a runner that
		// executes a command line and returns its rendered output.
		var tuiCommands []tui.CommandItem
		for _, c := range cmdReg.All() {
			tuiCommands = append(tuiCommands, tui.CommandItem{Name: c.Name(), Desc: c.Description()})
		}
		runCommand := func(line string) string {
			parts := strings.Fields(line)
			if len(parts) == 0 {
				return ""
			}
			name := strings.TrimPrefix(parts[0], "/")
			cmd, ok := cmdReg.Find(name)
			if !ok {
				return "未知命令: /" + name + "（输入 /help 查看可用命令）"
			}
			cwd, _ := os.Getwd()
			out, err := cmd.Execute(context.Background(), command.Input{
				Args:              parts[1:],
				Cwd:               cwd,
				Config:            cfg,
				SaveConfig:        config.Save,
				Engine:            replEngineAdapter{eng: eng},
				SessionStore:      eng.Store(),
				PluginManager:     pluginMgr,
				SkillManager:      skillMgr,
				MemoryStore:       memStore,
				PermissionManager: permMgr,
				MCPPool:           mcpPool,
				ProjectContext:    projCtx,
				AppState:          appState,
				Provider:          eng.Provider(),
			})
			if err != nil {
				return "错误: " + err.Error()
			}
			msg := out.Message
			if out.Data != "" {
				if msg != "" {
					msg += "\n"
				}
				msg += out.Data
			}
			return msg
		}

		runTUI(Version, bannerText, debugMode, eng, cfg, projCtx, permMgr, tuiCommands, runCommand)

		return

	}

	runREPL(bannerText, eng, cmdReg, toolReg, permMgr, appState, cfg, mcpPool, skillMgr, memStore, pluginMgr, projCtx)

}

func printBanner(cfg *config.Config, s *state.AppState, pc *ctxt.ProjectContext, pm *permission.Manager, eng *engine.Engine) {

	fmt.Print(repl.Banner(Version, cfg.Model, eng.ProviderName(), string(pm.Mode()), pc.Cwd, pc.GitBranch, "", len(eng.Registry().All()), pc.IsGitRepo))

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

			fmt.Printf("\r\n[中断 - 收到信号 %v]\r\n", sig)

			cancel()

		case <-ctx.Done():

		}

	}()

	f(ctx)

}

func shouldAutoSwitchToVision(warnings []string) bool {

	for _, w := range warnings {

		if strings.Contains(w, "fallback") || strings.Contains(w, "vision") {

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

func runPrintMode(eng *engine.Engine, prompt string, debug bool, attachmentPaths []string, cfg *config.Config) {

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

		os.Exit(1)

	}

	if shouldAutoSwitchToVision(warnings) {

		pc := cfg.EffectiveProvider()

		if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {

			if err := applyProviderConfigChange(cfg, eng, func() error {

				cfg.Model = visionModel

				return nil

			}); err == nil {

				fmt.Printf("[视觉] 检测到图片附件，已自动切换到视觉模型 %s。\n", visionModel)

				userMsg, warnings, err = buildUserMessage(prompt, cwd, attachmentPaths, cfg.Model)

				if err != nil {

					fmt.Fprintf(os.Stderr, "Error: %v\n", err)

					os.Exit(1)

				}

			}

		}

	}

	for _, w := range warnings {

		fmt.Fprintf(os.Stderr, "⚠ %s\n", w)

	}

	resp, err := eng.RunMessageWithStream(ctx, userMsg, nil, nil)

	if err != nil {

		fmt.Fprintf(os.Stderr, "Error: %v\n", err)

		os.Exit(1)

	}

	fmt.Println(resp)

}

type replEngineAdapter struct {
	eng *engine.Engine
}

func (a replEngineAdapter) Messages() []api.Message { return a.eng.Messages() }

func (a replEngineAdapter) LoadMessages(msgs []api.Message) { a.eng.LoadMessages(msgs) }

func (a replEngineAdapter) SetSystemOverride(prompt string) { a.eng.SetSystemOverride(prompt) }

func (a replEngineAdapter) SystemPrompt() string { return a.eng.SystemPrompt() }

func (a replEngineAdapter) CostTracker() command.CostTrackerView { return a.eng.CostTracker() }

func handleCommand(ctx context.Context, input string, reg *command.Registry, cfg *config.Config, eng *engine.Engine, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, permMgr *permission.Manager, projCtx *ctxt.ProjectContext, appState *state.AppState) {

	parts := strings.Fields(input)

	name := strings.TrimPrefix(parts[0], "/")

	c, ok := reg.Find(name)

	if !ok {

		repl.PrintAbove(fmt.Sprintf("未找到命令 /%s。请使用 /help 查看可用命令。\r\n", name))

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

		Provider: eng.Provider(),
	})

	if err != nil {

		repl.PrintAbove(fmt.Sprintf("Error: %v\r\n", err))

		return

	}

	if out.Message != "" {

		repl.PrintAbove(fmt.Sprintf("[%s] %s\r\n", c.Name(), out.Message))

	}

	if out.Data != "" {

		repl.PrintAbove(out.Data + "\r\n")

	}

}

func runDoctor() {

	cwd, _ := os.Getwd()

	c := command.NewDoctorCmd()

	out, _ := c.Execute(context.Background(), command.Input{Cwd: cwd})

	fmt.Println(out.Message)

}

func handleSkill(input string, eng *engine.Engine) {

	rt := eng.Runtime()

	prompts := rt.SkillPrompts

	parts := strings.Fields(input)

	if len(parts) == 1 || (len(parts) >= 2 && parts[1] == "list") {

		repl.PrintSafe("\n已安装的技能 (%d):\n", len(prompts))

		for name, prompt := range prompts {

			desc := strings.SplitN(prompt, "\n", 2)

			d := ""

			if len(desc) > 0 {

				d = strings.TrimSpace(desc[0])

				if len(d) > 60 {

					d = d[:57] + "..."

				}

			}

			repl.PrintSafe("  %-16s %s\n", name, d)

		}

		return

	}

	switch parts[1] {

	case "marketplace", "registry", "search":

		entries, err := skills.FetchRegistry()

		if err != nil {

			repl.PrintSafe("获取技能市场列表失败: %v\n", err)

			return

		}

		repl.PrintSafe("\n技能市场 (%d 个可用技能):\n", len(entries))

		for _, e := range entries {

			installed := ""

			if _, ok := prompts[e.Name]; ok {

				installed = " [installed]"

			}

			repl.PrintSafe("  %-16s %s%s\n", e.Name, truncateDesc(e.Description, 48), installed)

		}

		repl.PrintSafe("\n使用 /skill install <name> 安装技能\n")

	case "install":

		if len(parts) < 3 {

			repl.PrintSafe("Usage: /skill install <name>\n")

			return

		}

		name := parts[2]

		if _, ok := prompts[name]; ok {

			repl.PrintSafe("Skill '%s' already installed\n", name)

			return

		}

		entries, _ := skills.FetchRegistry()

		for _, e := range entries {

			if e.Name == name {

				skills.InstallSkill(name, "url", e.URL)

				repl.PrintSafe("成功安装技能 %s！现在可以使用 /skill %s 调用它。\n", name, name)

				return

			}

		}

		skills.InstallSkill(name, "local", "")

		repl.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.cove/skills/%s/SKILL.md\n", name, name)

	case "create":

		if len(parts) < 3 {

			repl.PrintSafe("用法: /skill create <name>\n")

			return

		}

		name := parts[2]

		skills.InstallSkill(name, "local", "")

		repl.PrintSafe("成功创建本地技能目录 %s，请编辑 ~/.cove/skills/%s/SKILL.md\n", name, name)

	default:

		name := parts[1]

		prompt, ok := prompts[name]

		if !ok {

			repl.PrintSafe("\n未找到技能 %s。您可以使用 /skill marketplace 浏览可用技能，或自建技能。\n", name)

			return

		}

		repl.PrintSafe("\n[Skill %s]\n\n%s\n\n", name, prompt)

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

func listSessions() {

	s, _ := session.NewStore()

	records, _ := s.List()

	if len(records) == 0 {

		fmt.Println("没有找到任何会话记录。")

		return

	}

	fmt.Printf("%d 条会话记录:\n", len(records))

	for _, r := range records {

		fmt.Printf("  %s  %s  (%dt)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("2006-01-02 15:04"))

	}

}

func printCLIHelp() {

	fmt.Println(`cove 是一款基于 Go 的 AI 终端代理工具。


用法:


 cove                       启动交互式 REPL（默认全屏 TUI 界面）


 cove --no-tui              使用经典逐行 REPL（关闭全屏 TUI）


 cove -p <prompt>           执行单次询问并输出结果


 cove -p <prompt> --image <path> 执行单次带有图片的询问


 cove -p <prompt> --file <path>  执行单次带有文件的询问


 cove -r <id>               恢复之前的会话记录


 cove --list-sessions       列出所有会话记录


 cove -d                    开启调试模式并打印日志


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


 /model, /provider, /api-key, /base-url, /mode, /budget


 /cost, /config, /system, /context, /compact


 /attach <path...>, /attach list, /attach remove <id>, /attach clear


 /commit, /review, /diff, /export


 /resume, /memory, /mcp, /plugin


 /doctor, /status, /stats, /permissions


 /cd, /help, /exit`)

	fmt.Println("\n提示: 在 prompt 中可以使用 @文件路径 的形式来附带文件。例如: 帮我分析这段日志 @logs/app.log")

}

func truncateDesc(s string, n int) string {

	if len(s) <= n {

		return s

	}

	return s[:n-3] + "..."

}
