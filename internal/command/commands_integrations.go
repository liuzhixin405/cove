package command

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/mcp"
	"github.com/liuzhixin405/cove/internal/plugin"
)

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
		if strings.HasPrefix(name, "https://") || strings.HasPrefix(name, "git@") {
			url = name
			parts := strings.Split(strings.TrimSuffix(url, ".git"), "/")
			name = parts[len(parts)-1]
		} else if at := strings.IndexByte(name, '@'); at > 0 {
			name = name[:at]
		}

		if url == "" {
			type marketInstaller interface {
				MarketplaceInstall(name string) error
			}
			if mi, ok := in.PluginManager.(marketInstaller); ok {
				if err := mi.MarketplaceInstall(name); err == nil {
					return Output{Message: fmt.Sprintf("✓ 已从 marketplace 安装: %s", name)}, nil
				} else {
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
