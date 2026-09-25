package main

import (
	"runtime"

	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/tool"
)

// toolOptions decides which optional tools are registered.
type toolOptions struct {
	goos string
	// interactive: someone can answer the question tool (not -p, not the
	// piped/headless frontend).
	interactive bool
	// experimental is the experimental_tools config key: the coordination
	// tools task/task_*, team_*, send_message, brief and sleep.
	experimental bool
	// chrome: headless Chrome is compiled in (-tags chromedp); without it the
	// browser tool is only a worse webfetch.
	chrome    bool
	webSearch tool.WebSearchSettings
}

func registerAllTools(mcpPool *mcp.Pool, cfg *config.Config, interactive bool) *tool.Registry {
	o := toolOptions{goos: runtime.GOOS, interactive: interactive, chrome: tool.BrowserChromeAvailable()}
	if cfg != nil {
		o.experimental = cfg.ExperimentalTools
		if cfg.WebSearch != nil {
			o.webSearch = tool.WebSearchSettings{Provider: cfg.WebSearch.Provider, APIKey: cfg.WebSearch.APIKey}
		}
	}
	return registerToolsWith(mcpPool, o)
}

// registerToolsFor registers every tool that can work on goos, experimental
// ones included (tests use it to inspect the full catalogue).
func registerToolsFor(mcpPool *mcp.Pool, goos string) *tool.Registry {
	return registerToolsWith(mcpPool, toolOptions{goos: goos, interactive: true, experimental: true, chrome: true})
}

// registerToolsWith builds the tool registry. Every registered tool costs
// prompt tokens on every request, so tools that cannot work here are left
// out: powershell off Windows, lsp (no language-server runner is wired in),
// cron (schedules were recorded but never fired), question when nobody can
// answer, browser without headless Chrome, and the experimental coordination
// tools unless experimental_tools is on. Their constructors stay in
// internal/tool.
func registerToolsWith(mcpPool *mcp.Pool, o toolOptions) *tool.Registry {
	r := tool.NewRegistry()

	// Planning and workspace isolation.
	r.Register(tool.NewPlanModeTool())
	r.Register(tool.NewExitPlanModeTool())
	r.Register(tool.NewEnterWorktreeTool())
	r.Register(tool.NewExitWorktreeTool())

	if o.experimental {
		// Task lifecycle.
		r.Register(tool.NewTaskCreateTool())
		r.Register(tool.NewTaskListTool())
		r.Register(tool.NewTaskUpdateTool())
		r.Register(tool.NewTaskStopTool())
		r.Register(tool.NewTaskGetTool())
		r.Register(tool.NewTaskOutputTool())

		// Coordination.
		r.Register(tool.NewSleepTool())
		r.Register(tool.NewBriefTool())
		r.Register(tool.NewTeamCreateTool())
		r.Register(tool.NewTeamDeleteTool())
		r.Register(tool.NewSendMessageTool())
	}

	// Agent operations.
	r.Register(tool.NewSkillTool())
	r.Register(tool.NewAgentTool())

	// Core local tools.
	r.Register(tool.NewBashTool())
	r.Register(tool.NewReadTool())
	r.Register(tool.NewWriteTool())
	r.Register(tool.NewEditTool())
	r.Register(tool.NewGrepTool())
	r.Register(tool.NewGlobTool())
	// The system prompt carries only a project outline; symbols and line
	// numbers are queried here.
	r.Register(tool.NewRepoMapTool())
	r.Register(tool.NewWebFetchTool())
	if o.chrome {
		r.Register(tool.NewBrowserTool())
	}
	if o.interactive {
		r.Register(tool.NewQuestionTool())
	}
	r.Register(tool.NewTodoWriteTool())
	r.Register(tool.NewExecutePlanTool())
	r.Register(tool.NewWebSearchToolWith(o.webSearch))
	if o.goos == "windows" {
		r.Register(tool.NewPowerShellTool())
	}
	r.Register(tool.NewSkillsListTool())
	r.Register(tool.NewSkillViewTool())
	r.Register(tool.NewDrawImageTool())

	// MCP proxy tools — expose tools/resources from connected MCP servers to the
	// agent so it can invoke external capabilities once a server is connected.
	r.Register(tool.NewMCPTool(mcpPool))
	r.Register(tool.NewListMCPResourcesTool(mcpPool))
	r.Register(tool.NewReadMCPResourceTool(mcpPool))

	return r
}

func registerAllCommands() *command.Registry {
	r := command.NewRegistry()

	// Git workflow.
	r.Register(command.NewCommitCmd())
	r.Register(command.NewReviewCmd())
	r.Register(command.NewDiffCmd())

	// Runtime and diagnostics.
	r.Register(command.NewDoctorCmd())
	r.Register(command.NewConfigCmd())
	r.Register(command.NewDiagnoseCmd())

	// Session and memory.
	r.Register(command.NewCompactCmd())
	r.Register(command.NewCostCmd())
	r.Register(command.NewRateLimitCmd())
	r.Register(command.NewUndoCmd())
	r.Register(command.NewCheckpointsCmd())
	r.Register(command.NewMemoryCmd())
	r.Register(command.NewResumeCmd())
	r.Register(command.NewHistoryCmd())
	r.Register(command.NewExportCmd())
	r.Register(command.NewSystemCmd())
	r.Register(command.NewStatusCmd())
	r.Register(command.NewStatsCmd())

	// Workspace context and permissions.
	r.Register(command.NewCdCmd())
	r.Register(command.NewContextCmd())
	r.Register(command.NewPermissionsCmd())

	// Project and ecosystem.
	r.Register(command.NewInitCmd())
	r.Register(command.NewDreamCmd())
	r.Register(command.NewHooksCmd())
	// Keep SkillsCmd registered so command-registry and help output stay complete;
	// REPL still routes /skill and /skills to the richer built-in handler first.
	r.Register(command.NewSkillsCmd())
	r.Register(command.NewPluginCmd())
	r.Register(command.NewMcpCmd())

	return r
}
