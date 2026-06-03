package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agentgo/internal/api"
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

	mcpPool := mcp.NewPool()

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
		repl.PrintAbove(repl.PermissionPrompt(toolName, desc))
		repl.PrintAbove(fmt.Sprintf("  %s允许？ (y)确认 / (n)拒绝 / (a)始终允许:%s", repl.Yellow, repl.Reset))

		readAnswer := func() string {
			line, err := bufio.NewReader(os.Stdin).ReadString('\n')
			if err != nil {
				return ""
			}
			return strings.TrimSpace(strings.ToLower(line))
		}

		answer := readAnswer()
		if answer == "" {
			repl.PrintAbove(fmt.Sprintf("\n  %s检测到空输入，请再次输入 (y/n/a):%s", repl.Yellow, repl.Reset))
			answer = readAnswer()
		}
		if answer == "" {
			repl.PrintAbove(fmt.Sprintf("  %s(未输入，默认拒绝)%s\n", repl.Dim, repl.Reset))
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
	_ = mcpPool
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
	r.Register(command.NewExportCmd())
	r.Register(command.NewSystemCmd())
	r.Register(command.NewCdCmd())
	r.Register(command.NewContextCmd())
	r.Register(command.NewPermissionsCmd())
	r.Register(command.NewStatusCmd())
	r.Register(command.NewStatsCmd())
	r.Register(command.NewInitCmd())
	r.Register(command.NewDiagnoseCmd())
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
	historyPickPending := false
	var pendingFailedMsg *api.Message
	var taskMu sync.Mutex
	taskRunning := false
	var taskCancel context.CancelFunc
	taskQueue := make([]api.Message, 0)

	var startNextTaskLocked func()
	startNextTaskLocked = func() {
		if taskRunning || len(taskQueue) == 0 {
			return
		}
		msg := taskQueue[0]
		taskQueue = taskQueue[1:]
		taskRunning = true
		ctx, cancel := context.WithCancel(context.Background())
		taskCancel = cancel

		go func(userMsg api.Message) {
			// Guard against panics inside the task: without this, a panic would
			// leave taskRunning=true forever and silently freeze the whole queue.
			defer func() {
				if r := recover(); r != nil {
					// Inner defer taskMu.Unlock() (if it had locked) already ran,
					// so re-locking here is safe.
					taskMu.Lock()
					msgCopy := userMsg
					pendingFailedMsg = &msgCopy
					_ = saveInterruptedDraft(userMsg, fmt.Errorf("internal panic: %v", r))
					taskRunning = false
					taskCancel = nil
					repl.PrintAbove(fmt.Sprintf("\r\n%s任务执行出现内部异常，已恢复输入。可输入“继续”重试。%s\r\n", repl.Red, repl.Reset))
					startNextTaskLocked()
					taskMu.Unlock()
				}
			}()
			_, reqErr := runChatInteractionMessage(ctx, eng, userMsg)

			taskMu.Lock()
			defer taskMu.Unlock()

			if reqErr != nil {
				msgCopy := userMsg
				pendingFailedMsg = &msgCopy
				if isBudgetExceededError(reqErr) {
					repl.PrintAbove(budgetExceededRetryHint(eng.CostTracker()) + "\n")
				} else {
					_ = saveInterruptedDraft(userMsg, reqErr)
					repl.PrintAbove("可输入“继续”重试刚才中断的任务。\n")
				}
			} else {
				pendingFailedMsg = nil
				_ = clearInterruptedDraft()
			}

			taskRunning = false
			taskCancel = nil
			startNextTaskLocked()
		}(msg)
	}

	isTaskRunning := func() bool {
		taskMu.Lock()
		defer taskMu.Unlock()
		return taskRunning
	}

	waitForTaskIdle := func() {
		for {
			taskMu.Lock()
			running := taskRunning
			taskMu.Unlock()
			if !running {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	enqueueTask := func(msg api.Message) (int, bool) {
		taskMu.Lock()
		defer taskMu.Unlock()

		if taskRunning && len(taskQueue) > 0 {
			for i := len(taskQueue) - 1; i >= 0; i-- {
				if canMergeQueuedTask(taskQueue[i], msg) {
					taskQueue[i] = mergeQueuedTask(taskQueue[i], msg)
					return i, true
				}
			}
		}

		taskQueue = append(taskQueue, msg)
		queueSize := len(taskQueue)
		startNextTaskLocked()
		if taskRunning {
			if queueSize > 0 {
				return queueSize - 1, false
			}
			return 0, false
		}
		return 0, false
	}

	// On startup, check for interrupted draft and notify user
	if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {
		title := draft.Title
		if title == "" {
			title = shortDesc(draft.UserContent)
		}
		age := time.Since(draft.UpdatedAt).Truncate(time.Second)
		fmt.Printf("\n  \x1b[33m⚡ 检测到中断任务: %s (%s)\x1b[0m\n", title, age)
		fmt.Printf("  \x1b[2m输入「继续」恢复该任务，或直接输入新内容忽略。\x1b[0m\n\n")
	}

	for {
		input, err := reader.ReadLine()
		if err == repl.ErrInterrupt {
			if isTaskRunning() {
				taskMu.Lock()
				cancel := taskCancel
				taskMu.Unlock()
				if cancel != nil {
					cancel()
					fmt.Println("已请求中断当前任务。")
				}
				continue
			}
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
			taskMu.Lock()
			cancel := taskCancel
			running := taskRunning
			taskMu.Unlock()
			if cancel != nil {
				cancel()
			}
			// Wait briefly for the task goroutine to finish its cleanup (save draft)
			if running {
				deadline := time.After(3 * time.Second)
				for {
					time.Sleep(50 * time.Millisecond)
					taskMu.Lock()
					done := !taskRunning
					taskMu.Unlock()
					if done {
						break
					}
					select {
					case <-deadline:
						goto exitNow
					default:
					}
				}
			}
		exitNow:
			autoSaveSession(eng)
			fmt.Println("再见！")
			return
		case isContinueCommand(input):
			if isTaskRunning() {
				fmt.Println("当前已有任务在进行，继续输入内容会排队到当前任务后执行。")
				historyPickPending = false
				continue
			}
			if eng.CostTracker() != nil && eng.CostTracker().OverBudget() {
				fmt.Println(budgetExceededRetryHint(eng.CostTracker()))
				historyPickPending = false
				continue
			}
			taskMu.Lock()
			pf := pendingFailedMsg
			taskMu.Unlock()

			if pf != nil {
				if isLowSignalResumeInput(pf.Content) {
					taskMu.Lock()
					pendingFailedMsg = nil
					taskMu.Unlock()
					_ = clearInterruptedDraft()
					fmt.Println("检测到中断草稿内容过短，已跳过并恢复最近有效任务。")
					handleHistoryResumeMostRelevant(eng)
					historyPickPending = false
					continue
				}
				taskMu.Lock()
				pendingFailedMsg = nil
				taskMu.Unlock()
				_, merged := enqueueTask(*pf)
				if merged {
					fmt.Println("检测到相似重试任务，已合并到队列中。")
				} else {
					fmt.Println("已加入重试队列，任务完成后会自动继续。")
				}
				waitForTaskIdle()
				historyPickPending = false
				continue
			}
			if draft, _ := loadInterruptedDraft(); draft != nil && strings.TrimSpace(draft.UserContent) != "" {
				if isLowSignalResumeInput(draft.UserContent) {
					_ = clearInterruptedDraft()
					fmt.Println("检测到中断草稿内容过短，已跳过并恢复最近有效任务。")
					handleHistoryResumeMostRelevant(eng)
					historyPickPending = false
					continue
				}
				msg := api.Message{Role: "user", Content: draft.UserContent}
				_, merged := enqueueTask(msg)
				if merged {
					fmt.Println("检测到相似重试任务，已合并到队列中。")
				} else {
					fmt.Println("已加入重试队列，任务完成后会自动继续。")
				}
				waitForTaskIdle()
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
			arg := strings.TrimSpace(strings.TrimPrefix(input, "/budget "))
			if strings.EqualFold(arg, "auto") {
				b := cfg.MaxBudgetUsd
				if tr := eng.CostTracker(); tr != nil {
					suggested := tr.SuggestedBudget()
					if suggested > b {
						b = suggested
					}
				}
				if b > 0 {
					cfg.MaxBudgetUsd = b
					as.MaxBudget = b
					eng.SetMaxBudget(b)
					config.Save(cfg)
					fmt.Printf("预算已自动调整到: $%.2f\n", b)
				}
				continue
			}
			var b float64
			fmt.Sscanf(arg, "%f", &b)
			if b > 0 {
				cfg.MaxBudgetUsd = b
				as.MaxBudget = b
				eng.SetMaxBudget(b)
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
		case strings.HasPrefix(input, "/resume") || input == "/resume":
			sessionID := ""
			if strings.HasPrefix(input, "/resume ") {
				sessionID = strings.TrimPrefix(input, "/resume ")
			}
			withInterrupt(func(ctx context.Context) { handleResume(ctx, sessionID, eng) })
		case input == "/history":
			handleHistory(eng)
			historyPickPending = true
		case strings.HasPrefix(input, "/history "):
			histID := strings.TrimSpace(strings.TrimPrefix(input, "/history "))
			if strings.HasPrefix(strings.ToLower(histID), "detail ") {
				handleHistoryDetail(strings.TrimSpace(histID[len("detail "):]), eng)
				historyPickPending = false
				continue
			}
			handleHistoryResume(histID, eng)
			historyPickPending = false
		case input == "/compact":
			withInterrupt(func(ctx context.Context) {
				eng.Compact(ctx)
				fmt.Println("上下文窗口已压缩。")
			})
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
				fmt.Printf("附件处理失败: %v\n", err)
				continue
			}
			if shouldAutoSwitchToVision(warnings) {
				if visionModel := preferredVisionModelForProvider(pc.Name, cfg.Model); visionModel != "" && visionModel != cfg.Model {
					if err := applyProviderConfigChange(cfg, eng, func() error {
						cfg.Model = visionModel
						as.Model = visionModel
						return nil
					}); err == nil {
						fmt.Printf("ℹ 检测到图片输入，已自动切换视觉模型: %s\n", visionModel)
						userMsg, warnings, err = buildUserMessage(input, cwd, attachedFiles, cfg.Model)
						if err != nil {
							fmt.Printf("附件处理失败: %v\n", err)
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
			queuedAhead, merged := enqueueTask(userMsg)
			if merged {
				if queuedAhead > 0 {
					fmt.Printf("检测到相似任务，已合并到队列中（前方还有 %d 条）。\n", queuedAhead)
				} else {
					fmt.Println("检测到相似任务，已合并到队列中。")
				}
			} else if queuedAhead > 0 {
				fmt.Printf("已加入进行中任务队列，前方还有 %d 条。\n", queuedAhead)
			} else {
				fmt.Println("任务已开始执行。")
			}
			waitForTaskIdle()
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

func shouldAutoSwitchToVision(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, "已自动降级为文本提示") || strings.Contains(w, "不支持图片视觉") {
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
		s = string(runes[:maxLen-1]) + "…"
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

func isTransientRequestError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	transientHints := []string{
		"timeout", "timed out", "awaiting response headers", "deadline exceeded",
		"connection reset", "broken pipe", "connection refused", "eof",
		"temporary", "temporarily unavailable", "server error 5", "bad gateway",
	}
	for _, h := range transientHints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
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
				fmt.Fprintf(os.Stderr, "ℹ 检测到图片输入，已自动切换视觉模型: %s\n", visionModel)
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
	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		repl.PrintSafe("导出失败: %v\n", err)
		return
	}
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
}

type interruptedDraft struct {
	UpdatedAt   time.Time `json:"updated_at"`
	Title       string    `json:"title"`
	UserContent string    `json:"user_content"`
	Error       string    `json:"error"`
}

func interruptedDraftPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agentgo", "interrupted.json"), nil
}

// writeFileAtomic writes data to a temp file in the same directory and renames
// it into place, so a crash or power loss can never leave a half-written
// (corrupt, unrecoverable) file at path.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename already moved the file
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func saveInterruptedDraft(msg api.Message, reqErr error) error {
	p, err := interruptedDraftPath()
	if err != nil {
		return err
	}
	d := interruptedDraft{
		UpdatedAt:   time.Now(),
		Title:       shortDesc(msg.Content),
		UserContent: strings.TrimSpace(msg.Content),
	}
	if d.Title == "" {
		d.Title = "(未命名中断任务)"
	}
	if reqErr != nil {
		d.Error = reqErr.Error()
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, raw, 0600)
}

func loadInterruptedDraft() (*interruptedDraft, error) {
	p, err := interruptedDraftPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var d interruptedDraft
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.UserContent) == "" {
		return nil, fmt.Errorf("empty draft")
	}
	return &d, nil
}

func clearInterruptedDraft() error {
	p, err := interruptedDraftPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p); err != nil {
		return nil
	}
	return os.Remove(p)
}

func handleHistory(eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}
	records, _ := store.List()
	draft, _ := loadInterruptedDraft()
	if len(records) == 0 && draft == nil {
		repl.PrintSafe("暂无历史。退出时会自动保存会话。\n")
		return
	}
	repl.PrintSafe("\n  历史记录 (%d 个会话):\n\n", len(records))
	if draft != nil {
		repl.PrintSafe("  ⚠ 中断草稿 [%s] %s\n", draft.UpdatedAt.Format("01-02 15:04"), shortDesc(draft.Title))
	}
	limit := 20
	if len(records) < limit {
		limit = len(records)
	}
	for i, r := range records[:limit] {
		msgCount := r.MessageCount
		if msgCount == 0 && len(r.Messages) > 0 {
			msgCount = len(r.Messages)
		}
		date := r.UpdatedAt.Format("01-02 15:04")
		title := r.Title
		if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
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
	repl.PrintSafe("\n  继续会话: /history <编号>  (例如 /history 1)\n")
	repl.PrintSafe("  或直接输入编号: 1 / 2 / 3 ...\n")
	repl.PrintSafe("  查看详情: /history detail <编号>\n\n")
	if draft != nil {
		repl.PrintSafe("  中断详情: /history detail interrupted\n\n")
	}
}

func sessionPreview(r session.Record) string {
	if r.Preview != "" {
		return r.Preview
	}
	fallback := ""
	for _, m := range r.Messages {
		if m.Role == "user" && m.Content != "" {
			content := strings.ReplaceAll(m.Content, "\n", " ")
			if len(content) > 50 {
				content = content[:50] + "..."
			}
			if !isLowSignalHistoryTitle(content) {
				return content
			}
			if fallback == "" {
				fallback = content
			}
		}
	}
	if fallback != "" {
		return fallback
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
		rMeta := records[idx-1]
		r, err := store.Load(rMeta.ID)
		if err != nil {
			repl.PrintSafe("恢复失败: %v\n", err)
			return
		}
		eng.LoadMessages(r.Messages)
		title := r.Title
		if title == "New session" || title == "" {
			title = sessionPreview(*r)
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
	if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*r)
	}
	repl.PrintSafe("已恢复: %s (%d 条消息)\n", title, len(r.Messages))
	repl.PrintSafe("可以继续对话了。\n\n")
}

func handleHistoryDetail(input string, eng *engine.Engine) {
	if strings.TrimSpace(input) == "" {
		repl.PrintSafe("用法: /history detail <编号|session-id>\n")
		return
	}
	if strings.EqualFold(strings.TrimSpace(input), "interrupted") {
		draft, _ := loadInterruptedDraft()
		if draft == nil {
			repl.PrintSafe("当前没有中断草稿。\n")
			return
		}
		repl.PrintSafe("\n  中断草稿详情\n")
		repl.PrintSafe("  更新时间: %s\n", draft.UpdatedAt.Format("2006-01-02 15:04:05"))
		repl.PrintSafe("  标题: %s\n", draft.Title)
		repl.PrintSafe("  错误: %s\n\n", shortDesc(draft.Error))
		repl.PrintSafe("  用户输入:\n")
		repl.PrintSafe("  %s\n\n", draft.UserContent)
		return
	}
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}

	resolve := func(sel string) (*session.Record, error) {
		records, _ := store.List()
		var idx int
		if _, err := fmt.Sscanf(sel, "%d", &idx); err == nil && idx >= 1 && idx <= len(records) {
			return store.Load(records[idx-1].ID)
		}
		return store.Load(sel)
	}

	r, err := resolve(strings.TrimSpace(input))
	if err != nil {
		repl.PrintSafe("无效选择: %s\n输入 /history 查看可用会话。\n", input)
		return
	}

	title := r.Title
	if title == "" || title == "New session" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*r)
	}

	repl.PrintSafe("\n  会话详情\n")
	repl.PrintSafe("  ID: %s\n", r.ID)
	repl.PrintSafe("  标题: %s\n", title)
	repl.PrintSafe("  更新时间: %s\n", r.UpdatedAt.Format("2006-01-02 15:04:05"))
	repl.PrintSafe("  消息数: %d\n\n", len(r.Messages))

	if len(r.Messages) == 0 {
		repl.PrintSafe("  该会话暂无消息。\n\n")
		return
	}

	const window = 6
	total := len(r.Messages)
	indices := make([]int, 0, window)
	if total <= window {
		for i := 0; i < total; i++ {
			indices = append(indices, i)
		}
	} else {
		indices = append(indices, 0, 1, 2, total-3, total-2, total-1)
	}

	repl.PrintSafe("  消息预览:\n")
	for i, idx := range indices {
		if total > window && i == 3 {
			repl.PrintSafe("    ...\n")
		}
		m := r.Messages[idx]
		role := strings.ToUpper(strings.TrimSpace(m.Role))
		if role == "" {
			role = "UNKNOWN"
		}
		content := m.Content
		if strings.TrimSpace(content) == "" && len(m.Parts) > 0 {
			content = fmt.Sprintf("[%d part(s)]", len(m.Parts))
		}
		if strings.TrimSpace(content) == "" {
			content = "(空)"
		}
		repl.PrintSafe("  [%03d] %-9s %s\n", idx+1, role, shortDesc(content))
	}
	repl.PrintSafe("\n")
}

func handleHistoryResumeMostRelevant(eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("会话存储不可用\n")
		return
	}
	records, _ := store.List()
	if len(records) == 0 {
		repl.PrintSafe("暂无历史。\n")
		return
	}

	type candidate struct {
		rec   *session.Record
		idx   int
		score int
	}

	best := candidate{score: -1}
	for i, meta := range records {
		rec, err := store.Load(meta.ID)
		if err != nil {
			continue
		}
		s := scoreSessionForResume(*rec)
		if s > best.score {
			best = candidate{rec: rec, idx: i + 1, score: s}
		}
		if i >= 30 {
			break
		}
	}

	if best.rec == nil {
		handleHistoryResume("1", eng)
		return
	}

	eng.LoadMessages(best.rec.Messages)
	title := best.rec.Title
	if title == "New session" || title == "" || isLowSignalHistoryTitle(title) {
		title = sessionPreview(*best.rec)
	}
	repl.PrintSafe("已自动恢复最近有效任务 #%d: %s (%d 条消息)\n", best.idx, title, len(best.rec.Messages))
	repl.PrintSafe("可以继续对话了。\n\n")
}

func scoreSessionForResume(r session.Record) int {
	if len(r.Messages) == 0 {
		return -100
	}
	score := 0
	if len(r.Messages) >= 6 {
		score += 4
	} else {
		score += len(r.Messages)
	}

	userText := ""
	toolCount := 0
	assistantCount := 0
	for _, m := range r.Messages {
		if userText == "" && m.Role == "user" {
			userText = strings.TrimSpace(m.Content)
		}
		if m.Role == "tool" {
			toolCount++
		}
		if m.Role == "assistant" {
			assistantCount++
		}
	}

	if toolCount > 0 {
		score += 4
	}
	if assistantCount > 1 {
		score += 2
	}

	if isTrivialResumePrompt(userText) {
		score -= 6
	} else {
		score += 3
		if strings.Contains(userText, "http") || strings.Contains(userText, "https") {
			score += 2
		}
		if len([]rune(userText)) >= 20 {
			score += 1
		}
	}

	return score
}

func isTrivialResumePrompt(s string) bool {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return true
	}
	trivial := map[string]bool{
		"继续": true, "continue": true,
		"hi": true, "hello": true, "你好": true,
		"你": true, "我": true, "嗯": true, "好的": true,
		"1": true, "2": true, "3": true, "4": true, "?": true,
	}
	if trivial[v] {
		return true
	}
	if strings.HasPrefix(v, "继续") {
		return true
	}
	if strings.HasPrefix(v, "/history") || strings.HasPrefix(v, "/resume") {
		return true
	}
	return false
}

func isLowSignalHistoryTitle(s string) bool {
	v := strings.TrimSpace(strings.ToLower(s))
	if v == "" {
		return true
	}
	if len([]rune(v)) <= 2 {
		return true
	}
	noise := map[string]bool{
		"write":        true,
		"write a file": true,
		"read":         true,
		"read file":    true,
		"grep":         true,
		"继续":           true,
		"continue":     true,
		"hi":           true,
		"hello":        true,
		"你好":           true,
		"?":            true,
	}
	if noise[v] {
		return true
	}
	if strings.HasPrefix(v, "/") {
		return true
	}
	return false
}

func isLowSignalResumeInput(s string) bool {
	v := strings.TrimSpace(s)
	if v == "" {
		return true
	}
	if len([]rune(v)) <= 1 {
		return true
	}
	return isTrivialResumePrompt(v)
}

func isContinueCommand(input string) bool {
	v := strings.TrimSpace(strings.ToLower(input))
	if v == "继续" || v == "continue" {
		return true
	}
	return strings.HasPrefix(v, "继续") || strings.HasPrefix(v, "continue ")
}

func canMergeQueuedTask(existing, incoming api.Message) bool {
	if existing.Role != "user" || incoming.Role != "user" {
		return false
	}
	if len(existing.Parts) > 0 || len(incoming.Parts) > 0 {
		return false
	}
	a := normalizeTaskForMerge(existing.Content)
	b := normalizeTaskForMerge(incoming.Content)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return true
	}
	if commonPrefixRunes(a, b) >= 24 {
		return true
	}
	return false
}

func mergeQueuedTask(existing, incoming api.Message) api.Message {
	merged := existing
	add := strings.TrimSpace(incoming.Content)
	if add == "" {
		return merged
	}
	base := strings.TrimSpace(existing.Content)
	if base == "" {
		merged.Content = add
		return merged
	}
	if strings.Contains(base, add) {
		return merged
	}
	merged.Content = base + "\n\n[补充要求]\n" + add
	return merged
}

func normalizeTaskForMerge(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"\n", " ", "\t", " ",
		"，", " ", "。", " ", "！", " ", "？", " ",
		",", " ", ".", " ", "!", " ", "?", " ",
		"：", " ", ":", " ", ";", " ", "；", " ",
	)
	v = replacer.Replace(v)
	return strings.Join(strings.Fields(v), " ")
}

