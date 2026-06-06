package main

import (
	"bufio"

	"context"

	"fmt"

	"os"

	"os/signal"

	"strings"

	"syscall"

	"time"

	"github.com/liuzhixin405/cove/internal/api"

	"github.com/liuzhixin405/cove/internal/command"

	"github.com/liuzhixin405/cove/internal/config"

	ctxt "github.com/liuzhixin405/cove/internal/context"

	"github.com/liuzhixin405/cove/internal/diagnostic"

	"github.com/liuzhixin405/cove/internal/engine"

	"github.com/liuzhixin405/cove/internal/hooks"

	"github.com/liuzhixin405/cove/internal/log"

	"github.com/liuzhixin405/cove/internal/mcp"

	"github.com/liuzhixin405/cove/internal/memory"

	"github.com/liuzhixin405/cove/internal/permission"

	"github.com/liuzhixin405/cove/internal/plugin"

	"github.com/liuzhixin405/cove/internal/repl"

	"github.com/liuzhixin405/cove/internal/session"

	"github.com/liuzhixin405/cove/internal/skills"

	"github.com/liuzhixin405/cove/internal/state"

	"github.com/liuzhixin405/cove/internal/tool"
)

type providerReloader interface {
	ReloadProvider(provider, model, baseURL, apiKey string) error
}

// chatRunner is the interface needed for runChatInteraction.

type chatRunner interface {
	RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error)
}

var (
	Version = "3.0.3"

	BuildTime = "dev"

	GitCommit = "unknown"

	resumeMode = false

	resumeID = ""

	dumpPrompt = false

	noAuto = false
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

		default:

			if printMode && printPrompt == "" {

				printPrompt = args[i]

			}

		}

	}

	cfg, err := config.Load()

	if err != nil {

		log.Warnf("config load: %v", err)

		cfg = config.DefaultConfig()

	}

	config.Migrate(cfg, 0)

	if debugMode {

		log.SetLevel(log.Debug)

	}

	pc := cfg.EffectiveProvider()

	projCtx := ctxt.Collect()

	appState := state.NewState()

	appState.Model = cfg.Model

	appState.PermissionMode = cfg.PermissionMode

	appState.MaxBudget = cfg.MaxBudgetUsd

	appState.Debug = debugMode

	permMgr := permission.NewManager(permission.Default)

	if permission.ValidMode(permission.Mode(cfg.PermissionMode)) {

		permMgr.SetMode(permission.Mode(cfg.PermissionMode))

	}

	permMgr.SetBypassAvailable(true)

	classifier := permission.NewClassifier()

	hookMgr := hooks.NewManager()

	skillMgr := skills.NewManager()

	memStore := memory.NewStore()

	pluginMgr := plugin.NewManager()

	mcpPool := mcp.NewPool()

	toolReg := registerAllTools(mcpPool, skillMgr)

	cmdReg := registerAllCommands()

	eng, err := engine.New(engine.Config{

		Model: cfg.Model,

		PermissionMode: string(permMgr.Mode()),

		MaxBudget: cfg.MaxBudgetUsd,

		Debug: debugMode || cfg.Debug,

		Tools: toolReg.All(),

		Provider: api.ProviderConfig{

			Name: pc.Name, APIKey: pc.APIKey, APIKeys: pc.APIKeys, BaseURL: pc.BaseURL,
		},

		MemoryStore: memStore,

		SkillManager: skillMgr,

		HookManager: hookMgr,

		Classifier: classifier,
	})

	if err != nil {

		fmt.Fprintf(os.Stderr, "Engine start error: %v\n", err)

		os.Exit(1)

	}

	eng.SetProjectContext(projCtx)

	// Disable background API features if --no-auto flag is set

	if noAuto {

		eng.SetAutoExtract(false)

	}

	// Set up interactive permission prompt for the REPL

	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {

		desc := ""

		switch toolName {

		case "write":

			if p, ok := input["filePath"].(string); ok {

				desc = p

			}

		case "edit":

			if p, ok := input["filePath"].(string); ok {

				desc = p

			}

		case "bash", "powershell":

			if cmd, ok := input["command"].(string); ok {

				if len(cmd) > 80 {

					cmd = cmd[:80] + "..."

				}

				desc = cmd

			}

		default:

			desc = reason

		}

		repl.PrintAbove(repl.PermissionPrompt(toolName, desc))

		repl.PrintAbove(fmt.Sprintf("\n  %s请输入 (y)确认 / (n)拒绝 / (a)始终允许: %s", repl.Yellow, repl.Reset))

		readAnswer := func() string {

			line, err := bufio.NewReader(os.Stdin).ReadString('\n')

			if err != nil {

				return ""

			}

			return strings.TrimSpace(strings.ToLower(line))

		}

		answer := readAnswer()

		if answer == "" {

			repl.PrintAbove(fmt.Sprintf("\n  %s检测到空输入，请重新输入 (y/n/a):%s", repl.Yellow, repl.Reset))

			answer = readAnswer()

		}

		if answer == "" {

			repl.PrintAbove(fmt.Sprintf("  %s(按 y 确认，n 拒绝，a 全部接受)%s\n", repl.Dim, repl.Reset))

			return false

		}

		switch answer {

		case "y", "yes":

			return true

		case "a", "always":

			// Add a permanent allow rule for this tool

			eng.AddPermissionRule(permission.DAllow, permission.Rule{ToolPattern: toolName})

			return true

		default:

			return false

		}

	}

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

	if issues := diagnostic.QuickCheck(cfg); len(issues) > 0 {

		fmt.Fprintf(os.Stderr, "\n\x1b[33m? 启动时检测到可能的问题：\x1b[0m\n")

		for _, issue := range issues {

			fmt.Fprintf(os.Stderr, "  %s\n", issue.Format())

		}

		fmt.Fprintf(os.Stderr, "  \x1b[2m运行 /diagnose 获取完整报告\x1b[0m\n\n")

	}

	printBanner(cfg, appState, projCtx, permMgr, eng)

	if dumpPrompt {

		fmt.Println(eng.SystemPrompt())

		return

	}

	if printMode && printPrompt != "" {

		runPrintMode(eng, printPrompt, debugMode, printAttachments, cfg)

		return

	}

	runREPL(eng, cmdReg, toolReg, permMgr, appState, cfg, mcpPool, skillMgr, memStore, pluginMgr, projCtx)

}

