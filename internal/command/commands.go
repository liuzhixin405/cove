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

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/onboarding"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/plugin"
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
type DreamCmd struct{}

type CdCmd struct{}
type ContextCmd struct{}

type PermissionsCmd struct{}
type StatusCmd struct{}
type StatsCmd struct{}
type InitCmd struct{}

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
func NewDreamCmd() Command       { return &DreamCmd{} }
func NewContextCmd() Command     { return &ContextCmd{} }
func NewPermissionsCmd() Command { return &PermissionsCmd{} }
func NewStatusCmd() Command      { return &StatusCmd{} }
func NewStatsCmd() Command       { return &StatsCmd{} }
func NewInitCmd() Command        { return &InitCmd{} }

func (c *CommitCmd) Name() string        { return "commit" }
func (c *CommitCmd) Aliases() []string   { return nil }
func (c *CommitCmd) Description() string { return "暂存并创建 git 提交" }
func (c *CommitCmd) Help() string        { return "/commit [消息] - 暂存所有更改并提交" }
func (c *CommitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	sc := exec.CommandContext(ctx, "git", "status", "--porcelain")
	sc.Dir = in.Cwd
	so, _ := sc.Output()
	if strings.TrimSpace(string(so)) == "" {
		return Output{Message: "没有可提交的更改"}, nil
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
		return Output{Message: fmt.Sprintf("提交失败: %s", strings.TrimSpace(string(co))), Data: string(so)}, nil
	}
	return Output{Message: fmt.Sprintf("已提交: %s", msg), Data: string(co)}, nil
}

func (c *ReviewCmd) Name() string        { return "review" }
func (c *ReviewCmd) Aliases() []string   { return nil }
func (c *ReviewCmd) Description() string { return "审查工作区更改" }
func (c *ReviewCmd) Help() string        { return "/review - 显示并分析未提交的更改" }
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
		return Output{Message: "没有需要审查的更改"}, nil
	}
	return Output{Message: "变更文件:\n" + diff}, nil
}

func (c *DoctorCmd) Name() string        { return "doctor" }
func (c *DoctorCmd) Aliases() []string   { return nil }
func (c *DoctorCmd) Description() string { return "系统诊断" }
func (c *DoctorCmd) Help() string        { return "/doctor - 检查 Go、git、ripgrep、配置" }
func (c *DoctorCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== 系统诊断 ===\n")
	sb.WriteString(fmt.Sprintf("目录: %s\n", in.Cwd))
	if g, err := exec.LookPath("git"); err == nil {
		sb.WriteString(fmt.Sprintf("Git: %s\n", g))
	} else {
		sb.WriteString("Git: 未找到\n")
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		sb.WriteString(fmt.Sprintf("Ripgrep: %s\n", rg))
	} else {
		sb.WriteString("Ripgrep: 未找到\n")
	}
	sb.WriteString(fmt.Sprintf("时间: %s\n", time.Now().Format(time.RFC3339)))
	return Output{Message: sb.String()}, nil
}

func (c *ConfigCmd) Name() string        { return "config" }
func (c *ConfigCmd) Aliases() []string   { return nil }
func (c *ConfigCmd) Description() string { return "查看或修改配置" }
func (c *ConfigCmd) Help() string        { return "/config [键] [值] - 查看/设置配置" }
func (c *ConfigCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cfg := in.Config
	if cfg == nil {
		return Output{Message: "配置不可用"}, nil
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
	return Output{Message: fmt.Sprintf("已保存 %s = %s", key, configValue(cfg, key))}, nil
}

func (c *CompactCmd) Name() string        { return "compact" }
func (c *CompactCmd) Aliases() []string   { return nil }
func (c *CompactCmd) Description() string { return "压缩对话历史" }
func (c *CompactCmd) Help() string {
	return "/compact - 总结早期消息以释放上下文窗口"
}
func (c *CompactCmd) Execute(ctx context.Context, in Input) (Output, error) {
	return Output{Message: "请从 REPL 内置路径使用 /compact。"}, nil
}

func (c *CostCmd) Name() string        { return "cost" }
func (c *CostCmd) Aliases() []string   { return nil }
func (c *CostCmd) Description() string { return "查看用量和费用" }
func (c *CostCmd) Help() string        { return "/cost - 显示会话 token 用量和预估费用" }
func (c *CostCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "费用跟踪器不可用"}, nil
	}
	return Output{Message: in.Engine.CostTracker().Summary()}, nil
}

