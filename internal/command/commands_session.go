package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ctxt "github.com/liuzhixin405/cove/internal/context"
)

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
	if pc.RepoMap != "" {
		sb.WriteString("\n代码大纲地图 (Repo Map):\n")
		sb.WriteString(pc.RepoMap)
		if !strings.HasSuffix(pc.RepoMap, "\n") {
			sb.WriteString("\n")
		}
	}
	return Output{Message: sb.String()}, nil
}
