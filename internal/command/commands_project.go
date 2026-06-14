package command

import (
	"context"
	"fmt"
	"os"

	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/onboarding"
)

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