func (c *DiffCmd) Name() string        { return "diff" }
func (c *DiffCmd) Aliases() []string   { return nil }
func (c *DiffCmd) Description() string { return "显示 git diff" }
func (c *DiffCmd) Help() string        { return "/diff - 显示工作区差异" }
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
		return Output{Message: "无差异"}, nil
	}
	return Output{Data: string(out)}, nil
}

func (c *MemoryCmd) Name() string        { return "memory" }
func (c *MemoryCmd) Aliases() []string   { return nil }
func (c *MemoryCmd) Description() string { return "管理持久化记忆" }
func (c *MemoryCmd) Help() string {
	return "/memory [list|add|remove|search <关键词>|stats] - 管理持久记忆文件"
}
func (c *MemoryCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MemoryStore == nil {
		return Output{Message: "记忆存储不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		entries := in.MemoryStore.All()
		if len(entries) == 0 {
			return Output{Message: "暂无记忆文件"}, nil
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		var sb strings.Builder
		sb.WriteString("记忆文件:\n")
		for _, e := range entries {
			sb.WriteString(fmt.Sprintf("- %s\n", e.Name))
		}
		return Output{Message: sb.String()}, nil
	}
	switch in.Args[0] {
	case "add":
		if len(in.Args) < 3 {
			return Output{Message: "用法: /memory add <名称> <内容>"}, nil
		}
		name := in.Args[1]
		content := strings.Join(in.Args[2:], " ")
		if err := in.MemoryStore.Save(name, content); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("记忆 '%s' 已保存", name)}, nil
	case "remove", "delete", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /memory remove <名称>"}, nil
		}
		if err := in.MemoryStore.Delete(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("记忆 '%s' 已删除", in.Args[1])}, nil
	case "search", "find":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /memory search <关键词>"}, nil
		}
		query := strings.Join(in.Args[1:], " ")
		results := in.MemoryStore.Search(query, 5)
		if len(results) == 0 {
			return Output{Message: fmt.Sprintf("未找到与 %q 相关的记忆", query)}, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("与 %q 相关的记忆 (BM25 关键词检索):\n", query))
		for _, r := range results {
			marker := ""
			if r.Entry.Project {
				marker = " (项目)"
			}
			preview := strings.ReplaceAll(r.Entry.Content, "\n", " ")
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			sb.WriteString(fmt.Sprintf("  %s%s [%.2f]: %s\n", r.Entry.Name, marker, r.Score, preview))
		}
		return Output{Message: sb.String()}, nil
	case "stats", "stat":
		st := in.MemoryStore.Stats()
		var sb strings.Builder
		sb.WriteString("记忆统计:\n")
		sb.WriteString(fmt.Sprintf("  文件数:   %d (其中项目记忆 %d)\n", st.FileCount, st.ProjectCount))
		sb.WriteString(fmt.Sprintf("  总行数:   %d\n", st.TotalLines))
		sb.WriteString(fmt.Sprintf("  总大小:   %s / %s\n", humanBytes(st.TotalBytes), humanBytes(st.MaxTotalBytes)))
		if st.MaxTotalBytes > 0 {
			sb.WriteString(fmt.Sprintf("  使用率:   %.1f%%\n", float64(st.TotalBytes)*100/float64(st.MaxTotalBytes)))
		}
		sb.WriteString(fmt.Sprintf("  单条上限: %s\n", humanBytes(st.MaxEntryBytes)))
		return Output{Message: sb.String()}, nil
	default:
		return Output{Message: "用法: /memory [list|add|remove|search <关键词>|stats]"}, nil
	}
}

func humanBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%.1fMB", float64(n)/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func (c *ResumeCmd) Name() string        { return "resume" }
func (c *ResumeCmd) Aliases() []string   { return nil }
func (c *ResumeCmd) Description() string { return "恢复已保存的会话" }
func (c *ResumeCmd) Help() string        { return "/resume [session-id] - 列出或恢复已保存的会话" }
func (c *ResumeCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SessionStore == nil {
		return Output{Message: "会话存储不可用"}, nil
	}
	if len(in.Args) == 0 {
		records, err := in.SessionStore.List()
		if err != nil {
			return Output{}, err
		}
		if len(records) == 0 {
			return Output{Message: "暂无已保存的会话"}, nil
		}
		var sb strings.Builder
		sb.WriteString("已保存的会话:\n")
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
	return Output{Message: fmt.Sprintf("已恢复: %s (%d 条消息, %d tokens)", r.Title, len(r.Messages), r.TokensIn+r.TokensOut)}, nil
}

func (c *McpCmd) Name() string        { return "mcp" }
func (c *McpCmd) Aliases() []string   { return nil }
func (c *McpCmd) Description() string { return "管理 MCP 服务器" }
func (c *McpCmd) Help() string        { return "/mcp [list|connect|disconnect|read] - 管理 MCP 服务器" }
func (c *McpCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.MCPPool == nil {
		return Output{Message: "MCP 连接池不可用"}, nil
	}
	action := "list"
	if len(in.Args) > 0 {
		action = in.Args[0]
	}
	switch action {
	case "list":
		servers := in.MCPPool.AllServers()
		if len(servers) == 0 {
			return Output{Message: "暂无已连接的 MCP 服务器"}, nil
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
			return Output{Message: "用法: /mcp disconnect <名称|all>"}, nil
		}
		if in.Args[1] == "all" {
			in.MCPPool.DisconnectAll()
			return Output{Message: "已断开所有 MCP 服务器"}, nil
		}
		in.MCPPool.Disconnect(in.Args[1])
		return Output{Message: fmt.Sprintf("已断开 MCP 服务器: %s", in.Args[1])}, nil
	case "connect":
		if in.Config == nil {
			return Output{Message: "配置不可用"}, nil
		}
		if len(in.Args) < 2 {
			return Output{Message: "用法: /mcp connect <名称>"}, nil
		}
		cfg, ok := in.Config.MCPServers[in.Args[1]]
		if !ok {
			return Output{Message: fmt.Sprintf("配置中未找到 MCP 服务器 '%s'", in.Args[1])}, nil
		}
		if err := in.MCPPool.Connect(ctx, in.Args[1], mcp.ServerConfig(cfg)); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已连接 MCP 服务器: %s", in.Args[1])}, nil
	case "read":
		if len(in.Args) < 3 {
			return Output{Message: "用法: /mcp read <服务器> <资源URI>"}, nil
		}
		result, err := in.MCPPool.ReadResource(ctx, in.Args[1], in.Args[2])
		if err != nil {
			return Output{}, err
		}
		if result == nil || len(result.Contents) == 0 {
			return Output{Message: fmt.Sprintf("MCP 资源无内容: %s", in.Args[2])}, nil
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
			return Output{Message: fmt.Sprintf("MCP 资源 %s 返回了 %d 个内容块", in.Args[2], len(result.Contents))}, nil
		}
		return Output{Message: text}, nil
	default:
		return Output{Message: "用法: /mcp [list|connect|disconnect|read]"}, nil
	}
}

func (c *PluginCmd) Name() string        { return "plugin" }
func (c *PluginCmd) Aliases() []string   { return nil }
func (c *PluginCmd) Description() string { return "管理插件" }
func (c *PluginCmd) Help() string {
	return "/plugin [list|install|search|refresh|update|enable|disable|uninstall] - 管理插件"
}
func (c *PluginCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.PluginManager == nil {
		return Output{Message: "插件管理器不可用"}, nil
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
			dir := "~/.cove/plugins"
			if withDir, ok := in.PluginManager.(interface{ Dir() string }); ok {
				dir = withDir.Dir()
			}
			return Output{Message: fmt.Sprintf("暂无已安装插件\n插件目录: %s\n安装: /plugin install <名称> [url]", dir)}, nil
		}
		sort.Slice(plugins, func(i, j int) bool { return plugins[i].Manifest.Name < plugins[j].Manifest.Name })
		var sb strings.Builder
		sb.WriteString("已安装插件:\n")
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
			return Output{Message: "用法: /plugin install <名称|git-url>"}, nil
		}
		name := in.Args[1]
		url := ""
		if len(in.Args) > 2 {
			url = in.Args[2]
		}

		// If name looks like a git URL, install directly from it
		if strings.HasPrefix(name, "https://") || strings.HasPrefix(name, "git@") {
			url = name
			// Derive name from URL
			parts := strings.Split(strings.TrimSuffix(url, ".git"), "/")
			name = parts[len(parts)-1]
		} else if at := strings.IndexByte(name, '@'); at > 0 {
			// Claude-style "plugin@marketplace" reference: keep only the plugin
			// name. The marketplace source is resolved from the local index.
			name = name[:at]
		}

		// Try marketplace install first (if no explicit URL and marketplace is available)
		if url == "" {
			type marketInstaller interface {
				MarketplaceInstall(name string) error
			}
			if mi, ok := in.PluginManager.(marketInstaller); ok {
				if err := mi.MarketplaceInstall(name); err == nil {
					return Output{Message: fmt.Sprintf("✓ 已从 marketplace 安装: %s", name)}, nil
				} else {
					// Not found in marketplace. Do NOT silently scaffold an empty
					// plugin (that produces an installed-but-useless entry). Tell
					// the user how to install from a git URL instead.
					return Output{Message: fmt.Sprintf(
						"无法从 marketplace 安装 %q: %v\n如果你知道插件仓库地址，用: /plugin install %s <git-url>\n或先刷新索引: /plugin refresh",
						name, err, name)}, nil
				}
			}
		}

		if err := in.PluginManager.Install(name, url); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已安装插件: %s", name)}, nil
	case "enable":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin enable <名称>"}, nil
		}
		if err := in.PluginManager.Enable(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已启用插件: %s", in.Args[1])}, nil
	case "disable":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin disable <名称>"}, nil
		}
		if err := in.PluginManager.Disable(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已禁用插件: %s", in.Args[1])}, nil
	case "uninstall", "remove", "rm":
		if len(in.Args) < 2 {
			return Output{Message: "用法: /plugin uninstall <名称>"}, nil
		}
		if err := in.PluginManager.Uninstall(in.Args[1]); err != nil {
			return Output{}, err
		}
		return Output{Message: fmt.Sprintf("已卸载插件: %s", in.Args[1])}, nil
	case "search", "find":
		query := ""
		if len(in.Args) > 1 {
			query = strings.Join(in.Args[1:], " ")
		}
		type marketSearcher interface {
			MarketplaceSearch(query string) string
		}
		if ms, ok := in.PluginManager.(marketSearcher); ok {
			return Output{Message: ms.MarketplaceSearch(query)}, nil
		}
		return Output{Message: "marketplace 不可用"}, nil
	case "refresh":
		type marketRefresher interface {
			MarketplaceRefresh() error
		}
		if mr, ok := in.PluginManager.(marketRefresher); ok {
			if err := mr.MarketplaceRefresh(); err != nil {
				return Output{Message: fmt.Sprintf("刷新marketplace索引时部分失败: %v", err)}, nil
			}
			return Output{Message: "✓ marketplace 索引已更新"}, nil
		}
		return Output{Message: "marketplace 不可用"}, nil
	case "update":
		name := ""
		if len(in.Args) > 1 {
			name = in.Args[1]
		}
		type marketUpdater interface {
			MarketplaceUpdate(name string) (string, error)
		}
		if mu, ok := in.PluginManager.(marketUpdater); ok {
			msg, err := mu.MarketplaceUpdate(name)
			if err != nil {
				return Output{}, err
			}
			return Output{Message: msg}, nil
		}
		return Output{Message: "marketplace 不可用"}, nil
	default:
		return Output{Message: "用法: /plugin [list|install|search|refresh|update|enable|disable|uninstall]"}, nil
	}
}

