package command

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// gitOutput runs git in dir and returns its stdout. A failure carries git's
// own stderr: the commands used to discard it, so outside a repository (or
// without git on PATH) /commit, /review and /diff all reported "no changes".
func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(strings.TrimSpace(string(ee.Stderr))) > 0 {
			return string(out), fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out), nil
}

func (c *CommitCmd) Name() string        { return "commit" }
func (c *CommitCmd) Aliases() []string   { return nil }
func (c *CommitCmd) Description() string { return "暂存并创建 git 提交" }
func (c *CommitCmd) Help() string        { return "/commit [消息] - 暂存所有更改并提交" }
func (c *CommitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	so, err := gitOutput(ctx, in.Cwd, "status", "--porcelain")
	if err != nil {
		return Output{Message: fmt.Sprintf("无法读取 git 状态: %v", err)}, nil
	}
	if strings.TrimSpace(so) == "" {
		return Output{Message: "没有可提交的更改"}, nil
	}
	msg := "auto-commit"
	if len(in.Args) > 0 {
		msg = strings.Join(in.Args, " ")
	}
	// A failed add used to be ignored, and the commit that followed either
	// failed with an unrelated message or committed only part of the tree.
	ac := exec.CommandContext(ctx, "git", "add", "-A")
	ac.Dir = in.Cwd
	if ao, err := ac.CombinedOutput(); err != nil {
		return Output{Message: fmt.Sprintf("git add 失败: %s", strings.TrimSpace(string(ao)))}, nil
	}
	cc := exec.CommandContext(ctx, "git", "commit", "-m", msg)
	cc.Dir = in.Cwd
	co, err := cc.CombinedOutput()
	if err != nil {
		return Output{Message: fmt.Sprintf("提交失败: %s", strings.TrimSpace(string(co))), Data: so}, nil
	}
	return Output{Message: fmt.Sprintf("已提交: %s", msg), Data: string(co)}, nil
}

func (c *ReviewCmd) Name() string        { return "review" }
func (c *ReviewCmd) Aliases() []string   { return nil }
func (c *ReviewCmd) Description() string { return "审查工作区更改" }
func (c *ReviewCmd) Help() string        { return "/review - 显示并分析未提交的更改" }
func (c *ReviewCmd) Execute(ctx context.Context, in Input) (Output, error) {
	out, err := gitOutput(ctx, in.Cwd, "diff", "--stat")
	if err != nil {
		return Output{Message: fmt.Sprintf("无法读取 git 差异: %v", err)}, nil
	}
	diff := strings.TrimSpace(out)
	if diff == "" {
		out, err = gitOutput(ctx, in.Cwd, "diff", "--cached", "--stat")
		if err != nil {
			return Output{Message: fmt.Sprintf("无法读取 git 差异: %v", err)}, nil
		}
		diff = strings.TrimSpace(out)
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
	unstaged, err := gitOutput(ctx, in.Cwd, "diff")
	if err != nil {
		return Output{Message: fmt.Sprintf("无法读取 git 差异: %v", err)}, nil
	}
	staged, err := gitOutput(ctx, in.Cwd, "diff", "--cached")
	if err != nil {
		return Output{Message: fmt.Sprintf("无法读取 git 差异: %v", err)}, nil
	}
	// Both halves are shown: showing the staged diff only when the unstaged
	// one was empty hid staged changes exactly when there were both kinds.
	hasStaged, hasUnstaged := strings.TrimSpace(staged) != "", strings.TrimSpace(unstaged) != ""
	switch {
	case !hasStaged && !hasUnstaged:
		return Output{Message: "无差异"}, nil
	case hasStaged && hasUnstaged:
		return Output{Data: "# 已暂存 (staged)\n" + staged + "\n# 未暂存 (unstaged)\n" + unstaged}, nil
	case hasStaged:
		return Output{Data: staged}, nil
	default:
		return Output{Data: unstaged}, nil
	}
}
