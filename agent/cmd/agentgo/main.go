package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentgo/internal/agent"
	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/buddy"
	"github.com/agentgo/internal/command"
	"github.com/agentgo/internal/config"
	ctxt "github.com/agentgo/internal/context"
	"github.com/agentgo/internal/cost"
	"github.com/agentgo/internal/diagnostic"
	"github.com/agentgo/internal/engine"
	"github.com/agentgo/internal/hooks"
	"github.com/agentgo/internal/log"
	"github.com/agentgo/internal/mcp"
	"github.com/agentgo/internal/memory"
	"github.com/agentgo/internal/onboarding"
	"github.com/agentgo/internal/permission"
	"github.com/agentgo/internal/plugin"
	"github.com/agentgo/internal/repl"
	"github.com/agentgo/internal/session"
	"github.com/agentgo/internal/skills"
	"github.com/agentgo/internal/state"
	"github.com/agentgo/internal/tool"
)

type providerReloader interface {
	ReloadProvider(provider, model, baseURL, apiKey string) error
}

// chatRunner is the interface needed for runChatInteraction.
type chatRunner interface {
	RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error)
}

var (
	Version    = "1.0.0"
	BuildTime  = "dev"
	GitCommit  = "unknown"
	resumeMode = false
	resumeID   = ""
	dumpPrompt = false
	noAuto     = false
)

