package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/agentgo/internal/agent"
	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/command"
	"github.com/agentgo/internal/config"
	ctxt "github.com/agentgo/internal/context"
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

type streamingRunner interface {
	RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error)
}

var (
	Version    = "1.0.0"
	BuildTime  = "dev"
	GitCommit  = "unknown"
	resumeMode = false
	resumeID   = ""
)

func main() {
	args := os.Args[1:]
	debugMode, printMode := false, false
	var printPrompt string

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
		case "-d", "--debug":
			debugMode = true
		case "-p", "--print":
			if i+1 < len(args) {
				i++
				printPrompt = args[i]
			}
			printMode = true
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
	skills.LoadAll(skillMgr, projCtx.Cwd)
	memStore := memory.NewStore()
	pluginMgr := plugin.NewManager()
	pluginMgr.Init()

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

	toolReg := registerAllTools(mcpPool, skillMgr)
	cmdReg := registerAllCommands()

	eng, err := engine.New(engine.Config{
		Model:          cfg.Model,
		PermissionMode: string(permMgr.Mode()),
		MaxBudget:      cfg.MaxBudgetUsd,
		Debug:          debugMode || cfg.Debug,
		Tools:          toolReg.All(),
		Provider: api.ProviderConfig{
			Name: pc.Name, APIKey: pc.APIKey, BaseURL: pc.BaseURL,
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
		case "bash":
			if cmd, ok := input["command"].(string); ok {
				if len(cmd) > 80 {
					cmd = cmd[:80] + "..."
				}
				desc = cmd
			}
		default:
			desc = reason
		}
		fmt.Printf("\n  ⚠ Permission required: [%s] %s\n", toolName, desc)
		fmt.Print("  Allow? (y)es / (n)o / (a)lways: ")

		var answer string
		fmt.Scanln(&answer)
		answer = strings.TrimSpace(strings.ToLower(answer))
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
			repl.PrintSafe("Resumed session: %s (%d messages)\n", r.Title, len(r.Messages))
		}
	}

	printBanner(cfg, appState, projCtx, permMgr, eng)

	if printMode && printPrompt != "" {
		runPrintMode(eng, printPrompt, debugMode)
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
	return r
}

func printBanner(cfg *config.Config, s *state.AppState, pc *ctxt.ProjectContext, pm *permission.Manager, eng *engine.Engine) {
	fmt.Println("┌─────────────────────────────────────────────┐")
	fmt.Printf("│  agentgo v%-27s│\n", Version)
	fmt.Println("└─────────────────────────────────────────────┘")
	fmt.Printf("\nModel: %s  Provider: %s  Mode: %s\n", cfg.Model, eng.ProviderName(), pm.Mode())
	if pc.IsGitRepo {
		fmt.Printf("Git: %s (%s)\n", pc.GitBranch, pc.GitStatus)
	}
	fmt.Printf("CWD: %s\n", pc.Cwd)
	fmt.Printf("Tools: %d | Commands: %d\n", len(eng.Registry().All()), 15)
	fmt.Printf("\nType /help | 输入 /help 查看帮助；Ctrl+C exit / Ctrl+C 退出\n")
	fmt.Println()
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
			fmt.Print("\r\n[Interrupted]\r\n")
			cancel()
		case <-ctx.Done():
		}
	}()
	f(ctx)
}

