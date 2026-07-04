package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
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
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/state"
	"github.com/liuzhixin405/cove/internal/tool"
)

type appBootstrap struct {
	cfg       *config.Config
	eng       *engine.Engine
	permMgr   *permission.Manager
	appState  *state.AppState
	mcpPool   *mcp.Pool
	skillMgr  *skills.Manager
	memStore  *memory.Store
	pluginMgr *plugin.Manager
	projCtx   *ctxt.ProjectContext
	toolReg   *tool.Registry
}

func bootstrapApp(debugMode bool) (*appBootstrap, error) {
	cfg, err := config.Load()
	if err != nil {
		log.Warnf("config load: %v", err)
		cfg = config.DefaultConfig()
	}
	config.Migrate(cfg, 0)
	diagnostic.AttachToLogger()
	if debugMode {
		log.SetLevel(log.Debug)
	}

	pc := cfg.EffectiveProvider()
	projCtx := ctxt.Collect()
	appState := state.NewState()
	appState.Model = cfg.Model
	appState.ModelFast = cfg.ModelFast
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
	if cfg.MemoryEmbedding != nil {
		// Reuse the main chat provider's base URL/API key when the embedding
		// config doesn't override them — enabling this should not require a
		// second account. See internal/memory/embed.go and
		// docs/中等模型平替优化建议.md §2.2.
		embedBaseURL := cfg.MemoryEmbedding.BaseURL
		if embedBaseURL == "" {
			embedBaseURL = pc.BaseURL
		}
		embedAPIKey := cfg.MemoryEmbedding.APIKey
		if embedAPIKey == "" {
			embedAPIKey = pc.APIKey
		}
		if embedBaseURL != "" && embedAPIKey != "" {
			memStore.EnableRemoteEmbeddings(memory.NewRemoteAPIEmbeddingProvider(embedBaseURL, embedAPIKey, cfg.MemoryEmbedding.Model))
		} else {
			log.Warnf("memory_embedding configured but no base_url/api_key resolved (from config or provider) — semantic memory search stays disabled")
		}
	}

	pluginMgr := plugin.NewManager()
	pluginMgr.Init()

	mcpPool := mcp.NewPool()
	if len(cfg.MCPServers) > 0 {
		servers := make(map[string]mcp.ServerConfig, len(cfg.MCPServers))
		for name, sc := range cfg.MCPServers {
			servers[name] = mcp.ServerConfig(sc)
		}
		mcpPool.LoadFromConfig(context.Background(), servers)
	}

	toolReg := registerAllTools(mcpPool)
	eng, err := engine.New(engine.Config{
		Model:          cfg.Model,
		ModelFast:      cfg.ModelFast,
		PermissionMode: string(permMgr.Mode()),
		MaxBudget:      cfg.MaxBudgetUsd,
		Debug:          debugMode || cfg.Debug,
		Tools:          toolReg.All(),
		Provider: api.ProviderConfig{
			Name: pc.Name, APIKey: pc.APIKey, APIKeys: pc.APIKeys, BaseURL: pc.BaseURL,
		},
		MemoryStore:        memStore,
		SkillManager:       skillMgr,
		HookManager:        hookMgr,
		Classifier:         classifier,
		DoneVerifyCommands: cfg.DoneVerifyCommands,
	})
	if err != nil {
		return nil, fmt.Errorf("engine start error: %w", err)
	}

	eng.SetProjectContext(projCtx)
	eng.WirePlanExecutor()

	return &appBootstrap{
		cfg:       cfg,
		eng:       eng,
		permMgr:   permMgr,
		appState:  appState,
		mcpPool:   mcpPool,
		skillMgr:  skillMgr,
		memStore:  memStore,
		pluginMgr: pluginMgr,
		projCtx:   projCtx,
		toolReg:   toolReg,
	}, nil
}

func configurePermissionPrompt(eng *engine.Engine) {
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		desc := ""
		switch toolName {
		case "write", "edit":
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
			if replInteractive {
				ch := make(chan string, 1)
				repl.SetPermInputCh(ch)
				repl.BeginPromptInput()
				line, ok := <-ch
				repl.EndPromptInput()
				if !ok {
					return ""
				}
				return strings.TrimSpace(strings.ToLower(line))
			}
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
			eng.AddPermissionRule(permission.DAllow, permission.Rule{ToolPattern: toolName})
			return true
		default:
			return false
		}
	}
}

func runStartupDiagnostics(cfg *config.Config, debugMode bool) {
	if s := startupDiagnosticsText(cfg, debugMode); s != "" {
		fmt.Fprint(os.Stderr, s)
	}
}

// startupDiagnosticsText renders the startup diagnostic notices (config/network
// issues, last-run problems) as a string so both the classic REPL (printed to
// stderr) and the full-screen TUI (seeded into the transcript) can show them.
func startupDiagnosticsText(cfg *config.Config, debugMode bool) string {
	if issues := diagnostic.QuickCheck(cfg); len(issues) > 0 {
		return "\n  \x1b[90m⚠️  系统检测到潜在环境或配置异常，建议输入 \x1b[36m/diagnose\x1b[90m 查看并进行一键修复。\x1b[0m\n"
	}
	return ""
}