func main() {
	args := os.Args[1:]
	debugMode, printMode := false, false
	var printPrompt string
	var printAttachments []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-v", "--version":
			fmt.Printf("agentgo %s (built %s, commit %s)\n", Version, BuildTime, GitCommit)
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

	// Parallel initialization of IO-heavy subsystems
	var initWg sync.WaitGroup
	initWg.Add(2)
	go func() { defer initWg.Done(); skills.LoadAll(skillMgr, projCtx.Cwd) }()
	go func() { defer initWg.Done(); pluginMgr.Init() }()

	mcpPool := mcp.NewPool()
	for name, sc := range cfg.MCPServers {
		go func(n string, s config.MCPServerConfig) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			mcpPool.Connect(ctx, n, mcp.ServerConfig{
				Command: s.Command, Args: s.Args, Env: s.Env, Type: s.Type, URL: s.URL,
			})
		}(name, sc)
	}

	// Wait for skills and plugins to finish loading before using them
	initWg.Wait()

	toolReg := registerAllTools(mcpPool, skillMgr)
	cmdReg := registerAllCommands()

	eng, err := engine.New(engine.Config{
		Model:          cfg.Model,
		PermissionMode: string(permMgr.Mode()),
		MaxBudget:      cfg.MaxBudgetUsd,
		Debug:          debugMode || cfg.Debug,
		Tools:          toolReg.All(),
		Provider: api.ProviderConfig{
			Name: pc.Name, APIKey: pc.APIKey, APIKeys: pc.APIKeys, BaseURL: pc.BaseURL,
		},
		MemoryStore:  memStore,
		SkillManager: skillMgr,
		HookManager:  hookMgr,
		Classifier:   classifier,
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
		fmt.Print(repl.PermissionPrompt(toolName, desc))
		fmt.Printf("  %s允许？ (y)确认 / (n)拒绝 / (a)始终允许:%s ", repl.Yellow, repl.Reset)

		readAnswer := func() string {
			// Read answer byte-by-byte from stdin to avoid buffered reads
			// that could consume user input meant for the next ReadLine call.
			var answerBytes []byte
			var b [1]byte
			for {
				n, err := os.Stdin.Read(b[:])
				if err != nil || n == 0 {
					break
				}
				if b[0] == '\n' || b[0] == '\r' {
					break
				}
				answerBytes = append(answerBytes, b[0])
			}
			return strings.TrimSpace(strings.ToLower(string(answerBytes)))
		}

		answer := readAnswer()
		if answer == "" {
			// Some terminals may leave a stale CR/LF in stdin. Confirm once
			// before denying to avoid accidental auto-reject.
			fmt.Printf("\n  %s检测到空输入，请再次输入 (y/n/a):%s ", repl.Yellow, repl.Reset)
			answer = readAnswer()
		}
		if answer == "" {
			fmt.Printf("  %s(未输入，默认拒绝)%s\n", repl.Dim, repl.Reset)
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

	agtRunner := agent.NewRunner(agent.Config{
		Model:     cfg.Model,
		Provider:  api.ProviderConfig{Name: pc.Name, APIKey: pc.APIKey, BaseURL: pc.BaseURL},
		Tools:     toolReg.All(),
		MaxBudget: cfg.MaxBudgetUsd,
		Debug:     debugMode,
	})
	agtRunner.Register("general", "General-purpose sub-agent for complex multi-step tasks.", "You are a sub-agent. Complete the assigned task thoroughly and return a clear result.")
	agtRunner.Register("explore", "Exploration agent. Searches codebase and gathers context.", "You are a code explorer. Gather relevant context and report findings concisely.")
	agtRunner.Register("plan", "Planning agent. Designs solutions and creates plans.", "You are a planning agent. Create detailed, structured plans with clear steps.")
	agtRunner.Register("review", "Code review agent. Checks for bugs, style, security.", "You are a code reviewer. Check for correctness, style, performance, and security.")
	agtRunner.Register("test", "Testing agent. Writes and runs tests.", "You are a testing agent. Write comprehensive tests and run them.")
	eng.Runtime().AgentRunner = agtRunner

	_ = pluginMgr

	if resumeMode && resumeID != "" {
		store := eng.Store()
		if r, err := store.Load(resumeID); err == nil {
			eng.LoadMessages(r.Messages)
			repl.PrintSafe("已恢复会话: %s (%d 条消息)\n", r.Title, len(r.Messages))
		}
	}

	// Startup diagnostic: quick check for critical issues
	if issues := diagnostic.QuickCheck(cfg); len(issues) > 0 {
		fmt.Fprintf(os.Stderr, "\n\x1b[33m⚠ 启动诊断发现问题:\x1b[0m\n")
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
		runPrintMode(eng, printPrompt, debugMode, printAttachments, cfg.Model)
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
	r.Register(tool.NewWebSearchTool())
	r.Register(tool.NewQuestionTool())
	r.Register(tool.NewTodoWriteTool())
	r.Register(tool.NewPlanModeTool())
	r.Register(tool.NewExitPlanModeTool())
	r.Register(tool.NewEnterWorktreeTool())
	r.Register(tool.NewExitWorktreeTool())
	r.Register(tool.NewTaskCreateTool())
	r.Register(tool.NewTaskListTool())
	r.Register(tool.NewTaskUpdateTool())
	r.Register(tool.NewTaskStopTool())
	r.Register(tool.NewTaskGetTool())
	r.Register(tool.NewTaskOutputTool())
	r.Register(tool.NewSleepTool())
	r.Register(tool.NewBriefTool())
	r.Register(tool.NewSkillTool())
	r.Register(tool.NewAgentTool())
	r.Register(tool.NewTeamCreateTool())
	r.Register(tool.NewTeamDeleteTool())
	r.Register(tool.NewCronTool())
	r.Register(tool.NewSendMessageTool())
	r.Register(tool.NewLSPTool())
	r.Register(tool.NewPowerShellTool())
	r.Register(tool.NewMCPTool(mcpPool))
	r.Register(tool.NewListMCPResourcesTool(mcpPool))
	r.Register(tool.NewReadMCPResourceTool(mcpPool))
	_ = skillMgr
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
	r.Register(command.NewMcpCmd())
	r.Register(command.NewPluginCmd())
	r.Register(command.NewSkillsCmd())
	r.Register(command.NewExportCmd())
	r.Register(command.NewSystemCmd())
	r.Register(command.NewCdCmd())
	r.Register(command.NewContextCmd())
	r.Register(command.NewPermissionsCmd())
	r.Register(command.NewStatusCmd())
	r.Register(command.NewStatsCmd())
	r.Register(command.NewDreamCmd())
	r.Register(command.NewBuddyCmd())
	r.Register(command.NewInitCmd())
	r.Register(command.NewDiagnoseCmd())
	return r
}

func printBanner(cfg *config.Config, s *state.AppState, pc *ctxt.ProjectContext, pm *permission.Manager, eng *engine.Engine) {
	fmt.Print(repl.Banner(Version, cfg.Model, eng.ProviderName(), string(pm.Mode()), pc.Cwd, pc.GitBranch, pc.GitStatus, len(eng.Registry().All()), pc.IsGitRepo))

	// Project onboarding check
	obs := onboarding.Check(pc.Cwd)
	if obs.NeedsOnboarding() {
		fmt.Fprintf(os.Stderr, "\n  %s💡 未找到 CLAUDE.md。输入 /init 生成项目指南。%s\n", repl.Dim, repl.Reset)
	}

	// Buddy greeting at session start
	if eng.BuddyDisplay != nil {
		_ = eng.BuddyDisplay.ReactWithMood(buddy.EventStart, "")
		fmt.Fprintf(os.Stderr, "\n%s\n", buddyFloatingBox(eng.BuddyDisplay))
		// Show daily fortune + time greeting
		fmt.Fprintf(os.Stderr, "  \x1b[33m%s %s\x1b[0m\n", buddy.TimeEmoji(), buddy.TimeGreeting())
		fmt.Fprintf(os.Stderr, "  \x1b[35m🔮 %s\x1b[0m\n\n", eng.BuddyDisplay.Fortune.Today())
	}
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
			if sig == syscall.SIGTERM {
				os.Exit(0)
			}
			fmt.Print("\r\n[已中断]\r\n")
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

	// Start buddy proactive idle chat listener
	if eng.BuddyDisplay != nil {
		go func() {
			for msg := range eng.BuddyDisplay.IdleChatCh() {
				// Print idle chat above the prompt line
				fmt.Fprintf(os.Stderr, "\r\n  \x1b[36m💬 %s: %s\x1b[0m\r\n", eng.BuddyDisplay.Name(), msg)
			}
		}()
		defer eng.BuddyDisplay.StopIdleChat()
	}

	for {
		input, err := reader.ReadLine()
		if err == repl.ErrInterrupt {
			fmt.Println("输入 /exit 或 Ctrl+D 退出")
			continue
		}
		if err == repl.ErrExit {
			autoSaveSession(eng)
			fmt.Println("再见！")
			return
		}
		if err != nil {
			repl.PrintSafe("\r\n读取错误，正在重启 REPL...\r\n")
			reader = repl.New(func(input string) []string {
				return complete(input, allCommands, skillDescs)
			})
			continue
		}
		if input == "" {
			continue
		}
		// Mark activity to reset buddy idle timer
		if eng.BuddyDisplay != nil {
			eng.BuddyDisplay.MarkActivity()
		}
		switch {
		case input == "exit" || input == "/exit":
			autoSaveSession(eng)
			fmt.Println("再见！")
			return
		case input == "/help":
			printHelp(cmdReg, toolReg)
		case input == "/":
			showQuickCommands(allCommands)
			continue
		case input == "/doctor":
			runDoctor()
		case input == "/config":
			showConfig()
		case input == "/attach" || strings.HasPrefix(input, "/attach "):
			cwd, _ := os.Getwd()
			handleAttachCommand(input, cwd, &attachedFiles)
		case strings.HasPrefix(input, "/model "):
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Model = config.ResolveModelForProvider(strings.TrimPrefix(input, "/model "), cfg.Provider.Name)
				as.Model = cfg.Model
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("模型更新失败: %v\n", err)
				continue
			}
			fmt.Printf("模型: %s（已保存）\n", cfg.Model)
		case strings.HasPrefix(input, "/provider "):
			providerName := strings.TrimSpace(strings.TrimPrefix(input, "/provider "))
			if !api.IsKnownProvider(providerName) {
				fmt.Printf("无效的供应商: %s\n", providerName)
				fmt.Println(providerHelpLine())
				continue
			}
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Provider.Name = providerName
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("供应商更新失败: %v\n", err)
				continue
			}
			fmt.Printf("供应商: %s（已保存）\n", cfg.Provider.Name)
		case strings.HasPrefix(input, "/api-key "):
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Provider.APIKey = strings.TrimSpace(strings.TrimPrefix(input, "/api-key "))
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("API 密钥更新失败: %v\n", err)
				continue
			}
			fmt.Println("API 密钥已保存")
		case strings.HasPrefix(input, "/base-url "):
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Provider.BaseURL = strings.TrimSpace(strings.TrimPrefix(input, "/base-url "))
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("Base URL 更新失败: %v\n", err)
				continue
			}
			fmt.Println("Base URL 已保存")
		case strings.HasPrefix(input, "/mode "):
			m := permission.Mode(strings.TrimPrefix(input, "/mode "))
			if permission.ValidMode(m) {
				pm.SetMode(m)
				eng.SetPermissionMode(m)
				cfg.PermissionMode = string(m)
				as.PermissionMode = string(m)
				config.Save(cfg)
				fmt.Printf("模式: %s\n", m)
			} else {
				fmt.Printf("无效模式。可选: %s\n", permission.Modes())
			}
		case strings.HasPrefix(input, "/budget "):
			var b float64
			fmt.Sscanf(strings.TrimPrefix(input, "/budget "), "%f", &b)
			if b > 0 {
				cfg.MaxBudgetUsd = b
				as.MaxBudget = b
				config.Save(cfg)
				fmt.Printf("预算: $%.2f\n", b)
			}
		case input == "/cost":
			fmt.Println("本次会话:", eng.CostTracker().Summary())
			ch := cost.NewCostHistory()
			if len(ch.Records) > 0 {
				fmt.Printf("近 24小时: $%.4f | 近 7天: $%.4f | 总计: $%.4f (%d 个会话)\n",
					ch.Last24Hours(), ch.Last7Days(), ch.TotalAllTime(), len(ch.Records))
			}
		case strings.HasPrefix(input, "/export"):
			handleExport(input, eng)
		case input == "/undo":
			if eng.CheckpointMgr() != nil {
				if err := eng.CheckpointMgr().Restore(""); err != nil {
					fmt.Printf("回退失败: %v\n", err)
				} else {
					fmt.Println("✓ 已回退到上一个检查点")
				}
			} else {
				fmt.Println("检查点功能未初始化")
			}
		case input == "/checkpoints":
			if eng.CheckpointMgr() != nil {
				list := eng.CheckpointMgr().List()
				if len(list) == 0 {
					fmt.Println("暂无检查点")
				} else {
					fmt.Println("检查点记录:")
					for _, cp := range list {
						fmt.Println("  " + cp)
					}
				}
			}
		case input == "/ratelimit":
			if eng.RateLimits() != nil {
				info := eng.RateLimits().Current()
				if info.HasData() {
					fmt.Println("速率限制: " + info.Format())
				} else {
					fmt.Println("暂无速率限制数据（需至少一次 API 调用）")
				}
			}
		case strings.HasPrefix(input, "/resume") || input == "/resume":
			sessionID := ""
			if strings.HasPrefix(input, "/resume ") {
				sessionID = strings.TrimPrefix(input, "/resume ")
			}
			withInterrupt(func(ctx context.Context) { handleResume(ctx, sessionID, eng) })
		case input == "/history":
			handleHistory(eng)
		case strings.HasPrefix(input, "/history "):
			histID := strings.TrimSpace(strings.TrimPrefix(input, "/history "))
			handleHistoryResume(histID, eng)
		case input == "/compact":
			withInterrupt(func(ctx context.Context) {
				eng.Compact(ctx)
				fmt.Println("上下文窗口已压缩。")
			})
		case strings.HasPrefix(input, "/skill") || input == "/skills":
			handleSkill(input, eng)
		case isSkillInvocation(input, eng):
			handleSkillInvocation(input, eng)
		case strings.HasPrefix(input, "/"):
			if handleUnknownCmd(input, cmdReg) {
				continue
			}
			withInterrupt(func(ctx context.Context) {
				handleCommand(ctx, input, cmdReg, cfg, eng, mcpPool, skillMgr, memStore, pluginMgr, pm, projCtx, as)
			})
		default:
			pc := cfg.EffectiveProvider()
			if pc.APIKey == "" {
				fmt.Println(missingAPIKeyMessage(pc.Name))
				continue
			}
			cwd, _ := os.Getwd()
			userMsg, warnings, err := buildUserMessage(input, cwd, attachedFiles, cfg.Model)
			if err != nil {
				fmt.Printf("附件处理失败: %v\n", err)
				continue
			}
			// Print warnings (e.g., non-vision model with image)
			for _, w := range warnings {
				fmt.Printf("  \x1b[33m%s\x1b[0m\n", w)
			}
			// Auto-clear attachments after sending (avoids resending images every turn)
			if len(attachedFiles) > 0 {
				attachedFiles = nil
			}
			withInterrupt(func(ctx context.Context) { runChatInteractionMessage(ctx, eng, userMsg) })
			// Progressive onboarding: show tool progress hint on first interaction
			if eng.OnboardHints() != nil {
				if hint := eng.OnboardHints().Show(onboarding.HintToolProgress); hint != "" {
					fmt.Fprint(os.Stderr, hint)
				}
			}
			// Show rate limit status if getting low
			if eng.RateLimits() != nil {
				info := eng.RateLimits().Current()
				if info.HasData() && info.TokensRemaining > 0 && info.TokensLimit > 0 {
					pct := info.TokensRemaining * 100 / info.TokensLimit
					if pct < 20 {
						fmt.Fprintf(os.Stderr, "  \x1b[33m⚠ %s\x1b[0m\n", info.Format())
					}
				}
			}
			// Buddy reaction after text responses
			if eng.BuddyDisplay != nil {
				quip := eng.BuddyDisplay.ReactWithMood(buddy.EventTurn, "")
				if quip != nil {
					fmt.Fprintf(os.Stderr, "%s\n", buddyFloatingBox(eng.BuddyDisplay))
				}
			}
			// Display follow-up suggestions if available
			if sug := eng.Suggestions(); len(sug) > 0 {
				fmt.Fprintf(os.Stderr, "  %s💡 建议:%s", repl.Dim, repl.Reset)
				for _, s := range sug {
					fmt.Fprintf(os.Stderr, " %s%s%s |", repl.Dim, s.Text, repl.Reset)
				}
				fmt.Fprintln(os.Stderr)
			}
		}
	}
}

