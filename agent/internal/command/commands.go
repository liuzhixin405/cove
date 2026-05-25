package command

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentgo/internal/config"
	ctxt "github.com/agentgo/internal/context"
	"github.com/agentgo/internal/mcp"
	"github.com/agentgo/internal/memory"
	"github.com/agentgo/internal/permission"
	"github.com/agentgo/internal/plugin"
	"github.com/agentgo/internal/skills"
)

type CommitCmd struct{}
type ReviewCmd struct{}
type DoctorCmd struct{}
type ConfigCmd struct{}
type CompactCmd struct{}
type CostCmd struct{}
type DiffCmd struct{}
type MemoryCmd struct{}
type ResumeCmd struct{}
type McpCmd struct{}
type PluginCmd struct{}
type SkillsCmd struct{}
type ExportCmd struct{}
type SystemCmd struct{}
type CdCmd struct{}
type ContextCmd struct{}

type PermissionsCmd struct{}
type StatusCmd struct{}
type StatsCmd struct{}

func NewCommitCmd() Command      { return &CommitCmd{} }
func NewReviewCmd() Command      { return &ReviewCmd{} }
func NewDoctorCmd() Command      { return &DoctorCmd{} }
func NewConfigCmd() Command      { return &ConfigCmd{} }
func NewCompactCmd() Command     { return &CompactCmd{} }
func NewCostCmd() Command        { return &CostCmd{} }
func NewDiffCmd() Command        { return &DiffCmd{} }
func NewMemoryCmd() Command      { return &MemoryCmd{} }
func NewResumeCmd() Command      { return &ResumeCmd{} }
func NewMcpCmd() Command         { return &McpCmd{} }
func NewPluginCmd() Command      { return &PluginCmd{} }
func NewSkillsCmd() Command      { return &SkillsCmd{} }
func NewExportCmd() Command      { return &ExportCmd{} }
func NewSystemCmd() Command      { return &SystemCmd{} }
func NewCdCmd() Command          { return &CdCmd{} }
func NewContextCmd() Command     { return &ContextCmd{} }
func NewPermissionsCmd() Command { return &PermissionsCmd{} }
func NewStatusCmd() Command      { return &StatusCmd{} }
func NewStatsCmd() Command       { return &StatsCmd{} }

func (c *CommitCmd) Name() string        { return "commit" }
func (c *CommitCmd) Aliases() []string   { return nil }
func (c *CommitCmd) Description() string { return "Stage and create a git commit" }
func (c *CommitCmd) Help() string        { return "/commit [message] - Stage all changes and create a commit" }
func (c *CommitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	sc := exec.CommandContext(ctx, "git", "status", "--porcelain")
	sc.Dir = in.Cwd
	so, _ := sc.Output()
	if strings.TrimSpace(string(so)) == "" {
		return Output{Message: "No changes to commit"}, nil
	}
	msg := "auto-commit"
	if len(in.Args) > 0 {
		msg = strings.Join(in.Args, " ")
	}
	ac := exec.CommandContext(ctx, "git", "add", "-A")
	ac.Dir = in.Cwd
	_ = ac.Run()
	cc := exec.CommandContext(ctx, "git", "commit", "-m", msg)
	cc.Dir = in.Cwd
	co, err := cc.CombinedOutput()
	if err != nil {
		return Output{Message: fmt.Sprintf("Commit failed: %s", strings.TrimSpace(string(co))), Data: string(so)}, nil
	}
	return Output{Message: fmt.Sprintf("Committed: %s", msg), Data: string(co)}, nil
}

func (c *ReviewCmd) Name() string        { return "review" }
func (c *ReviewCmd) Aliases() []string   { return nil }
func (c *ReviewCmd) Description() string { return "Review working tree changes" }
func (c *ReviewCmd) Help() string        { return "/review - Show and analyze uncommitted changes" }
func (c *ReviewCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--stat")
	cmd.Dir = in.Cwd
	out, _ := cmd.Output()
	diff := strings.TrimSpace(string(out))
	if diff == "" {
		cmd = exec.CommandContext(ctx, "git", "diff", "--cached", "--stat")
		cmd.Dir = in.Cwd
		out, _ = cmd.Output()
		diff = strings.TrimSpace(string(out))
	}
	if diff == "" {
		return Output{Message: "No changes to review"}, nil
	}
	return Output{Message: "Changed files:\n" + diff}, nil
}