func (c *SkillsCmd) Name() string        { return "skills" }
func (c *SkillsCmd) Aliases() []string   { return []string{"skill"} }
func (c *SkillsCmd) Description() string { return "查看可用技能" }
func (c *SkillsCmd) Help() string {
	return "/skills [list|名称] - 列出内置和用户定义的技能"
}
func (c *SkillsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.SkillManager == nil {
		return Output{Message: "技能管理器不可用"}, nil
	}
	if len(in.Args) == 0 || in.Args[0] == "list" {
		all := in.SkillManager.All()
		if len(all) == 0 {
			return Output{Message: "暂无已加载的技能"}, nil
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
		var sb strings.Builder
		sb.WriteString("技能列表:\n")
		for _, s := range all {
			sb.WriteString(fmt.Sprintf("- %-16s %s\n", s.Name, s.Description))
		}
		return Output{Message: sb.String()}, nil
	}
	name := in.Args[0]
	s, ok := in.SkillManager.Get(name)
	if !ok {
		return Output{Message: fmt.Sprintf("未找到技能 '%s'", name)}, nil
	}
	return Output{Message: fmt.Sprintf("# %s\n\n%s", s.Name, s.Prompt)}, nil
}

func (c *PermissionsCmd) Name() string        { return "permissions" }
func (c *PermissionsCmd) Aliases() []string   { return nil }
func (c *PermissionsCmd) Description() string { return "查看权限模式" }
func (c *PermissionsCmd) Help() string        { return "/permissions - 查看权限模式" }
func (c *PermissionsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.PermissionManager == nil {
		return Output{Message: "权限管理器不可用"}, nil
	}
	return Output{Message: fmt.Sprintf("权限模式: %s", in.PermissionManager.Mode())}, nil
}

func (c *StatusCmd) Name() string        { return "status" }
func (c *StatusCmd) Aliases() []string   { return nil }
func (c *StatusCmd) Description() string { return "查看代理状态和会话信息" }
func (c *StatusCmd) Help() string        { return "/status - 查看当前代理状态" }
func (c *StatusCmd) Execute(ctx context.Context, in Input) (Output, error) {
	var sb strings.Builder
	sb.WriteString("=== 代理状态 ===\n")
	sb.WriteString(fmt.Sprintf("目录: %s\n", in.Cwd))
	if in.AppState != nil {
		if in.AppState.SessionID != "" {
			sb.WriteString(fmt.Sprintf("会话: %s\n", in.AppState.SessionID))
		}
		if in.AppState.Model != "" {
			sb.WriteString(fmt.Sprintf("模型: %s\n", in.AppState.Model))
		}
		if in.AppState.PermissionMode != "" {
			sb.WriteString(fmt.Sprintf("模式: %s\n", in.AppState.PermissionMode))
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
	sb.WriteString(fmt.Sprintf("消息数: %d\n", messageCount))
	if costSummary != "" {
		sb.WriteString(fmt.Sprintf("费用: %s\n", costSummary))
	} else if in.AppState != nil {
		sb.WriteString(fmt.Sprintf("预算: $%.2f / $%.2f\n", in.AppState.BudgetUsed, in.AppState.MaxBudget))
	}
	return Output{Message: sb.String()}, nil
}

func (c *StatsCmd) Name() string        { return "stats" }
func (c *StatsCmd) Aliases() []string   { return nil }
func (c *StatsCmd) Description() string { return "查看会话统计" }
func (c *StatsCmd) Help() string        { return "/stats - 用量、费用、消息数" }
func (c *StatsCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "会话统计不可用"}, nil
	}
	msgCount := len(in.Engine.Messages())
	return Output{Message: fmt.Sprintf("消息数: %d\n费用: %s", msgCount, in.Engine.CostTracker().Summary())}, nil
}

func (c *ExportCmd) Name() string        { return "export" }
func (c *ExportCmd) Aliases() []string   { return nil }
func (c *ExportCmd) Description() string { return "导出对话到文件" }
func (c *ExportCmd) Help() string        { return "/export [文件名] - 导出聊天记录" }
func (c *ExportCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Engine == nil {
		return Output{Message: "引擎不可用"}, nil
	}
	filename := filepath.Join(in.Cwd, "conversation.md")
	if len(in.Args) > 0 {
		filename = in.Args[0]
	}
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(in.Cwd, filename)
	}
	var sb strings.Builder
	sb.WriteString("# 对话导出\n\n")
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
	return Output{Message: fmt.Sprintf("已导出 %d 条消息到 %s", len(in.Engine.Messages()), filename)}, nil
}

