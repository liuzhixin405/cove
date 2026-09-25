package command

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/hooks"
)

type hooksCmd struct{}

// NewHooksCmd is /hooks: it lists the hooks loaded from the user-level
// hooks.json (the only source cove reads hooks from).
func NewHooksCmd() Command { return &hooksCmd{} }

func (c *hooksCmd) Name() string        { return "hooks" }
func (c *hooksCmd) Aliases() []string   { return nil }
func (c *hooksCmd) Description() string { return "列出已加载的钩子（hooks.json）" }
func (c *hooksCmd) Help() string {
	return "/hooks - 列出从用户级 hooks.json 加载的钩子（事件、匹配工具、命令、超时）"
}

func (c *hooksCmd) Execute(ctx context.Context, in Input) (Output, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return Output{}, err
	}
	defs, loadErr := hooks.LoadConfigDir(dir)
	var sb strings.Builder
	if len(defs) == 0 {
		fmt.Fprintf(&sb, "没有已加载的钩子（配置文件：%s）\n", dirHooksPath(dir))
	} else {
		fmt.Fprintf(&sb, "已加载的钩子（%d 个，来自 %s）:\n", len(defs), dirHooksPath(dir))
		for _, d := range defs {
			matcher := d.Matcher
			if matcher == "" || matcher == "*" {
				matcher = "所有工具"
			}
			timeout := "60s"
			if d.Timeout > 0 {
				timeout = fmt.Sprintf("%ds", d.Timeout)
			}
			mode := "同步"
			if d.Async {
				mode = "异步"
			}
			if d.Event == hooks.SessionStart || d.Event == hooks.SessionEnd {
				fmt.Fprintf(&sb, "- %s [%s, %s]: %s\n", d.Event, mode, timeout, d.Command)
				continue
			}
			fmt.Fprintf(&sb, "- %s %s [%s, %s]: %s\n", d.Event, matcher, mode, timeout, d.Command)
		}
	}
	if loadErr != nil {
		fmt.Fprintf(&sb, "\n以下条目未加载:\n%s\n", loadErr)
	}
	return Output{Message: sb.String()}, nil
}

func dirHooksPath(dir string) string { return filepath.Join(dir, "hooks.json") }
