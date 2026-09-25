package command

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/fsatomic"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/onboarding"
)

// GuideGenerator is implemented by an EngineView that can make one
// tool-less model call (billed like any other, skipped when the budget is
// spent). /init uses it to draft CLAUDE.md; without it /init falls back to
// the static template.
type GuideGenerator interface {
	GenerateOnce(ctx context.Context, system, prompt string) (string, error)
}

// initGenerateTimeout bounds the /init drafting call.
const initGenerateTimeout = 3 * time.Minute

func (c *InitCmd) Name() string      { return "init" }
func (c *InitCmd) Aliases() []string { return nil }
func (c *InitCmd) Description() string {
	return "让模型阅读仓库并起草 CLAUDE.md（确认后写入）"
}
func (c *InitCmd) Help() string {
	return "/init [apply|discard] - 让模型阅读仓库并起草 CLAUDE.md，以 diff 展示；/init apply 写入，/init discard 放弃"
}

func (c *InitCmd) Execute(ctx context.Context, in Input) (Output, error) {
	cwd := in.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	obs := onboarding.Check(cwd)
	draftPath, err := initDraftPath(cwd)
	if err != nil {
		return Output{}, fmt.Errorf("init: %w", err)
	}

	sub := ""
	if len(in.Args) > 0 {
		sub = strings.ToLower(in.Args[0])
	}
	switch sub {
	case "apply", "yes", "y":
		return c.apply(obs, draftPath)
	case "discard", "no", "n":
		if err := os.Remove(draftPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Output{}, err
		}
		return Output{Message: "已放弃 CLAUDE.md 草稿。"}, nil
	case "":
	default:
		return Output{Message: "用法: " + c.Help()}, nil
	}

	if !obs.NeedsOnboarding() {
		return Output{Message: fmt.Sprintf("✓ CLAUDE.md 已存在。项目: %s", obs.Summary())}, nil
	}

	var sb strings.Builder
	if obs.HasAgentsMD {
		sb.WriteString("已检测到 AGENTS.md，将同时加载；下面的 CLAUDE.md 草稿只补充它没有写到的内容。\n")
	}
	draft, source := c.draft(ctx, in, obs)
	if err := fsatomic.WriteFile(draftPath, []byte(draft), 0o600); err != nil {
		return Output{}, fmt.Errorf("init: save draft: %w", err)
	}
	fmt.Fprintf(&sb, "CLAUDE.md 草稿（%s，检测到: %s）:\n\n", source, obs.Summary())
	sb.WriteString(onboarding.GuideDiff("", draft))
	sb.WriteString("\n确认写入请输入 /init apply，放弃请输入 /init discard。")
	return Output{Message: sb.String()}, nil
}

// draft asks the model for a CLAUDE.md, falling back to the template when no
// model is available or the call fails. It returns the content and a short
// Chinese note on where it came from.
func (c *InitCmd) draft(ctx context.Context, in Input, obs *onboarding.State) (string, string) {
	if gen, ok := in.Engine.(GuideGenerator); ok && gen != nil {
		gctx, cancel := context.WithTimeout(ctx, initGenerateTimeout)
		defer cancel()
		out, err := gen.GenerateOnce(gctx, onboarding.InitSystemPrompt, obs.InitPrompt())
		if content := cleanGuide(out); err == nil && content != "" {
			return content, "由模型阅读仓库后生成"
		} else if err != nil {
			return obs.GenerateClaudeMD(), fmt.Sprintf("模型调用失败（%v），使用模板", err)
		}
	}
	return obs.GenerateClaudeMD(), "模型不可用，使用模板"
}

func (c *InitCmd) apply(obs *onboarding.State, draftPath string) (Output, error) {
	data, err := os.ReadFile(draftPath)
	if errors.Is(err, fs.ErrNotExist) {
		return Output{Message: "没有待确认的 CLAUDE.md 草稿，先运行 /init。"}, nil
	}
	if err != nil {
		return Output{}, err
	}
	path, err := obs.WriteGuide(string(data))
	if err != nil {
		return Output{}, fmt.Errorf("init failed: %w", err)
	}
	_ = os.Remove(draftPath)
	if path == "" {
		return Output{Message: "CLAUDE.md 已存在，未覆盖；草稿已丢弃。"}, nil
	}
	return Output{Message: fmt.Sprintf("✓ 已写入 %s\n  下一轮对话起生效（压缩或 /clear 后刷新系统提示词）。", path)}, nil
}

// cleanGuide strips a code fence the model wrapped the whole file in.
func cleanGuide(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	if s == "" {
		return ""
	}
	return s + "\n"
}

// initDraftPath is where /init keeps the draft between the preview and
// /init apply: the project's data directory, not the repository.
func initDraftPath(cwd string) (string, error) {
	dir, err := config.ProjectDataDir(memory.ProjectRoot(cwd))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "init-CLAUDE.md.draft"), nil
}
