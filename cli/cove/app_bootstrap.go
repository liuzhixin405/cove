package main

import (
	"context"
	"fmt"
	"os"
	"time"

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
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/state"
	"github.com/liuzhixin405/cove/internal/tool"
)

// mcpBootstrapTimeout caps the total time spent connecting configured MCP
// servers during startup.
const mcpBootstrapTimeout = 30 * time.Second

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

// bootstrapApp builds the session. interactive says whether someone can
// answer the question tool (main's mode branch: not -p, interactive shell).
func bootstrapApp(debugMode bool, profileName, recordDir, replayDir string, interactive bool) (*appBootstrap, error) {
	cfg, err := config.LoadWithProfile(profileName)
	if err != nil {
		log.Warnf("config load: %v", err)
		if cfg == nil {
			cfg = config.DefaultConfig()
		}
	}
	if err := config.Migrate(cfg, 0); err != nil {
		log.Warnf("config migrate: %v", err)
	}
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
	// User-level hooks only (hooks.json in the config directory): a
	// project-level file would let a cloned repository run commands on this
	// machine.
	defs, err := loadUserHooks()
	if err != nil {
		log.Warnf("hooks config: %v", err)
	}
	hookMgr.RegisterDefs(defs)
	skillMgr := skills.NewManager()
	skills.LoadAll(skillMgr, projCtx.Cwd)
	skillMgr.Disable(cfg.DisabledSkills...)
	memStore := newProjectMemoryStore(projCtx.Cwd)
	// Masked tool outputs older than a week are never read again.
	go tool.PruneOldToolOutputs(tool.ToolOutputDir(), tool.ToolOutputMaxAge)
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
		// Startup must stay bounded: a misconfigured or unresponsive MCP server
		// would otherwise hang the whole launch. Servers that miss the window
		// are logged and skipped; the user can reconnect them via /mcp.
		mcpCtx, cancelMCP := context.WithTimeout(context.Background(), mcpBootstrapTimeout)
		mcpPool.LoadFromConfig(mcpCtx, servers)
		cancelMCP()
	}

	toolReg := registerAllTools(mcpPool, cfg, interactive)
	eng, err := engine.New(engine.Config{
		Model:          cfg.Model,
		ModelFast:      cfg.ModelFast,
		PermissionMode: string(permMgr.Mode()),
		MaxBudget:      cfg.MaxBudgetUsd,
		Debug:          debugMode || cfg.Debug,
		RecordingDir:   recordDir,
		ReplayDir:      replayDir,
		Tools:          toolReg.All(),
		Provider: api.ProviderConfig{
			Name: pc.Name, APIKey: pc.APIKey, APIKeys: pc.APIKeys, BaseURL: pc.BaseURL,
		},
		MemoryStore:        memStore,
		SkillManager:       skillMgr,
		HookManager:        hookMgr,
		Classifier:         classifier,
		DoneVerifyCommands: cfg.DoneVerifyCommands,
		DoneVerifyAuto:     cfg.VerifyAutoEnabled(),
		DoneVerifyTimeout:  time.Duration(cfg.DoneVerifyTimeoutSeconds) * time.Second,
		DoneCheck:          cfg.DoneCheckMode(),
		Thinking:           cfg.Thinking,
		Effort:             cfg.Effort,
		CustomInstructions: cfg.SystemPrompt,

		MaxIterations:         cfg.MaxIterations,
		MaxTurnMinutes:        cfg.MaxTurnMinutes,
		SubagentMaxIterations: cfg.SubagentMaxIterations,
		MaxSessions:           cfg.MaxSessions,
	})
	if err != nil {
		return nil, fmt.Errorf("engine start error: %w", err)
	}

	eng.SetProjectContext(projCtx)
	eng.WirePlanExecutor()
	wireDiagnostics(eng, memStore)
	showReasoning = cfg.ShowReasoning

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

// newProjectMemoryStore is the memory store for the project around cwd: its
// own ~/.cove/projects/<hash>/memory (written to, and winning on name clashes)
// merged with the global ~/.cove/memory, which is read but not migrated.
func newProjectMemoryStore(cwd string) *memory.Store {
	dir, err := config.ProjectDataDir(memory.ProjectRoot(cwd))
	if err != nil {
		log.Warnf("project data dir: %v (using the global memory directory only)", err)
		return memory.NewStore()
	}
	return memory.NewProjectStore(dir)
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

// loadUserHooks reads hooks.json from the config directory policies.json and
// config.json are read from (COVE_CONFIG_DIR, else ~/.cove). It used to be
// ~/.cove regardless, so COVE_CONFIG_DIR moved the config but not the hooks.
func loadUserHooks() ([]hooks.HookDef, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil, err
	}
	return hooks.LoadConfigDir(dir)
}

// wireDiagnostics points /diagnose and /doctor at the running session: the
// engine's dream runner and memory store, and why policies.json failed to
// load. Unset, the checker re-reads the same state from disk.
func wireDiagnostics(eng *engine.Engine, memStore *memory.Store) {
	diagnostic.PolicyLoadErrorFn = eng.PolicyLoadError
	diagnostic.BackgroundStatusFn = func() diagnostic.BackgroundStatus {
		st := diagnostic.BackgroundStatus{Dream: eng.DreamStatus()}
		if memStore != nil {
			st.Memory = memStore.Stats()
		}
		return st
	}
}