func runREPL(eng *engine.Engine, cmdReg *command.Registry, toolReg *tool.Registry, pm *permission.Manager, as *state.AppState, cfg *config.Config, mcpPool *mcp.Pool, skillMgr *skills.Manager, memStore *memory.Store, pluginMgr *plugin.Manager, projCtx *ctxt.ProjectContext) {

	allCommands := buildCommandList(cmdReg, toolReg)
	reader := repl.New(func(input string) []string {
		return complete(input, allCommands, eng.Runtime().SkillPrompts)
	})

	for {
		input, err := reader.ReadLine()
		if err == repl.ErrInterrupt {
			fmt.Println("Use /exit or Ctrl+D to quit")
			continue
		}
		if err == repl.ErrExit {
			autoSaveSession(eng)
			fmt.Println("Goodbye!")
			return
		}
		if err != nil {
			repl.PrintSafe("\r\nRead error, restarting REPL...\r\n")
			reader = repl.New(func(input string) []string {
				return complete(input, allCommands, eng.Runtime().SkillPrompts)
			})
			continue
		}
		if input == "" {
			continue
		}
		switch {
		case input == "exit" || input == "/exit":
			autoSaveSession(eng)
			fmt.Println("Goodbye!")
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
		case strings.HasPrefix(input, "/model "):
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Model = strings.TrimSpace(strings.TrimPrefix(input, "/model "))
				as.Model = cfg.Model
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("Model update failed: %v\n", err)
				continue
			}
			fmt.Printf("Model: %s (saved)\n", cfg.Model)
		case strings.HasPrefix(input, "/provider "):
			providerName := strings.TrimSpace(strings.TrimPrefix(input, "/provider "))
			if !api.IsKnownProvider(providerName) {
				fmt.Printf("Invalid provider: %s\n", providerName)
				fmt.Println(providerHelpLine())
				continue
			}
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Provider.Name = providerName
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("Provider update failed: %v\n", err)
				continue
			}
			fmt.Printf("Provider: %s (saved)\n", cfg.Provider.Name)
		case strings.HasPrefix(input, "/api-key "):
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Provider.APIKey = strings.TrimSpace(strings.TrimPrefix(input, "/api-key "))
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("API key update failed: %v\n", err)
				continue
			}
			fmt.Println("API key saved")
		case strings.HasPrefix(input, "/base-url "):
			if err := applyProviderConfigChange(cfg, eng, func() error {
				cfg.Provider.BaseURL = strings.TrimSpace(strings.TrimPrefix(input, "/base-url "))
				return config.Save(cfg)
			}); err != nil {
				fmt.Printf("Base URL update failed: %v\n", err)
				continue
			}
			fmt.Println("Base URL saved")
		case strings.HasPrefix(input, "/mode "):
			m := permission.Mode(strings.TrimPrefix(input, "/mode "))
			if permission.ValidMode(m) {
				pm.SetMode(m)
				eng.SetPermissionMode(m)
				cfg.PermissionMode = string(m)
				as.PermissionMode = string(m)
				config.Save(cfg)
				fmt.Printf("Mode: %s\n", m)
			} else {
				fmt.Printf("Invalid. Choose: %s\n", permission.Modes())
			}
		case strings.HasPrefix(input, "/budget "):
			var b float64
			fmt.Sscanf(strings.TrimPrefix(input, "/budget "), "%f", &b)
			if b > 0 {
				cfg.MaxBudgetUsd = b
				as.MaxBudget = b
				config.Save(cfg)
				fmt.Printf("Budget: $%.2f\n", b)
			}
		case input == "/cost":
			fmt.Println("Usage:", eng.CostTracker().Summary())
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
		case strings.HasPrefix(input, "/history "):
			histID := strings.TrimSpace(strings.TrimPrefix(input, "/history "))
			handleHistoryResume(histID, eng)
		case input == "/compact":
			withInterrupt(func(ctx context.Context) {
				eng.Compact(ctx)
				fmt.Println("Context window compacted.")
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
			withInterrupt(func(ctx context.Context) { fmt.Print(runChatInteraction(ctx, eng, input)) })
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
		cmdEntry{Name: "/model", Desc: "Set model name", Type: "config"},
		cmdEntry{Name: "/provider", Desc: "Set API provider", Type: "config", ArgHints: map[string][]string{"": providerNameSuggestions()}},
		cmdEntry{Name: "/api-key", Desc: "Set API key", Type: "config"},
		cmdEntry{Name: "/base-url", Desc: "Set API base URL", Type: "config"},
		cmdEntry{Name: "/mode", Desc: "Set permission mode (default|plan|auto|bypass)", Type: "config", ArgHints: map[string][]string{"": {"default", "plan", "auto", "bypass"}}},
		cmdEntry{Name: "/budget", Desc: "Set max budget ($)", Type: "config"},
		cmdEntry{Name: "/help", Desc: "Show help", Type: "builtin"},
		cmdEntry{Name: "/exit", Desc: "Exit agentgo", Type: "builtin"},
		cmdEntry{Name: "/history", Desc: "View & continue past sessions", Type: "builtin"},
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

func complete(input string, commands []cmdEntry, skills map[string]string) []string {
	input = strings.TrimLeft(input, " \t")
	if input == "" {
		return nil
	}
	if suggestions := completeArgs(input, commands); len(suggestions) > 0 {
		return limitSuggestions(suggestions)
	}
	var matches []string
	lower := strings.ToLower(input)
	for _, c := range commands {
		if strings.HasPrefix(input, "/") && c.Type == "tool" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(c.Name), lower) {
			matches = append(matches, c.Name)
		}
	}
	for name := range skills {
		candidate := name
		if strings.HasPrefix(input, "/") {
			candidate = "/" + name
		}
		if strings.HasPrefix(strings.ToLower(candidate), lower) {
			matches = append(matches, candidate)
		}
	}
	sort.Strings(matches)
	return limitSuggestions(matches)
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

func limitSuggestions(matches []string) []string {
	if len(matches) > 20 {
		return matches[:20]
	}
	return matches
}

func showQuickCommands(commands []cmdEntry) {
	fmt.Println("\nAvailable commands:")
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
		fmt.Printf("Unknown: /%s\nDid you mean?\n", name)
		for _, s := range suggestions {
			fmt.Printf("  /%s\n", s)
		}
		return true
	}
	fmt.Printf("Unknown: /%s. Type /help for available commands.\n", name)
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

func runPrintMode(eng *engine.Engine, prompt string, debug bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() { <-sigCh; cancel() }()
	resp, err := eng.Run(ctx, prompt)
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
		fmt.Printf("Unknown: /%s. Try /help\n", name)
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
		repl.PrintSafe("\n%d skills:\n", len(prompts))
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
			repl.PrintSafe("Registry unavailable: %v\n", err)
			return
		}
		repl.PrintSafe("\nSkill Marketplace (%d available):\n", len(entries))
		for _, e := range entries {
			installed := ""
			if _, ok := prompts[e.Name]; ok {
				installed = " [installed]"
			}
			repl.PrintSafe("  %-16s %s%s\n", e.Name, truncateDesc(e.Description, 48), installed)
		}
		repl.PrintSafe("\nInstall: /skill install <name>\n")

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
				repl.PrintSafe("Installed: %s — restart to load, or use /skill %s\n", name, name)
				return
			}
		}
		skills.InstallSkill(name, "local", "")
		repl.PrintSafe("Created: %s\nEdit ~/.agentgo/skills/%s/SKILL.md\n", name, name)

	case "create":
		if len(parts) < 3 {
			repl.PrintSafe("Usage: /skill create <name>\n")
			return
		}
		name := parts[2]
		skills.InstallSkill(name, "local", "")
		repl.PrintSafe("Created: %s — edit at ~/.agentgo/skills/%s/SKILL.md\n", name, name)

	default:
		name := parts[1]
		prompt, ok := prompts[name]
		if !ok {
			repl.PrintSafe("\nUnknown: %s\nType /skill marketplace to discover skills.\n", name)
			return
		}
		repl.PrintSafe("\n[Skill: %s]\n\n%s\n\nFollow these instructions.\n", name, prompt)
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
		repl.PrintSafe("Unknown skill: %s\n", name)
		return
	}
	args := strings.TrimSpace(strings.TrimPrefix(input, parts[0]))
	repl.PrintSafe("\n[Skill: %s]\n\n%s\n", name, prompt)
	if args != "" {
		repl.PrintSafe("\nUser request:\n%s\n", args)
	}
	repl.PrintSafe("\nFollow these instructions.\n")
}