func (c *DoctorCmd) Name() string        { return "doctor" }
func (c *DoctorCmd) Aliases() []string   { return nil }
func (c *DoctorCmd) Description() string { return "System diagnostics" }
func (c *DoctorCmd) Help() string        { return "/doctor - Check Go, git, ripgrep, config" }
func (c *DoctorCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== System Diagnostics ===\n")
	sb.WriteString(fmt.Sprintf("CWD: %s\n", in.Cwd))
	if g, err := exec.LookPath("git"); err == nil {
		sb.WriteString(fmt.Sprintf("Git: %s\n", g))
	} else {
		sb.WriteString("Git: NOT FOUND\n")
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		sb.WriteString(fmt.Sprintf("Ripgrep: %s\n", rg))
	} else {
		sb.WriteString("Ripgrep: NOT FOUND\n")
	}
	sb.WriteString(fmt.Sprintf("Time: %s\n", time.Now().Format(time.RFC3339)))
	return Output{Message: sb.String()}, nil
}

func (c *ConfigCmd) Name() string        { return "config" }
func (c *ConfigCmd) Aliases() []string   { return nil }
func (c *ConfigCmd) Description() string { return "View or modify configuration" }
func (c *ConfigCmd) Help() string        { return "/config [key] [value] - View/set config values" }
func (c *ConfigCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cfg := in.Config
	if cfg == nil {
		return Output{Message: "Config unavailable"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "show" {
		return Output{Message: renderConfig(cfg)}, nil
	}
	key := strings.ToLower(in.Args[0])
	if len(in.Args) == 1 {
		return Output{Message: fmt.Sprintf("%s = %s", key, configValue(cfg, key))}, nil
	}
	value := strings.Join(in.Args[1:], " ")
	if err := applyConfigValue(cfg, key, value); err != nil {
		return Output{}, err
	}
	if in.SaveConfig != nil {
		if err := in.SaveConfig(cfg); err != nil {
			return Output{}, err
		}
	}
	return Output{Message: fmt.Sprintf("Saved %s = %s", key, configValue(cfg, key))}, nil
}

func (c *CompactCmd) Name() string        { return "compact" }
func (c *CompactCmd) Aliases() []string   { return nil }
func (c *CompactCmd) Description() string { return "Compact conversation history" }
func (c *CompactCmd) Help() string {
	return "/compact - Summarize earlier messages to free context window"
}
func (c *CompactCmd) Execute(ctx context.Context, in Input) (Output, error) {
	return Output{Message: "Use /compact from the REPL built-in path."}, nil
}

func (c *CostCmd) Name() string        { return "cost" }
func (c *CostCmd) Aliases() []string   { return nil }
func (c *CostCmd) Description() string { return "Show token usage and cost" }
func (c *CostCmd) Help() string        { return "/cost - Display session token usage and estimated cost" }
func (c *CostCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "Cost tracker unavailable"}, nil
	}
	return Output{Message: in.Engine.CostTracker().Summary()}, nil
}

func (c *DiffCmd) Name() string        { return "diff" }
func (c *DiffCmd) Aliases() []string   { return nil }
func (c *DiffCmd) Description() string { return "Show git diff" }
func (c *DiffCmd) Help() string        { return "/diff - Show working tree diff" }
func (c *DiffCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cmd := exec.CommandContext(ctx, "git", "diff")
	cmd.Dir = in.Cwd
	out, _ := cmd.Output()
	if strings.TrimSpace(string(out)) == "" {
		cmd = exec.CommandContext(ctx, "git", "diff", "--cached")
		cmd.Dir = in.Cwd
		out, _ = cmd.Output()
	}
	if strings.TrimSpace(string(out)) == "" {
		return Output{Message: "No diff"}, nil
	}
	return Output{Data: string(out)}, nil
}

