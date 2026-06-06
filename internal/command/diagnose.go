package command

import (
	"context"
	"fmt"

	"github.com/liuzhixin405/cove/internal/diagnostic"
)

type diagnoseCmd struct{}

func NewDiagnoseCmd() Command { return &diagnoseCmd{} }

func (c *diagnoseCmd) Name() string        { return "diagnose" }
func (c *diagnoseCmd) Aliases() []string   { return []string{"diag"} }
func (c *diagnoseCmd) Description() string { return "运行系统诊断，检查并修复常见问题" }
func (c *diagnoseCmd) Help() string {
	return `/diagnose        运行完整系统诊断（含网络检测）
/diagnose quick  仅运行快速检查（跳过网络）
/diagnose codes  列出所有已知错误代码`
}

func (c *diagnoseCmd) Execute(ctx context.Context, input Input) (Output, error) {
	cfg := input.Config
	checker := diagnostic.NewChecker(cfg)

	mode := "full"
	if len(input.Args) > 0 {
		mode = input.Args[0]
	}

	switch mode {
	case "codes":
		return c.listCodes()
	case "quick":
		report := checker.RunQuick()
		return Output{Message: report.Format()}, nil
	default:
		report := checker.RunAll(ctx)
		msg := report.Format()
		if report.AutoFixed > 0 {
			msg += "\n\x1b[32m所有修复已热加载到当前进程，无需重启 exe。\x1b[0m\n"
		}
		return Output{Message: msg}, nil
	}
}

func (c *diagnoseCmd) listCodes() (Output, error) {
	all := diagnostic.AllErrors()

	// Group by category
	groups := map[diagnostic.Category][]diagnostic.ErrorCode{}
	for code, def := range all {
		groups[def.Category] = append(groups[def.Category], code)
	}

	var msg string
	msg += "\x1b[1m已注册的错误代码:\x1b[0m\n\n"

	categories := []diagnostic.Category{
		diagnostic.CatConfig,
		diagnostic.CatNetwork,
		diagnostic.CatAPI,
		diagnostic.CatPermission,
		diagnostic.CatTool,
		diagnostic.CatEngine,
		diagnostic.CatSession,
		diagnostic.CatFileSystem,
	}

	for _, cat := range categories {
		codes, ok := groups[cat]
		if !ok {
			continue
		}
		msg += fmt.Sprintf("  \x1b[36m[%s]\x1b[0m\n", cat)
		for _, code := range codes {
			def := all[code]
			fixable := ""
			if def.AutoFixable && def.HotFixable {
				fixable = " \x1b[32m(自动修复+即时生效)\x1b[0m"
			} else if def.AutoFixable {
				fixable = " \x1b[32m(可自动修复)\x1b[0m"
			}
			msg += fmt.Sprintf("    %s  %s%s\n", code, def.Message, fixable)
		}
		msg += "\n"
	}

	return Output{Message: msg}, nil
}
