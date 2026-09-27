package command

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove/internal/trace"
)

// showTrace is "/diagnose trace [N]": the last N events of the interaction
// log (internal/trace), one line each, oldest first, so a turn that "never
// finished" can be read back as what the engine did and how long each step
// took.
func (c *diagnoseCmd) showTrace(args []string) (Output, error) {
	n := 30
	if len(args) > 0 {
		if v, err := strconv.Atoi(strings.TrimSpace(args[0])); err == nil && v > 0 {
			n = v
		}
	}
	if trace.Path() == "" {
		return Output{Message: "交互轨迹未启用（找不到 ~/.cove）。\n"}, nil
	}
	events, err := trace.Tail(n)
	if err != nil {
		return Output{}, err
	}
	if len(events) == 0 {
		return Output{Message: fmt.Sprintf("还没有交互轨迹记录（%s）。\n", trace.Path())}, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\x1b[1m交互轨迹\x1b[0m 最近 %d 条（%s）\n\n", len(events), trace.Path())
	for _, ev := range events {
		sb.WriteString(FormatTraceEvent(ev))
		sb.WriteByte('\n')
	}
	return Output{Message: sb.String()}, nil
}

// FormatTraceEvent renders one trace event as a compact line:
//
//	15:34:11 model  qwen3.6-27b  12 msgs ~17773 tok  3m46s  → error E2008 request (17773 tokens) exceeds…
//	15:34:12 tool   write  0.0s  69 B  ✗ Error: path outside working directory…
//	15:35:00 compact  17773 → 9800 tok, 14 → 6 msgs  已摘要早期对话
func FormatTraceEvent(ev trace.Event) string {
	f := ev.Fields
	ts := ev.Time.Format("15:04:05")
	switch ev.Kind {
	case "turn":
		return fmt.Sprintf("%s turn     %s  用户 %s + 附加 %s，历史 %v 条 ~%v tok（固定开销 %v，窗口 %v）", ts, str(f["model"]),
			bytes(f["user_bytes"]), bytes(f["note_bytes"]), num(f["messages"]), num(f["est_tokens"]), num(f["overhead_tokens"]), num(f["window"]))
	case "model":
		line := fmt.Sprintf("%s model    %s  %v msgs ~%v tok  %s", ts, str(f["model"]), num(f["messages"]), num(f["est_tokens"]), dur(f["ms"]))
		if e, ok := f["error"].(string); ok && e != "" {
			kind := str(f["error_kind"])
			if f["cancelled"] == true {
				kind = "cancelled"
			}
			return line + fmt.Sprintf("  \x1b[31m✗ %s\x1b[0m %s", kind, e)
		}
		return line + fmt.Sprintf("  → %s  in %v out %v  %v 工具调用 %s 正文", str(f["stop"]), num(f["in"]), num(f["out"]), num(f["tool_calls"]), bytes(f["content_bytes"]))
	case "tool":
		line := fmt.Sprintf("%s tool     %s  %s  %s", ts, str(f["name"]), dur(f["ms"]), bytes(f["result_bytes"]))
		if f["error"] == true {
			return line + "  \x1b[31m✗\x1b[0m " + str(f["head"])
		}
		return line
	case "compact":
		how := str(f["reason"])
		if f["summarized"] == true {
			how = "已摘要早期对话"
		} else if f["compressed"] == true && how == "" {
			how = "已裁剪旧工具输出"
		} else if f["compressed"] != true && how == "" {
			how = "未压缩"
		}
		return fmt.Sprintf("%s compact  %v → %v tok, %v → %v msgs  %s", ts, num(f["tokens_before"]), num(f["tokens_after"]), num(f["msgs_before"]), num(f["msgs_after"]), how)
	case "overflow":
		action := "无法再缩小，本轮结束"
		if f["retry"] == true {
			action = "已缩小并重试"
		}
		return fmt.Sprintf("%s overflow %s  %v → %v tok  %s", ts, str(f["model"]), num(f["tokens_before"]), num(f["tokens_after"]), action)
	}
	return fmt.Sprintf("%s %s %v", ts, ev.Kind, f)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) any {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case nil:
		return 0
	}
	return v
}

func bytes(v any) string {
	n, _ := v.(float64)
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", n/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", n/(1<<10))
	}
	return fmt.Sprintf("%d B", int(n))
}

func dur(v any) string {
	ms, _ := v.(float64)
	switch {
	case ms >= 60000:
		return fmt.Sprintf("%dm%02ds", int(ms)/60000, (int(ms)%60000)/1000)
	case ms >= 1000:
		return fmt.Sprintf("%.1fs", ms/1000)
	}
	return fmt.Sprintf("%dms", int(ms))
}