func (c *MemoryCmd) Name() string        { return "memory" }
func (c *MemoryCmd) Aliases() []string   { return nil }
func (c *MemoryCmd) Description() string { return "Manage persistent memory files" }
func (c *MemoryCmd) Help() string        { return "/memory [list|add|remove] - Manage persistent memories" }
func (c *MemoryCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MemoryStore == nil {
		return Output{Message: "Memory store unavailable"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		entries := in.MemoryStore.All()
		if len(entries) == 0 {
			return Output{Message: "No memory files"}, nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		var sb strings.Builder
		sb.WriteString("Memory files:\n")
		for _, e := range entries {
			sb.WriteString(fmt.Sprintf("- %s\n", e.Name))
		}
		return Output{Message: sb.String()}, nil
	}
	switch in.Args[0] {
	case "add":
		if len(in.Args) < 3 {
			return Output{Message: "Usage: /memory add <name> <content>"}, nil
		}
		name := in.Args[1]
		content := strings.Join(in.Args[2:], " ")
		if err := in.MemoryStore.Save(name, content); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Memory '%s' saved", name)}, nil
	case "remove", "delete", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /memory remove <name>"}, nil
		}
		if err := in.MemoryStore.Delete(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Memory '%s' removed", in.Args[1])}, nil
	default:
		return Output{Message: "Usage: /memory [list|add|remove]"}, nil
	}
}

func (c *ResumeCmd) Name() string        { return "resume" }
func (c *ResumeCmd) Aliases() []string   { return nil }
func (c *ResumeCmd) Description() string { return "Resume a saved session" }
func (c *ResumeCmd) Help() string        { return "/resume [session-id] - List or resume saved sessions" }
func (c *ResumeCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SessionStore == nil {
		return Output{Message: "Session store unavailable"}, nil
	}
	if len(in.Args) == 0 {
		records, err := in.SessionStore.List()
		if err != nil {
			return Output{}, err
		}
		if len(records) == 0 {
			return Output{Message: "No saved sessions"}, nil
		}
		var sb strings.Builder
		sb.WriteString("Saved sessions:\n")
		for _, r := range records {
			sb.WriteString(fmt.Sprintf("- %s  %s  (%d tokens)\n", r.ID, r.Title, r.TokensIn+r.TokensOut))
		}
		return Output{Message: sb.String()}, nil
	}
	r, err := in.SessionStore.Load(in.Args[0])
	if err != nil {
		return Output{}, err
	}
	if in.Engine != nil {
		in.Engine.LoadMessages(r.Messages)
	}
	if in.AppState != nil {
		in.AppState.SessionID = r.ID
		if r.Model != "" {
			in.AppState.Model = r.Model
		}
		in.AppState.Messages = len(r.Messages)
		in.AppState.BudgetUsed = r.Cost
	}
	return Output{Message: fmt.Sprintf("Resumed: %s (%d msgs, %d tokens)", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)}, nil
}

func (c *McpCmd) Name() string        { return "mcp" }
func (c *McpCmd) Aliases() []string   { return nil }
func (c *McpCmd) Description() string { return "Manage MCP servers" }
func (c *McpCmd) Help() string        { return "/mcp [list|connect|disconnect|read] - Manage MCP servers" }
func (c *McpCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MCPPool == nil {
		return Output{Message: "MCP pool unavailable"}, nil
	}
	action := "list"
	if len(in.Args) > 0 {
		action = in.Args[0]
	}
	switch action {
	case "list":
		servers := in.MCPPool.AllServers()
		if len(servers) == 0 {
			return Output{Message: "No MCP servers connected"}, nil
		}
		var sb strings.Builder
		for _, s := range servers {
			typeName := s.Config.Type
			if typeName == "" {
				typeName = "stdio"
			}
			sb.WriteString(fmt.Sprintf("- %s [%s] connected=%t tools=%d resources=%d\n", s.Name, typeName, s.Connected, len(s.Tools), len(s.Resources)))
		}
		return Output{Message: sb.String()}, nil
	case "disconnect":
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /mcp disconnect <name|all>"}, nil
		}
		if in.Args[1] == "all" {
			in.MCPPool.DisconnectAll()
			return Output{Message: "Disconnected all MCP servers"}, nil
		}
		in.MCPPool.Disconnect(in.Args[1])
		return Output{Message: fmt.Sprintf("Disconnected MCP server: %s", in.Args[1])}, nil
	case "connect":
		if in.Config == nil {
			return Output{Message: "Config unavailable"}, nil
		}
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /mcp connect <name>"}, nil
		}
		cfg, ok := in.Config.MCPServers[in.Args[1]]
		if !ok {
			return Output{Message: fmt.Sprintf("MCP server '%s' not found in config", in.Args[1])}, nil
		}
		if err := in.MCPPool.Connect(ctx, in.Args[1], mcp.ServerConfig(cfg)); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Connected MCP server: %s", in.Args[1])}, nil
	case "read":
		if len(in.Args) < 3 {
			return Output{Message: "Usage: /mcp read <server> <resource-uri>"}, nil
		}
		result, err := in.MCPPool.ReadResource(ctx, in.Args[1], in.Args[2])
		if err != nil {
			return Output{}, err
		}
		if result == nil || len(result.Contents) == 0 {
			return Output{Message: fmt.Sprintf("No content for MCP resource: %s", in.Args[2])}, nil
		}
		var sb strings.Builder
		for _, block := range result.Contents {
			if block.Text != "" {
				sb.WriteString(block.Text)
				if !strings.HasSuffix(block.Text, "\n") {
					sb.WriteString("\n")
				}
				continue
			}
			if block.Data != "" {
				sb.WriteString(block.Data)
				if !strings.HasSuffix(block.Data, "\n") {
					sb.WriteString("\n")
				}
			}
		}
		text := strings.TrimSpace(sb.String())
		if text == "" {
			return Output{Message: fmt.Sprintf("MCP resource %s returned %d content blocks", in.Args[2], len(result.Contents))}, nil
		}
		return Output{Message: text}, nil
	default:
		return Output{Message: "Usage: /mcp [list|connect|disconnect|read]"}, nil
	}
}

