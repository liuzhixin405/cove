package command

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

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