func registerAllTools(mcpPool *mcp.Pool, skillMgr *skills.Manager) *tool.Registry {

	r := tool.NewRegistry()

	r.Register(tool.NewBashTool())

	r.Register(tool.NewReadTool())

	r.Register(tool.NewWriteTool())

	r.Register(tool.NewEditTool())

	r.Register(tool.NewGrepTool())

	r.Register(tool.NewGlobTool())

	r.Register(tool.NewWebFetchTool())

	r.Register(tool.NewQuestionTool())

	r.Register(tool.NewTodoWriteTool())

	r.Register(tool.NewPowerShellTool())

	r.Register(tool.NewSkillsListTool())

	r.Register(tool.NewSkillViewTool())

	return r

}

func registerAllCommands() *command.Registry {

	r := command.NewRegistry()

	r.Register(command.NewCommitCmd())

	r.Register(command.NewReviewCmd())

	r.Register(command.NewDoctorCmd())

	r.Register(command.NewConfigCmd())

	r.Register(command.NewCompactCmd())

	r.Register(command.NewCostCmd())

	r.Register(command.NewDiffCmd())

	r.Register(command.NewMemoryCmd())

	r.Register(command.NewResumeCmd())

	r.Register(command.NewExportCmd())

	r.Register(command.NewSystemCmd())

	r.Register(command.NewCdCmd())

	r.Register(command.NewContextCmd())

	r.Register(command.NewPermissionsCmd())

	r.Register(command.NewStatusCmd())

	r.Register(command.NewStatsCmd())

	r.Register(command.NewInitCmd())

	r.Register(command.NewDiagnoseCmd())

	r.Register(command.NewPluginCmd())

	r.Register(command.NewDreamCmd())

	return r

}