func (c *PluginCmd) Name() string        { return "plugin" }
func (c *PluginCmd) Aliases() []string   { return nil }
func (c *PluginCmd) Description() string { return "Manage plugins" }
func (c *PluginCmd) Help() string {
	return "/plugin [list|install|enable|disable|uninstall] - Manage plugins"
}
func (c *PluginCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.PluginManager == nil {
		return Output{Message: "Plugin manager unavailable"}, nil
	}
	action := "list"
	if len(in.Args) > 0 {
		action = in.Args[0]
	}
	switch action {
	case "list":
		if refresher, ok := in.PluginManager.(interface{ Refresh() }); ok {
			refresher.Refresh()
		}
		plugins := in.PluginManager.AllPlugins()
		if len(plugins) == 0 {
			dir := "~/.agentgo/plugins"
			if withDir, ok := in.PluginManager.(interface{ Dir() string }); ok {
				dir = withDir.Dir()
			}
			return Output{Message: fmt.Sprintf("No plugins installed\nPlugin dir: %s\nInstall: /plugin install <name> [url]", dir)}, nil
		}
		sort.Slice(plugins, func(i, j int) bool { return plugins[i].Manifest.Name < plugins[j].Manifest.Name })
		var sb strings.Builder
		sb.WriteString("Installed plugins:\n")
		for _, p := range plugins {
			sb.WriteString(fmt.Sprintf("- %s %s (%s)", p.Manifest.Name, p.Manifest.Version, pluginStateLabel(p.State)))
			if p.Error != "" {
				sb.WriteString(fmt.Sprintf(" - %s", p.Error))
			}
			sb.WriteString("\n")
		}
		return Output{Message: sb.String()}, nil
	case "install":
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /plugin install <name> [url]"}, nil
		}
		url := ""
		if len(in.Args) > 2 {
			url = in.Args[2]
		}
		if err := in.PluginManager.Install(in.Args[1], url); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Installed plugin: %s", in.Args[1])}, nil
	case "enable":
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /plugin enable <name>"}, nil
		}
		if err := in.PluginManager.Enable(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Enabled plugin: %s", in.Args[1])}, nil
	case "disable":
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /plugin disable <name>"}, nil
		}
		if err := in.PluginManager.Disable(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Disabled plugin: %s", in.Args[1])}, nil
	case "uninstall", "remove", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "Usage: /plugin uninstall <name>"}, nil
		}
		if err := in.PluginManager.Uninstall(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("Uninstalled plugin: %s", in.Args[1])}, nil
	default:
		return Output{Message: "Usage: /plugin [list|install|enable|disable|uninstall]"}, nil
	}
}