func (c *SystemCmd) Name() string        { return "system" }
func (c *SystemCmd) Aliases() []string   { return nil }
func (c *SystemCmd) Description() string { return "查看或设置自定义系统提示词" }
func (c *SystemCmd) Help() string {
	return "/system [提示词] - 查看或设置自定义系统提示词"
}
func (c *SystemCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if in.Config == nil {
		return Output{Message: "配置不可用"}, nil
	}
	if len(in.Args) == 0 {
		if in.Config.SystemPrompt == "" {
			return Output{Message: "未设置自定义系统提示词"}, nil
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
	return Output{Message: fmt.Sprintf("系统提示词已更新 (%d 字符)", len(prompt))}, nil
}

func (c *CdCmd) Name() string        { return "cd" }
func (c *CdCmd) Aliases() []string   { return nil }
func (c *CdCmd) Description() string { return "切换工作目录" }
func (c *CdCmd) Help() string        { return "/cd <路径> - 切换当前工作目录" }
func (c *CdCmd) Execute(ctx context.Context, in Input) (Output, error) {
	if len(in.Args) == 0 {
		return Output{Message: fmt.Sprintf("当前: %s", in.Cwd)}, nil
	}
	path := in.Args[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(in.Cwd, path)
	}
	if err := os.Chdir(path); err != nil {
		return Output{Message: fmt.Sprintf("错误: %v", err)}, nil
	}
	wd, _ := os.Getwd()
	if in.ProjectContext != nil {
		*in.ProjectContext = *ctxt.Collect()
	}
	return Output{Message: fmt.Sprintf("已切换到: %s", wd)}, nil
}

func (c *ContextCmd) Name() string        { return "context" }
func (c *ContextCmd) Aliases() []string   { return nil }
func (c *ContextCmd) Description() string { return "查看会话上下文 (git/文件/预算)" }
func (c *ContextCmd) Help() string        { return "/context - 查看 git 状态和项目上下文" }
func (c *ContextCmd) Execute(ctx context.Context, in Input) (Output, error) {
	pc := in.ProjectContext
	if pc == nil {
		collected := ctxt.Collect()
		pc = collected
	}
	var sb strings.Builder
	sb.WriteString("=== 会话上下文 ===\n")
	sb.WriteString(fmt.Sprintf("目录: %s\n", pc.Cwd))
	sb.WriteString(fmt.Sprintf("平台: %s\n", pc.Platform))
	sb.WriteString(fmt.Sprintf("Shell: %s\n", pc.Shell))
	if pc.IsGitRepo {
		sb.WriteString(fmt.Sprintf("Git: %s (%s)\n", pc.GitBranch, pc.GitStatus))
	}
	if pc.FileTree != "" {
		sb.WriteString("\n项目结构:\n")
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
		return "已启用"
	case plugin.Disabled:
		return "已禁用"
	case plugin.Error:
		return "错误"
	default:
		return "未知"
	}
}

func recentFailureClues(msgs []api.Message, limit int) []string {
	if limit <= 0 || len(msgs) == 0 {
		return nil
	}

	out := make([]string, 0, limit)
	seen := make(map[string]bool, limit)

	for i := len(msgs) - 1; i >= 0 && len(out) < limit; i-- {
		m := msgs[i]
		if m.Content == "" {
			continue
		}
		if !looksLikeFailureMessage(m) {
			continue
		}
		clue := summarizeFailureClue(m)
		if clue == "" || seen[clue] {
			continue
		}
		seen[clue] = true
		out = append(out, clue)
	}

	return out
}

func looksLikeFailureMessage(m api.Message) bool {
	text := strings.ToLower(strings.TrimSpace(m.Content))
	if text == "" {
		return false
	}
	if m.Role == "tool" && strings.HasPrefix(text, "error:") {
		return true
	}
	keywords := []string{
		"error:", "failed", "failure", "panic", "exception", "traceback",
		"timeout", "timed out", "denied", "forbidden", "not found", "no such",
		"invalid", "unable to", "cannot",
		"错误", "失败", "异常", "超时", "未找到", "无效", "拒绝",
	}
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			return true
		}
	}
	return false
}