func printBanner(cfg *config.Config, s *state.AppState, pc *ctxt.ProjectContext, pm *permission.Manager, eng *engine.Engine) {

	fmt.Print(repl.Banner(Version, cfg.Model, eng.ProviderName(), string(pm.Mode()), pc.Cwd, pc.GitBranch, pc.GitStatus, len(eng.Registry().All()), pc.IsGitRepo))

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

func runREPL(eng *engine.Engine, cmdReg *command.Registry, toolReg *tool.Registry, pm *permission.Manager, as *state.AppState, cfg *config.Config, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, projCtx *ctxt.ProjectContext) {

	allCommands := buildCommandList(cmdReg, toolReg)

	// Build skill description map (short descriptions, not full prompts)

	skillDescs := buildSkillDescs(skillMgr)

	reader := repl.New(func(input string) []string {

		return complete(input, allCommands, skillDescs)

	})

	var attachedFiles []string

	historyPickPending := false

	tasks := newREPLTaskRunner(eng)

	// On startup, check for interrupted draft and notify user

	if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {

		title := draft.Title

		if title == "" {

			title = shortDesc(draft.UserContent)

		}

		age := time.Since(draft.UpdatedAt).Truncate(time.Second)

		fmt.Printf("您有一个未完成的片段草稿 (创建于 %v 前)。输入 'continue' 恢复，或直接输入新指令忽略。\n", age)

		fmt.Printf("  \x1b[2m提示: 使用 /history 可查看全部可恢复的历史会话\x1b[0m\n\n")

	}

	for {

		input, err := reader.ReadLine()

		if err == repl.ErrInterrupt {

			if tasks.IsRunning() {

				if tasks.CancelRunning() {

					fmt.Println("[系统] 等待任务停止...")

				}

				continue

			}

			fmt.Println("Type /exit or Ctrl+D to exit.")

			continue

		}

		if err == repl.ErrExit {

			autoSaveSession(eng)

			fmt.Println("Goodbye!")

			return

		}

		if err != nil {

			repl.PrintSafe("\r\n终端读取错误，正在重新初始化 REPL...\r\n")

			reader = repl.New(func(input string) []string {

				return complete(input, allCommands, skillDescs)

			})

			continue

		}

		// If a permission prompt is waiting for an answer, route this line to it

		// instead of processing it as a task.

		if ch := repl.TakePermInputCh(); ch != nil {

			ch <- input

			continue

		}

		if input == "" {

			continue

		}

		if historyPickPending && !strings.HasPrefix(input, "/") {

			if isPositiveNumber(input) {

				handleHistoryResume(input, eng)

				historyPickPending = false

				continue

			}

			historyPickPending = false

		}

		switch {

		case input == "exit" || input == "/exit":

			// Wait briefly for the task goroutine to finish its cleanup (save draft)

			if tasks.CancelRunning() {

				_ = tasks.WaitIdleUntil(time.Now().Add(3 * time.Second))

			}

			autoSaveSession(eng)

			fmt.Println("Goodbye!")

			return

		case isContinueCommand(input):

			if tasks.IsRunning() {

				fmt.Println("[提示] 当前有任务正在运行，请等待其结束后再重试。")

				historyPickPending = false

				continue

			}

			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {

				fmt.Println(budgetExceededRetryHint(eng.CostTracker()))

				historyPickPending = false

				continue

			}

			pf := tasks.PendingFailed()

			if pf != nil {

				if isLowSignalResumeInput(pf.Content) {

					tasks.ClearPendingFailed()

					_ = clearInterruptedDraft()

					fmt.Println("[提示] 已为您推荐相关历史任务...")

					handleHistoryResumeMostRelevant(eng)

					historyPickPending = false

					continue

				}

				tasks.ClearPendingFailed()

				_, merged := tasks.Enqueue(*pf)

				if merged {

					fmt.Println("[恢复] 任务记录已合并。")

				} else {

					fmt.Println("[恢复] 任务已排队，即将开始处理。")

				}

				tasks.WaitIdle()

				historyPickPending = false

				continue

			}

			if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {

				if isLowSignalResumeInput(draft.UserContent) {

					_ = clearInterruptedDraft()

					fmt.Println("[提示] 已为您推荐相关历史任务...")

					handleHistoryResumeMostRelevant(eng)

					historyPickPending = false

					continue

				}

				msg := api.Message{Role: "user", Content: draft.UserContent}

				_, merged := tasks.Enqueue(msg)

				if merged {

					fmt.Println("[恢复] 任务记录已合并。")

				} else {

					fmt.Println("[恢复] 任务已排队，即将开始处理。")

				}

				tasks.WaitIdle()

				historyPickPending = false

				continue

			}

			handleHistoryResumeMostRelevant(eng)

			historyPickPending = false

			continue

		case input == "/help":

			printHelp(cmdReg, toolReg)

		case input == "/":

			showQuickCommands(allCommands)

			continue

		case input == "/doctor":

			runDoctor()

		case input == "/attach" || strings.HasPrefix(input, "/attach "):

			cwd, _ := os.Getwd()

			handleAttachCommand(input, cwd, &attachedFiles)

		case input == "/skill" || strings.HasPrefix(input, "/skill ") || input == "/skills" || strings.HasPrefix(input, "/skills "):

			handleSkill(input, eng)

			continue

		case handleBuiltinConfigCommand(input, cfg, eng, pm, as):

			continue

		case handleSessionCommand(input, eng, &historyPickPending):

			continue

		case strings.HasPrefix(input, "/"):

			if handleUnknownCmd(input, cmdReg) {

				continue

			}

			withInterrupt(func(ctx context.Context) {

				handleCommand(ctx, input, cmdReg, cfg, eng, mcpPool, skillMgr, memStore, pluginMgr, pm, projCtx, as)

			})

		default:

			pc := cfg.EffectiveProvider()

			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {

				fmt.Println(budgetExceededRetryHint(eng.CostTracker()))

				continue

			}

			if pc.APIKey == "" {

				fmt.Println(missingAPIKeyMessage(pc.Name))

				continue

			}

			cwd, _ := os.Getwd()

			userMsg, warnings, err := buildUserMessage(input, cwd, attachedFiles, cfg.Model)

			if err != nil {

				fmt.Printf("警告: 构建用户消息时出错: %v\n", err)

				continue

			}

			if shouldAutoSwitchToVision(warnings) {

				if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {

					if err := applyProviderConfigChange(cfg, eng, func() error {

						cfg.Model = visionModel

						as.Model = visionModel

						return nil

					}); err == nil {

						fmt.Println("[恢复] 任务记录已合并。")

						userMsg, warnings, err = buildUserMessage(input, cwd, attachedFiles, cfg.Model)

						if err != nil {

							fmt.Printf("警告: 构建用户消息时出错: %v\n", err)

							continue

						}

					}

				}

			}

			// Print warnings (e.g., non-vision model with image)

			for _, w := range warnings {

				fmt.Printf("  \x1b[33m%s\x1b[0m\n", w)

			}

			// Auto-clear attachments after sending (avoids resending images every turn)

			if len(attachedFiles) > 0 {

				attachedFiles = nil

			}

			queuedAhead, merged := tasks.Enqueue(userMsg)

			if merged {

				if queuedAhead > 0 {

					fmt.Printf("[已补充] 先前任务还在跑，前面排队: %d\n", queuedAhead)

				} else {

					fmt.Println("[已补充] 已合并进当前处理任务")

				}

			} else if queuedAhead > 0 {

				fmt.Printf("[任务排队中] 前方排队数: %d\n", queuedAhead)

			} else {

				fmt.Println("[输入已接收]")

			}

			tasks.WaitIdle()

		}

	}

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

		return "deepseek-chat"

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

				fmt.Println("[恢复] 任务记录已合并。")

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

	resp, err := eng.RunMessageWithStream(ctx, userMsg, nil)

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

		fmt.Printf("未找到命令 /%s。请使用 /help 查看可用命令。\n", name)

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

		fmt.Printf("Error: %v\n", err)

		return

	}

	if out.Message != "" {

		fmt.Printf("[%s] %s\n", c.Name(), out.Message)

	}

	if out.Data != "" {

		fmt.Println(out.Data)

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

func isSkillInvocation(input string, eng *engine.Engine) bool {

	if eng == nil || eng.Runtime() == nil {

		return false

	}

	parts := strings.Fields(input)

	if len(parts) == 0 || !strings.HasPrefix(parts[0], "/") {

		return false

	}

	name := strings.TrimPrefix(parts[0], "/")

	if name == "" || name == "skill" || name == "skills" {

		return false

	}

	_, ok := eng.Runtime().SkillPrompts[name]

	return ok

}

func handleSkillInvocation(input string, eng *engine.Engine) {

	parts := strings.Fields(input)

	if len(parts) == 0 {

		return

	}

	name := strings.TrimPrefix(parts[0], "/")

	prompt, ok := eng.Runtime().SkillPrompts[name]

	if !ok {

		repl.PrintSafe("未找到配置文件: %s\n", name)

		return

	}

	args := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))

	repl.PrintSafe("\n[Skill: %s]\n\n%s\n", name, prompt)

	if args != "" {

		repl.PrintSafe("\n无效的参数: %s\n", args)

	}

	repl.PrintSafe("\n")

}

func visibleRuneWidth(s string) int {

	return len([]rune(s))

}

func maxInt(a, b int) int {

	if a > b {

		return a

	}

	return b

}

func minInt(a, b int) int {

	if a < b {

		return a

	}

	return b

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


 cove                       启动交互式 REPL


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