func (c *SkillsCmd) Name() string        { return "skills" }
func (c *SkillsCmd) Aliases() []string   { return []string{"skill"} }
func (c *SkillsCmd) Description() string { return "List and inspect available skills" }
func (c *SkillsCmd) Help() string {
	return "/skills [list|name] - List bundled and user-defined skills"
}
func (c *SkillsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SkillManager == nil {
		return Output{Message: "Skill manager unavailable"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		all := in.SkillManager.All()
		if len(all) == 0 {
			return Output{Message: "No skills loaded"}, nil
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
		var sb strings.Builder
		sb.WriteString("Skills:\n")
		for _, s := range all {
			sb.WriteString(fmt.Sprintf("- %-16s %s\n", s.Name, s.Description))
		}
		return Output{Message: sb.String()}, nil
	}
	name := in.Args[0]
	s, ok := in.SkillManager.Get(name)
	if !ok {
		return Output{Message: fmt.Sprintf("Skill '%s' not found", name)}, nil
	}
	return Output{Message: fmt.Sprintf("# %s\n\n%s", s.Name, s.Prompt)}, nil
}

func (c *PermissionsCmd) Name() string        { return "permissions" }
func (c *PermissionsCmd) Aliases() []string   { return nil }
func (c *PermissionsCmd) Description() string { return "Show permission mode" }
func (c *PermissionsCmd) Help() string        { return "/permissions - Show permission mode" }
func (c *PermissionsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.PermissionManager == nil {
		return Output{Message: "Permission manager unavailable"}, nil
	}
	return Output{Message: fmt.Sprintf("Permission mode: %s", in.PermissionManager.Mode())}, nil
}

func (c *StatusCmd) Name() string        { return "status" }
func (c *StatusCmd) Aliases() []string   { return nil }
func (c *StatusCmd) Description() string { return "Show agent status and session info" }
func (c *StatusCmd) Help() string        { return "/status - Show current agent state" }
func (c *StatusCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== Agent Status ===\n")
	sb.WriteString(fmt.Sprintf("CWD: %s\n", in.Cwd))
	if in.AppState != nil {
		if in.AppState.SessionID != "" {
			sb.WriteString(fmt.Sprintf("Session: %s\n", in.AppState.SessionID))
		}
		if in.AppState.Model != "" {
			sb.WriteString(fmt.Sprintf("Model: %s\n", in.AppState.Model))
		}
		if in.AppState.PermissionMode != "" {
			sb.WriteString(fmt.Sprintf("Mode: %s\n", in.AppState.PermissionMode))
		}
	}
	messageCount := 0
	costSummary := ""
	if in.Engine != nil {
		messageCount = len(in.Engine.Messages())
		if tracker := in.Engine.CostTracker(); tracker != nil {
			costSummary = strings.TrimSpace(tracker.Summary())
		}
	} else if in.AppState != nil {
		messageCount = in.AppState.Messages
	}
	sb.WriteString(fmt.Sprintf("Messages: %d\n", messageCount))
	if costSummary != "" {
		sb.WriteString(fmt.Sprintf("Cost: %s\n", costSummary))
	} else if in.AppState != nil {
		sb.WriteString(fmt.Sprintf("Budget: $%.2f / $%.2f\n", in.AppState.BudgetUsed, in.AppState.MaxBudget))
	}
	return Output{Message: sb.String()}, nil
}

func (c *StatsCmd) Name() string        { return "stats" }
func (c *StatsCmd) Aliases() []string   { return nil }
func (c *StatsCmd) Description() string { return "Show session statistics" }
func (c *StatsCmd) Help() string        { return "/stats - Token usage, cost, message count" }
func (c *StatsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "Session stats unavailable"}, nil
	}
	msgCount := len(in.Engine.Messages())
	return Output{Message: fmt.Sprintf("Messages: %d\nCost: %s", msgCount, in.Engine.CostTracker().Summary())}, nil
}