type cmdEntry struct {
	Name     string
	Desc     string
	Type     string
	Args     []string
	ArgHints map[string][]string
}

func buildCommandList(cmdReg *command.Registry, toolReg *tool.Registry) []cmdEntry {
	var list []cmdEntry
	for _, c := range cmdReg.All() {
		list = append(list, cmdEntry{Name: "/" + c.Name(), Desc: c.Description(), Type: "cmd"})
	}
	list = append(list,
		cmdEntry{Name: "/model", Desc: "设置模型", Type: "config"},
		cmdEntry{Name: "/provider", Desc: "设置供应商", Type: "config", ArgHints: map[string][]string{"": providerNameSuggestions()}},
		cmdEntry{Name: "/api-key", Desc: "设置 API 密钥", Type: "config"},
		cmdEntry{Name: "/base-url", Desc: "设置 API 地址", Type: "config"},
		cmdEntry{Name: "/mode", Desc: "设置权限模式 (default|plan|auto|bypass)", Type: "config", ArgHints: map[string][]string{"": {"default", "plan", "auto", "bypass"}}},
		cmdEntry{Name: "/budget", Desc: "设置预算上限 ($)", Type: "config"},
		cmdEntry{Name: "/undo", Desc: "回退到上一个检查点", Type: "builtin"},
		cmdEntry{Name: "/checkpoints", Desc: "列出所有检查点", Type: "builtin"},
		cmdEntry{Name: "/ratelimit", Desc: "查看 API 速率限制", Type: "builtin"},
		cmdEntry{Name: "/attach", Desc: "挂载图片或文件到后续提问", Type: "builtin", ArgHints: map[string][]string{"": {"list", "clear", "remove", "add"}}},
		cmdEntry{Name: "/help", Desc: "显示帮助", Type: "builtin"},
		cmdEntry{Name: "/exit", Desc: "退出", Type: "builtin"},
		cmdEntry{Name: "/history", Desc: "查看和继续历史会话", Type: "builtin"},
	)
	for _, t := range toolReg.All() {
		d := t.Def()
		args := toolArgNames(d.InputSchema)
		list = append(list, cmdEntry{Name: d.Name, Desc: d.Description, Type: "tool", Args: args})
		for _, alias := range d.Aliases {
			list = append(list, cmdEntry{Name: alias, Desc: d.Description, Type: "tool", Args: args})
		}
	}
	return list
}

