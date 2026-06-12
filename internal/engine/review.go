package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/skills"
)

// backgroundReview runs after each turn to auto-extract learnings into memory/skills.
func (e *Engine) backgroundReview() {
	if e.memStore == nil && e.skillMgr == nil {
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

		prompt := `你是一个对话回顾助手。分析以下对话片段，判断是否有值得记住的内容。

只在以下情况输出：
1. 用户偏好（编码风格、工具偏好、工作习惯）→ 输出 MEMORY: <一句话描述>
2. 可复用的工作流程（解决特定问题的步骤）→ 输出 SKILL: <技能名> | <简要步骤>

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

		// Process MEMORY lines
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "MEMORY:") {
				mem := strings.TrimSpace(strings.TrimPrefix(line, "MEMORY:"))
				if mem != "" && e.memStore != nil {
					e.memStore.Save("auto", mem)
					log.Debugf("background review saved memory: %s", mem)
					e.engineOutput(fmt.Sprintf("  \x1b[2m🧠 记住了: %s\x1b[0m\n", reviewTruncate(mem, 50)))
				}
			}
			if strings.HasPrefix(line, "SKILL:") {
				skill := strings.TrimSpace(strings.TrimPrefix(line, "SKILL:"))
				if skill != "" && e.skillMgr != nil {
					parts := strings.SplitN(skill, "|", 2)
					if len(parts) == 2 {
						name := strings.TrimSpace(parts[0])
						content := strings.TrimSpace(parts[1])
						e.skillMgr.Register(skills.Skill{Name: name, Prompt: content})
						log.Debugf("background review saved skill: %s", name)
						e.engineOutput(fmt.Sprintf("  \x1b[2m📚 学会了: %s\x1b[0m\n", name))
					}
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