func (c *ExportCmd) Name() string        { return "export" }
func (c *ExportCmd) Aliases() []string   { return nil }
func (c *ExportCmd) Description() string { return "Export conversation to a file" }
func (c *ExportCmd) Help() string        { return "/export [filename] - Export chat history" }
func (c *ExportCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "Engine unavailable"}, nil
	}
	filename := filepath.Join(in.Cwd, "conversation.md")
	if len(in.Args) > 0 {
		filename = in.Args[0]
	}
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(in.Cwd, filename)
	}
	var sb strings.Builder
	sb.WriteString("# Conversation Export\n\n")
	for _, m := range in.Engine.Messages() {
		sb.WriteString(fmt.Sprintf("**%s**: %s\n\n", m.Role, m.Content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("- tool: %s %v\n", tc.Name, tc.Input))
		}
		if len(m.ToolCalls) > 0 {
			sb.WriteString("\n")
		}
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
		return Output{}, err
	}
	if err := os.WriteFile(filename, []byte(sb.String()), 0644); err != nil {
		return Output{}, err
	}
	return Output{Message: fmt.Sprintf("Exported %d messages to %s", len(in.Engine.Messages()), filename)}, nil
}

func (c *SystemCmd) Name() string        { return "system" }
func (c *SystemCmd) Aliases() []string   { return nil }
func (c *SystemCmd) Description() string { return "Show or set custom system prompt" }
func (c *SystemCmd) Help() string        { return "/system [prompt] - View or set custom system prompt" }
func (c *SystemCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Config == nil {
		return Output{Message: "Config unavailable"}, nil
	}
	if len(in.Args) == 0 {
		if in.Config.SystemPrompt == "" {
			return Output{Message: "No custom system prompt set"}, nil
		}
		return Output{Message: in.Config.SystemPrompt}, nil
	}
	prompt := strings.Join(in.Args, " ")
	in.Config.SystemPrompt = prompt
	if in.Engine != nil {
		in.Engine.SetSystemOverride(prompt)
	}
	if in.SaveConfig != nil {
		if err := in.SaveConfig(in.Config); err != nil {
			return Output{}, err
		}
	}
	return Output{Message: fmt.Sprintf("System prompt updated (%d chars)", len(prompt))}, nil
}

func (c *CdCmd) Name() string        { return "cd" }
func (c *CdCmd) Aliases() []string   { return nil }
func (c *CdCmd) Description() string { return "Change working directory" }
func (c *CdCmd) Help() string        { return "/cd <path> - Change current working directory" }
func (c *CdCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if len(in.Args) == 0 {
		return Output{Message: fmt.Sprintf("Current: %s", in.Cwd)}, nil
	}
	path := in.Args[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.Cwd, path)
	}
	if err := os.Chdir(path); err != nil {
		return Output{Message: fmt.Sprintf("Error: %v", err)}, nil
	}
	wd, _ := os.Getwd()
	if in.ProjectContext != nil {
		*in.ProjectContext = *ctxt.Collect()
	}
	return Output{Message: fmt.Sprintf("Changed to: %s", wd)}, nil
}