// buildSkillDescs creates a map of skill name → short description for Tab completion.
func buildSkillDescs(mgr *skills.Manager) map[string]string {
	descs := make(map[string]string)
	if mgr == nil {
		return descs
	}
	for _, s := range mgr.All() {
		if s.Description != "" {
			descs[s.Name] = s.Description
		} else {
			descs[s.Name] = "技能"
		}
	}
	return descs
}

func complete(input string, commands []cmdEntry, skills map[string]string) []string {
	input = strings.TrimLeft(input, " \t")
	if input == "" {
		return nil
	}
	if suggestions := completeArgs(input, commands); len(suggestions) > 0 {
		return suggestions
	}
	// Build description lookup
	descMap := make(map[string]string, len(commands))
	for _, c := range commands {
		descMap[strings.ToLower(c.Name)] = c.Desc
	}

	var matches []string
	lower := strings.ToLower(input)
	for _, c := range commands {
		if strings.HasPrefix(input, "/") && c.Type == "tool" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(c.Name), lower) {
			if c.Desc != "" {
				matches = append(matches, c.Name+"\t"+shortDesc(c.Desc))
			} else {
				matches = append(matches, c.Name)
			}
		}
	}
	// Build a set of existing command names to avoid duplicates with skills
	cmdNames := make(map[string]bool, len(commands))
	for _, c := range commands {
		cmdNames[strings.ToLower(c.Name)] = true
	}
	for name, desc := range skills {
		candidate := name
		if strings.HasPrefix(input, "/") {
			candidate = "/" + name
		}
		// Skip if already exists as a command
		if cmdNames[strings.ToLower(candidate)] {
			continue
		}
		if strings.HasPrefix(strings.ToLower(candidate), lower) {
			if desc != "" {
				matches = append(matches, candidate+"\t"+shortDesc(desc))
			} else {
				matches = append(matches, candidate+"\t"+"技能")
			}
		}
	}
	sort.Strings(matches)
	return matches
}