func handleExport(input string, eng *engine.Engine) {
	filename := "conversation.md"
	parts := strings.Fields(input)
	if len(parts) > 1 {
		filename = parts[1]
	}
	var sb strings.Builder
	sb.WriteString("# Conversation Export\r\n\r\n")
	for _, m := range eng.Messages() {
		sb.WriteString(fmt.Sprintf("**%s**: %s\r\n\r\n", m.Role, m.Content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  > tool: %s(%v)\r\n\r\n", tc.Name, tc.Input))
		}
	}
	os.WriteFile(filename, []byte(sb.String()), 0644)
	repl.PrintSafe("Exported %d messages to %s\n", len(eng.Messages()), filename)
}

func handleResume(ctx context.Context, sessionID string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("Session store unavailable\n")
		return
	}
	if sessionID == "" {
		records, _ := store.List()
		if len(records) == 0 {
			repl.PrintSafe("No saved sessions\n")
			return
		}
		repl.PrintSafe("%d saved sessions:\n", len(records))
		for _, r := range records {
			repl.PrintSafe("  %s  %s  (%d tokens)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("15:04"))
		}
		return
	}
	r, err := store.Load(sessionID)
	if err != nil {
		repl.PrintSafe("Session %s not found\n", sessionID)
		return
	}
	eng.LoadMessages(r.Messages)
	repl.PrintSafe("Resumed: %s (%d msgs, %d tokens)\n", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)
}

func autoSaveSession(eng *engine.Engine) {
	if eng.HasMessages() {
		eng.SaveSession()
		fmt.Println("Session auto-saved.")
	}
}

func handleHistory(eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("Session store unavailable\n")
		return
	}
	records, _ := store.List()
	if len(records) == 0 {
		repl.PrintSafe("No history. Sessions are auto-saved on exit.\n")
		return
	}
	repl.PrintSafe("\n  History (%d sessions):\n\n", len(records))
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
		repl.PrintSafe("  %2d. [%s] %s  (%d msgs)\n", i+1, date, title, msgCount)
	}
	if len(records) > limit {
		repl.PrintSafe("\n  ... and %d more.\n", len(records)-limit)
	}
	repl.PrintSafe("\n  Continue: /history <number>  (e.g. /history 1)\n\n")
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
	return "(empty)"
}