func (c *ContextCmd) Name() string        { return "context" }
func (c *ContextCmd) Aliases() []string   { return nil }
func (c *ContextCmd) Description() string { return "Show session context (git, files, budget)" }
func (c *ContextCmd) Help() string        { return "/context - Show git status and project context" }
func (c *ContextCmd) Execute(ctx context.Context, in Input) (Output, error) {
	pc := in.ProjectContext
	if pc == nil {
		collected := ctxt.Collect()
		pc = collected
	}
	var sb strings.Builder
	sb.WriteString("=== Session Context ===\n")
	sb.WriteString(fmt.Sprintf("CWD: %s\n", pc.Cwd))
	sb.WriteString(fmt.Sprintf("Platform: %s\n", pc.Platform))
	sb.WriteString(fmt.Sprintf("Shell: %s\n", pc.Shell))
	if pc.IsGitRepo {
		sb.WriteString(fmt.Sprintf("Git: %s (%s)\n", pc.GitBranch, pc.GitStatus))
	}
	if pc.FileTree != "" {
		sb.WriteString("\nProject structure:\n")
		sb.WriteString(pc.FileTree)
		if !strings.HasSuffix(pc.FileTree, "\n") {
			sb.WriteString("\n")
		}
	}
	return Output{Message: sb.String()}, nil
}

func configValue(cfg *config.Config, key string) string {
	switch key {
	case "model":
		return cfg.Model
	case "provider":
		return cfg.Provider.Name
	case "api_key", "api-key":
		if cfg.Provider.APIKey == "" {
			return ""
		}
		return "[REDACTED]"
	case "base_url", "base-url":
		return cfg.Provider.BaseURL
	case "mode", "permission_mode", "permission-mode":
		return cfg.PermissionMode
	case "budget", "max_budget_usd", "max-budget-usd":
		return fmt.Sprintf("%.2f", cfg.MaxBudgetUsd)
	case "system", "system_prompt", "system-prompt":
		return cfg.SystemPrompt
	default:
		return ""
	}
}

func applyConfigValue(cfg *config.Config, key, value string) error {
	switch key {
	case "model":
		cfg.Model = value
	case "provider":
		cfg.Provider.Name = value
	case "api_key", "api-key":
		cfg.Provider.APIKey = value
	case "base_url", "base-url":
		cfg.Provider.BaseURL = value
	case "mode", "permission_mode", "permission-mode":
		if !permission.ValidMode(permission.Mode(value)) {
			return fmt.Errorf("invalid permission mode: %s", value)
		}
		cfg.PermissionMode = value
	case "budget", "max_budget_usd", "max-budget-usd":
		var v float64
		if _, err := fmt.Sscanf(value, "%f", &v); err != nil {
			return fmt.Errorf("invalid budget: %w", err)
		}
		cfg.MaxBudgetUsd = v
	case "system", "system_prompt", "system-prompt":
		cfg.SystemPrompt = value
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func renderConfig(cfg *config.Config) string {
	pc := cfg.EffectiveProvider()
	data, _ := json.MarshalIndent(map[string]any{
		"model":           cfg.Model,
		"provider":        pc.Name,
		"base_url":        pc.BaseURL,
		"permission_mode": cfg.PermissionMode,
		"max_budget_usd":  cfg.MaxBudgetUsd,
		"thinking_tokens": cfg.ThinkingTokens,
		"debug":           cfg.Debug,
		"api_key_set":     pc.APIKey != "",
		"system_prompt":   cfg.SystemPrompt,
		"mcp_servers":     len(cfg.MCPServers),
	}, "", "  ")
	return string(data)
}

func pluginStateLabel(s plugin.State) string {
	switch s {
	case plugin.Enabled:
		return "enabled"
	case plugin.Disabled:
		return "disabled"
	case plugin.Error:
		return "error"
	default:
		return "unknown"
	}
}

var _ = memory.Entry{}
var _ = skills.Skill{}