func completeArgs(input string, commands []cmdEntry) []string {
	head, rest, ok := strings.Cut(input, " ")
	if !ok {
		return nil
	}
	entry, found := findCompletionEntry(head, commands)
	if !found {
		return nil
	}
	rest = strings.TrimLeft(rest, " \t")
	base := head + " "
	if hints := entry.ArgHints[""]; len(hints) > 0 {
		return completeValueHints(base, rest, hints)
	}
	if len(entry.Args) == 0 {
		return nil
	}
	used := usedArgNames(rest)
	current := currentArgPrefix(rest)
	var matches []string
	for _, arg := range entry.Args {
		if used[arg] {
			continue
		}
		if current == "" || strings.HasPrefix(strings.ToLower(arg), strings.ToLower(current)) {
			matches = append(matches, base+replaceCurrentArgPrefix(rest, current, arg+"="))
		}
	}
	return matches
}

func completeValueHints(base, rest string, hints []string) []string {
	current := strings.TrimSpace(rest)
	var matches []string
	for _, hint := range hints {
		if current == "" || strings.HasPrefix(strings.ToLower(hint), strings.ToLower(current)) {
			matches = append(matches, base+hint)
		}
	}
	return matches
}

func findCompletionEntry(name string, commands []cmdEntry) (cmdEntry, bool) {
	for _, c := range commands {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return cmdEntry{}, false
}

func usedArgNames(input string) map[string]bool {
	used := map[string]bool{}
	for _, part := range strings.Fields(input) {
		part = strings.Trim(part, ` "'{},`)
		if key, _, ok := strings.Cut(part, "="); ok {
			used[strings.Trim(key, ` "'`)] = true
			continue
		}
		if key, _, ok := strings.Cut(part, ":"); ok {
			used[strings.Trim(key, ` "'`)] = true
		}
	}
	return used
}

func currentArgPrefix(input string) string {
	input = strings.TrimRight(input, " \t")
	if input == "" {
		return ""
	}
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	if strings.ContainsAny(last, "=:") {
		return ""
	}
	return strings.Trim(last, ` "'{},`)
}

func replaceCurrentArgPrefix(input, current, replacement string) string {
	if current == "" {
		if strings.TrimSpace(input) == "" {
			return replacement
		}
		return strings.TrimRight(input, " \t") + " " + replacement
	}
	idx := strings.LastIndex(input, current)
	if idx < 0 {
		return strings.TrimRight(input, " \t") + " " + replacement
	}
	return input[:idx] + replacement
}

func toolArgNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	required := map[string]bool{}
	for _, name := range schema.Required {
		required[name] = true
	}
	names := make([]string, 0, len(schema.Properties))
	for name := range schema.Properties {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if required[names[i]] != required[names[j]] {
			return required[names[i]]
		}
		return names[i] < names[j]
	})
	return names
}

func providerNameSuggestions() []string {
	return []string{
		"anthropic", "deepseek", "openai", "openai-compatible", "glm", "kimi", "qwen", "doubao",
		"openrouter", "siliconflow", "groq", "together", "fireworks", "xai", "mistral",
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
		s = string(runes[:maxLen-1]) + "…"
	}
	return s
}

func showQuickCommands(commands []cmdEntry) {
	fmt.Println("\n可用命令:")
	for _, c := range commands {
		if c.Type == "cmd" || c.Type == "config" || c.Type == "builtin" {
			fmt.Printf("  %-16s %s\n", c.Name, c.Desc)
		}
	}
	fmt.Println()
}

func handleUnknownCmd(input string, cmdReg *command.Registry) bool {
	parts := strings.Fields(input)
	name := strings.TrimPrefix(parts[0], "/")
	_, ok := cmdReg.Find(name)
	if ok {
		return false
	}
	suggestions := fuzzyMatch(name, cmdReg)
	if len(suggestions) > 0 {
		fmt.Printf("未知命令: /%s\n你是不是想输入?\n", name)
		for _, s := range suggestions {
			fmt.Printf("  /%s\n", s)
		}
		return true
	}
	fmt.Printf("未知命令: /%s。输入 /help 查看可用命令。\n", name)
	return true
}

func fuzzyMatch(input string, cmdReg *command.Registry) []string {
	var matches []string
	lower := strings.ToLower(input)
	for _, c := range cmdReg.All() {
		name := c.Name()
		if strings.Contains(strings.ToLower(name), lower) {
			matches = append(matches, name)
		}
	}
	if len(matches) > 5 {
		return matches[:5]
	}
	return matches
}