func commonPrefixRunes(a, b string) int {
	ar := []rune(a)
	br := []rune(b)
	n := len(ar)
	if len(br) < n {
		n = len(br)
	}
	count := 0
	for i := 0; i < n; i++ {
		if ar[i] != br[i] {
			break
		}
		count++
	}
	return count
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
	fmt.Println("  /budget <金额|auto> 设置每会话预算上限 ($)，auto 为一键提升")
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

func runChatInteraction(ctx context.Context, runner chatRunner, input string) (string, error) {
	return runChatInteractionMessage(ctx, runner, api.Message{Role: "user", Content: input})
}

func runChatInteractionMessage(ctx context.Context, runner chatRunner, userMsg api.Message) (string, error) {
	repl.BeginOutput()
	defer repl.EndOutput()
	var totalOutput strings.Builder
	var finalErr error

	maxAttempts := 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		spinner := repl.NewSpinner("思考中...")
		spinner.Start()
		firstDelta := true
		gotDelta := false

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
				gotDelta = true
				repl.StreamPrint(delta)
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
					gotDelta = true
					repl.StreamPrint(delta)
					totalOutput.WriteString(delta)
				})
			}
		}
		spinner.Stop()

		if err == nil {
			finalErr = nil
			break
		}
		finalErr = err
		if gotDelta || attempt == maxAttempts || ctx.Err() != nil || !isTransientRequestError(err) {
			break
		}
		note := fmt.Sprintf("\n网络波动，自动重试中 (%d/%d)...\n", attempt, maxAttempts)
		repl.StreamPrint(fmt.Sprintf("%s%s%s", repl.Yellow, note, repl.Reset))
		totalOutput.WriteString(note)
		time.Sleep(time.Duration(attempt) * 1200 * time.Millisecond)
	}

	if finalErr != nil {
		errMsg := fmt.Sprintf("\nRequest failed: %s", finalErr.Error())
		repl.StreamPrint(fmt.Sprintf("%s%s%s", repl.Red, errMsg, repl.Reset))
		totalOutput.WriteString(errMsg)
	}
	totalOutput.WriteString("\r\n\r\n")
	return totalOutput.String(), finalErr
}

func isBudgetExceededError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "budget exceeded")
}

func budgetExceededRetryHint(tr *cost.Tracker) string {
	if tr != nil {
		suggested := tr.SuggestedBudget()
		if suggested > 0 {
			return fmt.Sprintf("预算已超限，继续重试不会成功。可执行 /budget auto 一键提高到 $%.2f，或手动 /budget <金额>，然后再输入“继续”。", suggested)
		}
	}
	return "预算已超限，继续重试不会成功。请先执行 /budget auto 或 /budget <更大金额>，然后再输入“继续”。"
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
