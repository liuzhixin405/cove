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
/diagnose errors 查看运行时记录的错误/卡顿及修复建议
/diagnose archive 修复完成后归档错误日志，开始新的记录周期
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
	case "errors", "log", "recent":
		return c.showRuntimeErrors()
	case "archive", "fixed", "clear":
		return c.archiveRuntimeLog()
	case "quick":
		report := checker.RunQuick()
		return Output{Message: report.Format()}, nil
	default:
		report := checker.RunAll(ctx)
		msg := report.Format()
		if report.AutoFixed > 0 {
			msg += "\n\x1b[32m所有修复已热加载到当前进程，无需重启 exe。\x1b[0m\n"
		}
		// Append a runtime-error reminder so recurring hangs/failures from this
		// session (and previous ones) surface alongside the static checks.
		msg += c.runtimeReminder()
		return Output{Message: msg}, nil
	}
}

// showRuntimeErrors lists problems recorded while the agent was running, merged
// with persisted events from previous runs, each paired with a fix suggestion.
func (c *diagnoseCmd) showRuntimeErrors() (Output, error) {
	events := diagnostic.RecentRuntime()
	if len(events) == 0 {
		events = diagnostic.LoadRuntimeLog()
	}
	if len(events) == 0 {
		return Output{Message: "\x1b[32m✓ 没有记录到运行时错误或卡顿。\x1b[0m\n"}, nil
	}
	summaries := diagnostic.SummarizeRuntime(events)
	const reset = "\x1b[0m"
	msg := fmt.Sprintf("\x1b[1m运行时问题记录\x1b[0m (共 %d 条，按严重程度排序)\n\n", len(events))
	for _, s := range summaries {
		count := ""
		if s.Count > 1 {
			count = fmt.Sprintf(" \x1b[2m×%d\x1b[0m", s.Count)
		}
		msg += fmt.Sprintf(" %s[%s]%s %s%s\n", s.Severity.Color(), s.Severity.String(), reset, s.Message, count)
		if s.Recovery != "" {
			tag := "💡"
			if s.Fixable {
				tag = "🔧 可自动修复:"
			}
			msg += fmt.Sprintf("    \x1b[33m%s %s\x1b[0m\n", tag, s.Recovery)
		}
	}
	msg += "\n\x1b[2m日志文件: ~/.cove/errors.log\x1b[0m\n"
	return Output{Message: msg}, nil
}

// archiveRuntimeLog archives the current error log and starts a fresh cycle,
// to be run after the reported problems have been fixed.
func (c *diagnoseCmd) archiveRuntimeLog() (Output, error) {
	dest, err := diagnostic.ArchiveRuntimeLog()
	if err != nil {
		return Output{Message: fmt.Sprintf("\x1b[31m归档失败: %s\x1b[0m\n", err.Error())}, nil
	}
	if dest == "" {
		return Output{Message: "\x1b[32m✓ 当前没有需要归档的错误日志，已开始新的记录周期。\x1b[0m\n"}, nil
	}
	return Output{Message: fmt.Sprintf("\x1b[32m✓ 错误日志已归档至 %s，已开始新的记录周期。\x1b[0m\n", dest)}, nil
}

// runtimeReminder returns a short reminder block when there are recorded
// runtime problems, or an empty string otherwise.
func (c *diagnoseCmd) runtimeReminder() string {
	events := diagnostic.RecentRuntime()
	if len(events) == 0 {
		return ""
	}
	summaries := diagnostic.SummarizeRuntime(events)
	if len(summaries) == 0 {
		return ""
	}
	const reset = "\x1b[0m"
	msg := "\n\x1b[1m运行时问题提醒\x1b[0m (本次会话，详见 /diagnose errors)\n"
	shown := 0
	for _, s := range summaries {
		if shown >= 5 {
			break
		}
		count := ""
		if s.Count > 1 {
			count = fmt.Sprintf(" ×%d", s.Count)
		}
		msg += fmt.Sprintf("  %s%s%s %s%s\n", s.Severity.Color(), s.Severity.String(), reset, s.Message, count)
		shown++
	}
	return msg
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