func runPrintMode(eng *engine.Engine, prompt string, debug bool, attachmentPaths []string, model string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() { <-sigCh; cancel() }()
	cwd, _ := os.Getwd()
	userMsg, warnings, err := buildUserMessage(prompt, cwd, attachmentPaths, model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
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

func (a replEngineAdapter) Messages() []api.Message              { return a.eng.Messages() }
func (a replEngineAdapter) LoadMessages(msgs []api.Message)      { a.eng.LoadMessages(msgs) }
func (a replEngineAdapter) SetSystemOverride(prompt string)      { a.eng.SetSystemOverride(prompt) }
func (a replEngineAdapter) SystemPrompt() string                 { return a.eng.SystemPrompt() }
func (a replEngineAdapter) CostTracker() command.CostTrackerView { return a.eng.CostTracker() }

func handleCommand(ctx context.Context, input string, reg *command.Registry, cfg *config.Config, eng *engine.Engine, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, permMgr *permission.Manager, projCtx *ctxt.ProjectContext, appState *state.AppState) {
	parts := strings.Fields(input)
	name := strings.TrimPrefix(parts[0], "/")
	c, ok := reg.Find(name)
	if !ok {
		fmt.Printf("未知命令: /%s。输入 /help 查看帮助\n", name)
		return
	}
	cwd, _ := os.Getwd()
	out, err := c.Execute(ctx, command.Input{
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
		BuddyDisplay:      eng.BuddyDisplay,
		BuddyChat:         eng.BuddyChat,
		Provider:          eng.Provider(),
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
		repl.PrintSafe("\n%d 个技能:\n", len(prompts))
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
			repl.PrintSafe("技能市场不可用: %v\n", err)
			return
		}
		repl.PrintSafe("\n技能市场 (%d 个可用):\n", len(entries))
		for _, e := range entries {
			installed := ""
			if _, ok := prompts[e.Name]; ok {
				installed = " [已安装]"
			}
			repl.PrintSafe("  %-16s %s%s\n", e.Name, truncateDesc(e.Description, 48), installed)
		}
		repl.PrintSafe("\n安装: /skill install <名称>\n")

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
				repl.PrintSafe("已安装: %s — 重启后生效，或直接输入 /skill %s\n", name, name)
				return
			}
		}
		skills.InstallSkill(name, "local", "")
		repl.PrintSafe("已创建: %s\n编辑 ~/.agentgo/skills/%s/SKILL.md\n", name, name)

	case "create":
		if len(parts) < 3 {
			repl.PrintSafe("用法: /skill create <名称>\n")
			return
		}
		name := parts[2]
		skills.InstallSkill(name, "local", "")
		repl.PrintSafe("已创建: %s — 编辑 ~/.agentgo/skills/%s/SKILL.md\n", name, name)

	default:
		name := parts[1]
		prompt, ok := prompts[name]
		if !ok {
			repl.PrintSafe("\n未知技能: %s\n输入 /skill marketplace 发现更多技能。\n", name)
			return
		}
		repl.PrintSafe("\n[技能: %s]\n\n%s\n\n请按照以上指引执行。\n", name, prompt)
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
		repl.PrintSafe("未知技能: %s\n", name)
		return
	}
	args := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))
	repl.PrintSafe("\n[技能: %s]\n\n%s\n", name, prompt)
	if args != "" {
		repl.PrintSafe("\n用户请求:\n%s\n", args)
	}
	repl.PrintSafe("\n请按照以上指引执行。\n")
}

func handleExport(input string, eng *engine.Engine) {
	filename := "conversation.md"
	parts := strings.Fields(input)
	if len(parts) > 1 {
		filename = parts[1]
	}
	var sb strings.Builder
	sb.WriteString("# 对话导出\r\n\r\n")
	for _, m := range eng.Messages() {
		sb.WriteString(fmt.Sprintf("**%s**: %s\r\n\r\n", m.Role, m.Content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  > 工具: %s(%v)\r\n\r\n", tc.Name, tc.Input))
		}
	}
	os.WriteFile(filename, []byte(sb.String()), 0644)
	repl.PrintSafe("已导出 %d 条消息到 %s\n", len(eng.Messages()), filename)
}

func handleResume(ctx context.Context, sessionID string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}
	if sessionID == "" {
		records, _ := store.List()
		if len(records) == 0 {
			repl.PrintSafe("没有已保存的会话\n")
			return
		}
		repl.PrintSafe("%d 个已保存的会话:\n", len(records))
		for _, r := range records {
			repl.PrintSafe("  %s  %s  (%d tokens)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("15:04"))
		}
		return
	}
	r, err := store.Load(sessionID)
	if err != nil {
		repl.PrintSafe("会话 %s 未找到\n", sessionID)
		return
	}
	eng.LoadMessages(r.Messages)
	repl.PrintSafe("已恢复: %s (%d 条消息, %d tokens)\n", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)
}

func autoSaveSession(eng *engine.Engine) {
	if eng.HasMessages() {
		eng.SaveSession()
		// Persist cost history
		ch := cost.NewCostHistory()
		sessionID := ""
		model := ""
		if s := eng.Session(); s != nil {
			sessionID = s.ID
			model = s.Model
		}
		ch.Add(sessionID, model, eng.CostTracker())
		ch.Save()
		fmt.Println("会话已自动保存。")
	}
	// Save buddy chat history
	if eng.BuddyChat != nil {
		_ = eng.BuddyChat.SaveHistory()
	}
}

func handleHistory(eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}
	records, _ := store.List()
	if len(records) == 0 {
		repl.PrintSafe("暂无历史。退出时会自动保存会话。\n")
		return
	}
	repl.PrintSafe("\n  历史记录 (%d 个会话):\n\n", len(records))
	limit := 20
	if len(records) < limit {
		limit = len(records)
	}
	for i, r := range records[:limit] {
		msgCount := len(r.Messages)
		date := r.UpdatedAt.Format("01-02 15:04")
		title := r.Title
		if title == "New session" || title == "" {
			title = sessionPreview(r)
		}
		if len(title) > 50 {
			title = title[:50] + "..."
		}
		repl.PrintSafe("  %2d. [%s] %s  (%d 条)\n", i+1, date, title, msgCount)
	}
	if len(records) > limit {
		repl.PrintSafe("\n  ... 还有 %d 条。\n", len(records)-limit)
	}
	repl.PrintSafe("\n  继续会话: /history <编号>  (例如 /history 1)\n\n")
}

func sessionPreview(r session.Record) string {
	for _, m := range r.Messages {
		if m.Role == "user" && m.Content != "" {
			content := strings.ReplaceAll(m.Content, "\n", " ")
			if len(content) > 50 {
				content = content[:50] + "..."
			}
			return content
		}
	}
	return "(空)"
}

