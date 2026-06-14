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
	if issues := diagnostic.QuickCheck(cfg); len(issues) > 0 {
		fmt.Fprintf(os.Stderr, "\n\x1b[33m? 启动时检测到可能的问题：\x1b[0m\n")
		for _, issue := range issues {
			fmt.Fprintf(os.Stderr, "  %s\n", issue.Format())
		}
		fmt.Fprintf(os.Stderr, "  \x1b[2m运行 /diagnose 获取完整报告\x1b[0m\n\n")
	}

	if scan := diagnostic.ScanRuntimeLogOnStartup(); (debugMode || cfg.Debug) && scan.HasProblems() {
		const reset = "\x1b[0m"
		fmt.Fprintf(os.Stderr, "\n\x1b[33m⚠ 上次运行记录到 %d 条问题（最早 %s），建议修复：\x1b[0m\n",
			scan.Total, scan.Since.Format("01-02 15:04"))
		shown := 0
		for _, s := range scan.Summaries {
			if shown >= 5 {
				break
			}
			count := ""
			if s.Count > 1 {
				count = fmt.Sprintf(" ×%d", s.Count)
			}
			fmt.Fprintf(os.Stderr, "  %s%s%s %s%s\n", s.Severity.Color(), s.Severity.String(), reset, s.Message, count)
			if s.Recovery != "" {
				fmt.Fprintf(os.Stderr, "    \x1b[2m%s\x1b[0m\n", s.Recovery)
			}
			shown++
		}
		fmt.Fprintf(os.Stderr, "  \x1b[2m查看全部: /diagnose errors  修复后归档: /diagnose archive\x1b[0m\n\n")
	}
}
