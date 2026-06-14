package main

import (
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/tool"
)

func registerAllTools(mcpPool *mcp.Pool) *tool.Registry {
	r := tool.NewRegistry()

	// Planning and workspace isolation.
	r.Register(tool.NewPlanModeTool())
	r.Register(tool.NewExitPlanModeTool())
	r.Register(tool.NewEnterWorktreeTool())
	r.Register(tool.NewExitWorktreeTool())

	// Task lifecycle.
	r.Register(tool.NewTaskCreateTool())
	r.Register(tool.NewTaskListTool())
	r.Register(tool.NewTaskUpdateTool())
	r.Register(tool.NewTaskStopTool())
	r.Register(tool.NewTaskGetTool())
	r.Register(tool.NewTaskOutputTool())

	// Coordination and agent operations.
	r.Register(tool.NewSleepTool())
	r.Register(tool.NewBriefTool())
	r.Register(tool.NewSkillTool())
	r.Register(tool.NewAgentTool())
	r.Register(tool.NewTeamCreateTool())
	r.Register(tool.NewTeamDeleteTool())
	r.Register(tool.NewCronTool())
	r.Register(tool.NewSendMessageTool())
	r.Register(tool.NewLSPTool())

	// Core local tools.
	r.Register(tool.NewBashTool())
	r.Register(tool.NewReadTool())
	r.Register(tool.NewWriteTool())
	r.Register(tool.NewEditTool())
	r.Register(tool.NewGrepTool())
	r.Register(tool.NewGlobTool())
	r.Register(tool.NewWebFetchTool())
	r.Register(tool.NewBrowserTool())
	r.Register(tool.NewQuestionTool())
	r.Register(tool.NewTodoWriteTool())
	r.Register(tool.NewExecutePlanTool())
	r.Register(tool.NewWebSearchTool())
	r.Register(tool.NewPowerShellTool())
	r.Register(tool.NewSkillsListTool())
	r.Register(tool.NewSkillViewTool())

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
	r.Register(command.NewMemoryCmd())
	r.Register(command.NewResumeCmd())
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
	// Keep SkillsCmd registered so command-registry and help output stay complete;
	// REPL still routes /skill and /skills to the richer built-in handler first.
	r.Register(command.NewSkillsCmd())
	r.Register(command.NewPluginCmd())
	r.Register(command.NewMcpCmd())

	return r
}
