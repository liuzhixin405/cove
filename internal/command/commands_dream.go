package command

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/dream"
)

func (c *DreamCmd) Name() string        { return "dream" }
func (c *DreamCmd) Aliases() []string   { return nil }
func (c *DreamCmd) Description() string { return "查看或立即运行记忆整理 (dream)" }
func (c *DreamCmd) Help() string {
	return "/dream [status|run] - 无参数显示自动整理的门槛与上次结果；run 忽略时间与会话门槛立即在后台整理"
}

func (c *DreamCmd) Execute(ctx context.Context, in Input) (Output, error) {
	sub := ""
	if len(in.Args) > 0 {
		sub = strings.ToLower(in.Args[0])
	}
	switch sub {
	case "", "status":
		var st dream.Status
		if r := dream.Current(); r != nil {
			st = r.Status()
		} else {
			st = dream.StatusFromDisk()
		}
		return Output{Message: FormatDreamStatus(st)}, nil
	case "run", "now":
		r := dream.Current()
		if r == nil {
			return Output{Message: "当前没有可用的整理运行器（需要在会话中执行）。"}, nil
		}
		n, err := r.RunNow(ctx)
		switch {
		case errors.Is(err, dream.ErrDisabled):
			return Output{Message: "自动整理已禁用。请在 ~/.cove/dream.json 中设置 \"enabled\": true。"}, nil
		case errors.Is(err, dream.ErrLockHeld):
			if task := dream.ActiveTask(); task != nil {
				return Output{Message: fmt.Sprintf("整理锁被占用：已有整理在运行 (开始于 %s)。", task.StartTime.Format("15:04:05"))}, nil
			}
			return Output{Message: "整理锁被占用：另一个 cove 进程正在整理，或本进程 1 小时内刚整理过（整理完成后锁会保留 1 小时）。稍后再试。"}, nil
		case err != nil:
			return Output{}, fmt.Errorf("dream: %w", err)
		}
		return Output{Message: fmt.Sprintf("已开始整理 (回顾 %d 个会话)，在后台运行；完成后用 /dream 查看结果。", n)}, nil
	default:
		return Output{Message: "用法: /dream [status|run]"}, nil
	}
}

// FormatDreamStatus renders a dream.Status for /dream (and /doctor-style
// reports): the two gates, what is still missing, and the last run.
func FormatDreamStatus(st dream.Status) string {
	var sb strings.Builder
	sb.WriteString("记忆整理 (dream):\n")
	if st.Enabled {
		sb.WriteString("  自动整理: 已启用\n")
	} else {
		sb.WriteString("  自动整理: 已禁用 (~/.cove/dream.json)\n")
	}
	if st.Suppressed != "" {
		fmt.Fprintf(&sb, "  本进程不自动整理: %s\n", st.Suppressed)
	}
	if st.LastConsolidatedAt.IsZero() {
		sb.WriteString("  距上次整理: 从未整理\n")
	} else {
		fmt.Fprintf(&sb, "  距上次整理: %.1f 小时 (%s)\n", st.HoursSinceLast, st.LastConsolidatedAt.Format("2006-01-02 15:04"))
	}
	if st.Trigger == dream.TriggerSessionEnd {
		fmt.Fprintf(&sb, "  触发方式: 对话结束时（至少 %d 个助手回合，后台进程整理；dream.json 设 \"trigger\": \"threshold\" 改回门槛模式）\n", st.MinTurns)
		fmt.Fprintf(&sb, "  上次整理后的新会话: %d 个\n", st.SessionsSinceLast)
	} else {
		sb.WriteString("  触发方式: 门槛（回合结束时检查时间与会话门槛）\n")
		if h := st.HoursNeeded(); h > 0 {
			fmt.Fprintf(&sb, "  时间门槛: %d 小时，还差 %.1f 小时\n", st.MinHours, h)
		} else {
			fmt.Fprintf(&sb, "  时间门槛: %d 小时，已满足\n", st.MinHours)
		}
		if n := st.SessionsNeeded(); n > 0 {
			fmt.Fprintf(&sb, "  会话门槛: %d 个会话，已有 %d 个，还差 %d 个会话\n", st.MinSessions, st.SessionsSinceLast, n)
		} else {
			fmt.Fprintf(&sb, "  会话门槛: %d 个会话，已有 %d 个，已满足\n", st.MinSessions, st.SessionsSinceLast)
		}
	}
	if !st.LastWorkerStartedAt.IsZero() {
		fmt.Fprintf(&sb, "  上次会话结束整理: %s\n", st.LastWorkerResult)
	}
	switch {
	case st.Running:
		sb.WriteString("  状态: 正在整理\n")
	case !st.LastRunAt.IsZero() && st.LastRunErr != nil:
		fmt.Fprintf(&sb, "  上次运行: %s 失败: %v\n", st.LastRunAt.Format(time.TimeOnly), st.LastRunErr)
	case !st.LastRunAt.IsZero():
		fmt.Fprintf(&sb, "  上次运行: %s 完成，写入 %d 个文件\n", st.LastRunAt.Format(time.TimeOnly), st.LastRunFilesTouched)
	}
	sb.WriteString(FormatDreamCost(DreamUsage{InputTokens: st.LastRunInputTokens, OutputTokens: st.LastRunOutputTokens, CostUSD: st.LastRunCostUSD}))
	return strings.TrimRight(sb.String(), "\n")
}