func handleHistoryResume(input string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}

	// Support number-based selection (e.g. "1", "2") or direct session ID
	records, _ := store.List()
	var idx int
	if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx >= 1 && idx <= len(records) {
		r := records[idx-1]
		eng.LoadMessages(r.Messages)
		title := r.Title
		if title == "New session" || title == "" {
			title = sessionPreview(r)
		}
		repl.PrintSafe("已恢复 #%d: %s (%d 条消息)\n", idx, title, len(r.Messages))
		repl.PrintSafe("可以继续对话了。\n\n")
		return
	}

	// Fallback: try loading by session ID
	r, err := store.Load(input)
	if err != nil {
		repl.PrintSafe("无效选择: %s\n输入 /history 查看可用会话。\n", input)
		return
	}
	eng.LoadMessages(r.Messages)
	title := r.Title
	if title == "New session" || title == "" {
		title = sessionPreview(*r)
	}
	repl.PrintSafe("已恢复: %s (%d 条消息)\n", title, len(r.Messages))
	repl.PrintSafe("可以继续对话了。\n\n")
}

func showConfig() {
	cfg, _ := config.Load()
	pc := cfg.EffectiveProvider()
	data, _ := json.MarshalIndent(map[string]any{
		"version":         Version,
		"model":           cfg.Model,
		"provider":        pc.Name,
		"base_url":        pc.BaseURL,
		"permission_mode": cfg.PermissionMode,
		"max_budget_usd":  cfg.MaxBudgetUsd,
		"thinking_tokens": cfg.ThinkingTokens,
		"debug":           cfg.Debug,
		"api_key_set":     pc.APIKey != "",
		"mcp_servers":     len(cfg.MCPServers),
	}, "", "  ")
	fmt.Println(string(data))
}

func providerHelpLine() string {
	return "  /provider <名称>    设置供应商 (anthropic, deepseek, openai, openai-compatible, glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral)"
}

func providerEnvHelpLine() string {
	return "环境变量: LLM_API_KEY | ANTHROPIC_API_KEY | DEEPSEEK_API_KEY | OPENAI_API_KEY | GLM_API_KEY | KIMI_API_KEY | QWEN_API_KEY | OPENROUTER_API_KEY | SILICONFLOW_API_KEY | LLM_BASE_URL"
}

func printHelp(cmdReg *command.Registry, toolReg *tool.Registry) {
	fmt.Println("\n=== agentgo v" + Version + " ===")
	fmt.Println("\n供应商 / 模型:")
	fmt.Println("  /model <名称>       设置模型")
	fmt.Println(providerHelpLine())
	fmt.Println("  /api-key <密钥>     保存 API 密钥")
	fmt.Println("  /base-url <地址>    设置自定义接口地址")
	fmt.Println("  /mode <模式>        设置权限模式 (default|plan|auto|bypass)")
	fmt.Println("  /budget <金额>      设置每会话预算上限 ($)")
	fmt.Println("  /cost               查看用量和费用")
	fmt.Println("  /ratelimit          查看 API 速率限制状态")
	fmt.Println("  /attach <文件...>   挂载图片或文件；list/remove/clear 管理列表")
	fmt.Println("  /config             查看完整配置")
	fmt.Println("\n会话:")
	fmt.Println("  /compact            压缩对话历史")
	fmt.Println("  /undo               回退到上一个检查点")
	fmt.Println("  /checkpoints        列出所有检查点")
	fmt.Println("  /history            查看和继续历史会话")
	fmt.Println("  /resume [id]        恢复已保存的会话")
	fmt.Println("  /memory             管理持久化记忆")
	fmt.Println("\n系统:")
	fmt.Println("  /mcp                管理 MCP 服务器")
	fmt.Println("  /plugin             管理插件")
	fmt.Println("  /skills             列出技能")
	fmt.Println("\n命令:")
	for _, c := range cmdReg.All() {
		fmt.Printf("  /%-16s %s\n", c.Name(), c.Description())
	}
	fmt.Println("\n工具:")
	for _, t := range toolReg.All() {
		d := t.Def()
		ro := " "
		if d.IsReadOnly {
			ro = "R"
		}
		fmt.Printf("  [%s] %-12s %s\n", ro, d.Name, truncateDesc(d.Description, 48))
	}
	fmt.Println("\n" + providerEnvHelpLine())
	fmt.Println("启动参数: -p <提示> [--image <路径>] [--file <路径>] | -d --debug | -v --version | --doctor | --config")
	fmt.Println("附件输入: 在 REPL 或 -p 文本中可写 @路径，例如：解释这张图 @assets/screen.png")
	fmt.Println()
}