func summarizeFailureClue(m api.Message) string {
	line := firstNonEmptyLine(m.Content)
	if line == "" {
		return ""
	}
	line = strings.TrimSpace(line)
	line = trimRunes(line, 140)

	source := ""
	if m.Role == "tool" {
		if m.Name != "" {
			source = fmt.Sprintf("[%s] ", m.Name)
		} else {
			source = "[tool] "
		}
	}
	return source + line
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func trimRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

func failureActionHint(clues []string) string {
	if len(clues) == 0 {
		return "先执行 /doctor 验证环境，再排查具体错误信息。"
	}
	text := strings.ToLower(strings.Join(clues, " | "))
	switch {
	case strings.Contains(text, "denied") || strings.Contains(text, "forbidden") || strings.Contains(text, "拒绝"):
		return "先检查权限与模式（/permissions、/mode），确认工具是否被拦截。"
	case strings.Contains(text, "not found") || strings.Contains(text, "no such") || strings.Contains(text, "未找到"):
		return "优先核对路径/文件名，再用 grep 或 ls 验证目标是否存在。"
	case strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || strings.Contains(text, "超时"):
		return "先缩小输入范围并减少并发，再重试一次确认是否稳定复现。"
	case strings.Contains(text, "invalid") || strings.Contains(text, "schema") || strings.Contains(text, "无效"):
		return "先对照命令/工具参数定义，逐项校验必填字段与格式。"
	case strings.Contains(text, "panic") || strings.Contains(text, "nil"):
		return "先抓首个栈帧位置，加空值保护并补最小回归测试。"
	default:
		return "按“最小复现 -> 增加观测 -> 二分排查 -> 修复回归”顺序推进。"
	}
}

func countGitStatusLines(status string) int {
	s := strings.TrimSpace(status)
	if s == "" || s == "(clean)" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// --- Init Command ---

func (c *InitCmd) Name() string        { return "init" }
func (c *InitCmd) Aliases() []string   { return nil }
func (c *InitCmd) Description() string { return "初始化项目并创建 CLAUDE.md" }
func (c *InitCmd) Help() string        { return "/init - 检测项目结构并创建 CLAUDE.md" }
func (c *InitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	obs := onboarding.Check(cwd)
	if !obs.NeedsOnboarding() {
		return Output{Message: fmt.Sprintf("✓ CLAUDE.md 已存在。项目: %s", obs.Summary())}, nil
	}

	path, err := obs.InitProject()
	if err != nil {
		return Output{}, fmt.Errorf("init failed: %w", err)
	}

	return Output{
		Message: fmt.Sprintf("✓ 已创建 %s\n  检测到: %s\n  编辑此文件可自定义项目约定。", path, obs.Summary()),
	}, nil
}

// --- DreamCmd ---

func (c *DreamCmd) Name() string        { return "dream" }
func (c *DreamCmd) Aliases() []string   { return nil }
func (c *DreamCmd) Description() string { return "运行记忆整理 (梦境)" }
func (c *DreamCmd) Help() string {
	return "/dream - 手动触发记忆整理。回顾近期会话并整理记忆文件。"
}
func (c *DreamCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cfg := dream.LoadConfig()
	if !cfg.Enabled {
		return Output{Message: "自动梦境已禁用。请在 ~/.cove/dream.json 中启用。"}, nil
	}
	if task := dream.ActiveTask(); task != nil {
		return Output{Message: fmt.Sprintf("梦境已在运行中 (开始于 %s)", task.StartTime.Format("15:04:05"))}, nil
	}
	if err := dream.RecordConsolidation(); err != nil {
		return Output{Message: fmt.Sprintf("记录整理失败: %v", err)}, nil
	}
	return Output{Message: "梦境已触发。记忆整理将在后台运行。"}, nil
}