func handleHistoryResume(input string, eng *engine.Engine) {
	store := eng.Store()
	if store == nil {
		repl.PrintSafe("Session store unavailable\n")
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
		repl.PrintSafe("Resumed #%d: %s (%d msgs)\n", idx, title, len(r.Messages))
		repl.PrintSafe("You can now continue the conversation.\n\n")
		return
	}

	// Fallback: try loading by session ID
	r, err := store.Load(input)
	if err != nil {
		repl.PrintSafe("Invalid selection: %s\nUse /history to see available sessions.\n", input)
		return
	}
	eng.LoadMessages(r.Messages)
	title := r.Title
	if title == "New session" || title == "" {
		title = sessionPreview(*r)
	}
	repl.PrintSafe("Resumed: %s (%d msgs)\n", title, len(r.Messages))
	repl.PrintSafe("You can now continue the conversation.\n\n")
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
	return "  /provider <name>    Set provider (anthropic, deepseek, openai, openai-compatible, glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral)"
}

func providerEnvHelpLine() string {
	return "Env Vars: LLM_API_KEY | ANTHROPIC_API_KEY | DEEPSEEK_API_KEY | OPENAI_API_KEY | GLM_API_KEY | KIMI_API_KEY | QWEN_API_KEY | OPENROUTER_API_KEY | SILICONFLOW_API_KEY | LLM_BASE_URL"
}

func printHelp(cmdReg *command.Registry, toolReg *tool.Registry) {
	fmt.Println("\n=== agentgo v" + Version + " ===")
	fmt.Println("\nProvider / Model:")
	fmt.Println("  /model <name>       Set model")
	fmt.Println(providerHelpLine())
	fmt.Println("  /api-key <key>      Save API key")
	fmt.Println("  /base-url <url>     Set custom base URL")
	fmt.Println("  /mode <mode>        Set permission mode (default|plan|auto|bypass)")
	fmt.Println("  /budget <amount>    Max cost per session ($)")
	fmt.Println("  /cost               Show token/cost usage")
	fmt.Println("  /config             Show full config")
	fmt.Println("\nSession:")
	fmt.Println("  /compact            Compact conversation history")
	fmt.Println("  /history            View & continue past sessions")
	fmt.Println("  /resume [id]        Resume saved session")
	fmt.Println("  /memory             Manage persistent memory")
	fmt.Println("\nSystem:")
	fmt.Println("  /mcp                Manage MCP servers")
	fmt.Println("  /plugin             Manage plugins")
	fmt.Println("  /skills             List skills")
	fmt.Println("\nCommands:")
	for _, c := range cmdReg.All() {
		fmt.Printf("  /%-16s %s\n", c.Name(), c.Description())
	}
	fmt.Println("\nTools:")
	for _, t := range toolReg.All() {
		d := t.Def()
		ro := " "
		if d.IsReadOnly {
			ro = "R"
		}
		fmt.Printf("  [%s] %-12s %s\n", ro, d.Name, truncateDesc(d.Description, 48))
	}
	fmt.Println("\n" + providerEnvHelpLine())
	fmt.Println("Flags: -p <prompt> | -d --debug | -v --version | --doctor | --config")
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

func runChatInteraction(ctx context.Context, runner streamingRunner, input string) string {
	var sb strings.Builder
	sb.WriteString("\n")
	_, err := runner.RunWithStream(ctx, input, func(delta string) {
		sb.WriteString(delta)
	})
	if err != nil {
		sb.WriteString(fmt.Sprintf("Request failed: %v", err))
	}
	sb.WriteString("\r\n\r\n")
	return sb.String()
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
		fmt.Println("No saved sessions")
		return
	}
	fmt.Printf("%d sessions:\n", len(records))
	for _, r := range records {
		fmt.Printf("  %s  %s  (%dt)  %s\n", r.ID, r.Title, r.TokensIn+r.TokensOut, r.UpdatedAt.Format("2006-01-02 15:04"))
	}
}

func printCLIHelp() {
	fmt.Println(`agentgo — Go-powered AI coding assistant

Usage:
 agentgo                       Start interactive REPL
 agentgo -p <prompt>            Run a single query and exit
 agentgo -r <id>                Resume a saved session
 agentgo --list-sessions        List saved sessions
 agentgo -d                     Start with debug logging
 agentgo --doctor               Run diagnostics
 agentgo --config               Show configuration
 agentgo -v, --version          Show version
 agentgo -h, --help             Show this help

Skill Commands:
 /skill [name]                  Activate a skill
 /skill marketplace             Browse skill marketplace
 /skill install <name>          Install from marketplace
 /skill create <name>           Create new skill

REPL Commands:
 /model, /provider, /api-key, /base-url, /mode, /budget
 /cost, /config, /system, /context, /compact
 /commit, /review, /diff, /export
 /resume, /memory, /mcp, /plugin
 /doctor, /status, /stats, /permissions
 /cd, /help, /exit`)
}

func truncateDesc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