func missingAPIKeyMessage(provider string) string {
	provider = api.NormalizeProviderName(strings.TrimSpace(provider))
	if provider == "" {
		provider = "anthropic"
	}
	providerEnvCandidates := api.ProviderEnvCandidates(provider)
	primaryEnv := "LLM_API_KEY"
	if len(providerEnvCandidates) > 0 {
		primaryEnv = providerEnvCandidates[0]
	}
	openAICompatList := "glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral"
	return fmt.Sprintf(
		"No API key configured / 未配置 API key.\n"+
			"先看当前厂商：%s\n\n"+
			"最快的办法：直接在当前 REPL 输入\n"+
			"  /api-key <你的key>\n\n"+
			"如果你用 Claude / Anthropic：设置 %s\n"+
			"如果你用 DeepSeek：设置 DEEPSEEK_API_KEY\n"+
			"如果你用 OpenAI：设置 OPENAI_API_KEY\n\n"+
			"如果你用 GLM / Kimi / Qwen / 豆包 / OpenRouter / 硅基流动 / Groq / Together / Fireworks / xAI / Mistral 这类兼容 OpenAI 的接口：\n"+
			"  1) /provider openai-compatible  （或直接 /provider 对应厂商名）\n"+
			"  2) /base-url <兼容 OpenAI 的接口地址>\n"+
			"  3) /api-key <你的key>\n\n"+
			"例如 GLM：        /provider glm          + GLM_API_KEY / ZHIPU_API_KEY\n"+
			"例如 Kimi：       /provider kimi         + KIMI_API_KEY / MOONSHOT_API_KEY\n"+
			"例如 Qwen：       /provider qwen         + QWEN_API_KEY / DASHSCOPE_API_KEY\n"+
			"例如 豆包：       /provider doubao       + DOUBAO_API_KEY / ARK_API_KEY\n"+
			"例如 OpenRouter： /provider openrouter   + OPENROUTER_API_KEY\n"+
			"例如 硅基流动：   /provider siliconflow + SILICONFLOW_API_KEY\n\n"+
			"当前内置适配 provider：anthropic, deepseek, openai, openai-compatible, %s\n"+
			"也可用通用变量：LLM_API_KEY\n"+
			"设置后执行 /config，确认 api_key_set: true。",
		provider,
		primaryEnv,
		openAICompatList,
	)
}

// buddyFloatingBox renders the buddy sprite with speech bubble as a visible box.
func buddyFloatingBox(d *buddy.Display) string {
	rendered := d.RenderWithBubble()
	// Add color: cyan for the whole block
	lines := strings.Split(rendered, "\n")
	var sb strings.Builder
	sb.WriteString("  \x1b[36m") // cyan
	for i, line := range lines {
		if line == "" && i == len(lines)-1 {
			continue
		}
		sb.WriteString("  ")
		sb.WriteString(line)
		sb.WriteString("\n\x1b[36m")
	}
	// Mood indicator line
	m := d.Mood.Current()
	sb.WriteString(fmt.Sprintf("  \x1b[2m%s %s  Lv.%d\x1b[0m",
		buddy.MoodEmoji(m), buddy.MoodLabel(m), d.XP.Level))
	sb.WriteString("\x1b[0m")
	return sb.String()
}

func runChatInteraction(ctx context.Context, runner chatRunner, input string) string {
	return runChatInteractionMessage(ctx, runner, api.Message{Role: "user", Content: input})
}

func runChatInteractionMessage(ctx context.Context, runner chatRunner, userMsg api.Message) string {
	fmt.Print("\n")

	spinner := repl.NewSpinner("思考中...")
	spinner.Start()
	firstDelta := true
	var totalOutput strings.Builder

	// If runner is a full engine, wire up permission pause to stop spinner
	if eng, ok := runner.(*engine.Engine); ok {
		eng.OnPermissionPause = func() {
			spinner.Stop()
		}
		eng.OnPermissionDone = nil
		defer func() { eng.OnPermissionPause = nil }()
	}

	var err error
	if richRunner, ok := runner.(interface {
		RunMessageWithStream(context.Context, api.Message, func(string)) (string, error)
	}); ok {
		_, err = richRunner.RunMessageWithStream(ctx, userMsg, func(delta string) {
			if firstDelta {
				spinner.Stop()
				firstDelta = false
			}
			fmt.Print(delta)
			totalOutput.WriteString(delta)
		})
	} else {
		if len(userMsg.Parts) > 0 {
			err = fmt.Errorf("当前运行器不支持附件消息")
		} else {
			_, err = runner.RunWithStream(ctx, userMsg.Content, func(delta string) {
				if firstDelta {
					spinner.Stop()
					firstDelta = false
				}
				fmt.Print(delta)
				totalOutput.WriteString(delta)
			})
		}
	}
	spinner.Stop()

	if err != nil {
		errMsg := fmt.Sprintf("\nRequest failed: %s", err.Error())
		fmt.Printf("%s%s%s", repl.Red, errMsg, repl.Reset)
		totalOutput.WriteString(errMsg)
	}
	fmt.Print("\r\n\r\n")
	totalOutput.WriteString("\r\n\r\n")
	return totalOutput.String()
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
		fmt.Println("没有已保存的会话")
		return
	}
	fmt.Printf("%d 个会话:\n", len(records))
	for _, r := range records {
		fmt.Printf("  %s  %s  (%dt)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("2006-01-02 15:04"))
	}
}

func printCLIHelp() {
	fmt.Println(`agentgo — Go 驱动的 AI 编程助手

用法:
 agentgo                       启动交互式 REPL
 agentgo -p <提示>              执行单次查询后退出
 agentgo -p <提示> --image <路径>  单次查询并附带图片
 agentgo -p <提示> --file <路径>   单次查询并附带文件
 agentgo -r <id>                恢复已保存的会话
 agentgo --list-sessions        列出已保存的会话
 agentgo -d                     启用调试日志
 agentgo --doctor               运行系统诊断
 agentgo --config               显示配置
 agentgo -v, --version          显示版本
 agentgo -h, --help             显示帮助

技能命令:
 /skill [名称]                  激活技能
 /skill marketplace             浏览技能市场
 /skill install <名称>          从市场安装
 /skill create <名称>           创建新技能

REPL 命令:
 /model, /provider, /api-key, /base-url, /mode, /budget
 /cost, /config, /system, /context, /compact
 /attach <文件...>, /attach list, /attach remove <序号>, /attach clear
 /commit, /review, /diff, /export
 /resume, /memory, /mcp, /plugin
 /doctor, /status, /stats, /permissions
 /cd, /help, /exit`)
	fmt.Println("\n提示中可直接使用 @文件路径 附加文件，如: 分析这个日志 @logs/app.log")
}

func truncateDesc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
