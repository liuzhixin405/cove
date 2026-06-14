package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
)

// backgroundReview runs after each turn to auto-record session-local learnings.
func (e *Engine) backgroundReview() {
	if e.sessionNotes == nil {
		return
	}
	if len(e.messages) < 6 {
		return // not enough conversation to review
	}
	// Throttle: only run if at least 4 new messages since last review
	newMsgs := len(e.messages) - e.lastReviewMsgCount
	if newMsgs < 4 {
		return
	}
	e.lastReviewMsgCount = len(e.messages)

	// Snapshot messages so the background goroutine does not read e.messages
	// concurrently with a later turn appending to it (data race).
	snapshotMsgs := append([]api.Message(nil), e.messages...)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Build a concise snapshot of the conversation
		snapshot := buildReviewSnapshot(snapshotMsgs)
		if snapshot == "" {
			return
		}

		prompt := `你是一个对话回顾助手。分析以下对话片段，判断是否有值得记录到“当前会话笔记”的内容。

只在以下情况输出：
	1. 用户明确做出的技术选择 → 输出 DECISION: <一句话描述>
	2. 调试或实现中发现的关键事实 → 输出 DISCOVERY: <一句话描述>

不要输出：
- 一次性的任务细节
- 已经很显然的事实
- 代码本身（太长）

如果没有值得记住的，只输出 NONE。`

		resp, err := e.provider.Chat(ctx, api.ChatRequest{
			Model:      e.config.Model,
			SystemBase: prompt,
			Messages:   []api.Message{{Role: "user", Content: snapshot}},
			MaxTokens:  300,
		})
		if err != nil {
			log.Warnf("background review failed: %v", err)
			return
		}

		output := strings.TrimSpace(resp.Content)
		if output == "NONE" || output == "" {
			return
		}

		// Process session-note lines only. Durable long-term memory is owned by
		// extract.Runner; cross-session consolidation is owned by dream.Runner.
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "DECISION:") {
				decision := strings.TrimSpace(strings.TrimPrefix(line, "DECISION:"))
				if decision != "" {
					e.sessionNotes.AddDecision(decision)
					log.Debugf("background review saved decision: %s", decision)
					e.engineOutput(fmt.Sprintf("  \x1b[2m记录决策: %s\x1b[0m\n", reviewTruncate(decision, 50)))
				}
			}
			if strings.HasPrefix(line, "DISCOVERY:") {
				discovery := strings.TrimSpace(strings.TrimPrefix(line, "DISCOVERY:"))
				if discovery != "" {
					e.sessionNotes.AddDiscovery(discovery)
					log.Debugf("background review saved discovery: %s", discovery)
					e.engineOutput(fmt.Sprintf("  \x1b[2m记录发现: %s\x1b[0m\n", reviewTruncate(discovery, 50)))
				}
			}
		}
	}()
}

func buildReviewSnapshot(msgs []api.Message) string {
	// Take the last 10 messages (or all if fewer)
	start := 0
	if len(msgs) > 10 {
		start = len(msgs) - 10
	}
	msgs = msgs[start:]

	var sb strings.Builder
	for _, m := range msgs {
		content := m.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		switch m.Role {
		case "user":
			sb.WriteString("用户: " + content + "\n")
		case "assistant":
			sb.WriteString("助手: " + content + "\n")
			for _, tc := range m.ToolCalls {
				if path, ok := tc.Input["filePath"].(string); ok {
					sb.WriteString("  → " + tc.Name + "(" + path + ")\n")
				} else if cmd, ok := tc.Input["command"].(string); ok {
					c := cmd
					if len(c) > 80 {
						c = c[:80] + "..."
					}
					sb.WriteString("  → bash(" + c + ")\n")
				}
			}
		case "tool":
			r := content
			if len(r) > 80 {
				r = r[:80] + "..."
			}
			sb.WriteString("  结果: " + r + "\n")
		}
	}
	return sb.String()
}

func reviewTruncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
