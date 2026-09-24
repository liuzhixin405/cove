package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/memory"
	"github.com/liuzhixin405/cove/internal/skills"
	"github.com/liuzhixin405/cove/internal/textutil"
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

		resp, _, err := e.fallback.TryChat(ctx, func(api.Provider) api.ChatRequest { return e.reviewRequest(snapshot) })
		if err != nil {
			log.Warnf("background review failed: %v", err)
			return
		}
		e.applyReview(resp.Content)
	}()
}

const reviewPrompt = `你是一个对话回顾助手。分析以下对话片段，判断是否有值得记住的内容。

只在以下情况输出：
1. 用户偏好（编码风格、工具偏好、工作习惯）→ 输出 MEMORY: <一句话描述>
2. 可复用的工作流程（解决特定问题的步骤）→ 输出 SKILL: <技能名> | <简要步骤>

不要输出：
- 一次性的任务细节
- 已经很显然的事实
- 代码本身（太长）

如果没有值得记住的，只输出 NONE。`

// backgroundMaxTokens bounds the answer of a background bookkeeping request.
// Reasoning models (deepseek-v4-pro) spend part of max_tokens on thinking
// before they answer; the old 300 often left no room for the answer at all.
const backgroundMaxTokens = 4000

// reviewRequest builds the background review request for a snapshot. It runs
// on the background (fast) model, like extraction and consolidation; it used
// to run on the premium model for a job that only writes one-line notes.
func (e *Engine) reviewRequest(snapshot string) api.ChatRequest {
	model := e.backgroundModel
	if model == "" {
		model = e.config.Model
	}
	return api.ChatRequest{
		Model:      model,
		SystemBase: reviewPrompt,
		Messages:   []api.Message{{Role: "user", Content: snapshot}},
		MaxTokens:  backgroundMaxTokens,
	}
}

// applyReview stores the MEMORY and SKILL lines of a review answer.
func (e *Engine) applyReview(output string) {
	output = strings.TrimSpace(output)
	if output == "NONE" || output == "" {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MEMORY:") {
			mem := strings.TrimSpace(strings.TrimPrefix(line, "MEMORY:"))
			if mem != "" && e.memStore != nil {
				// One file per memory, named by its content: every memory used
				// to be saved as "auto", so each review replaced the last one,
				// while a repeated memory still lands on the same file.
				sum := sha256.Sum256([]byte(mem))
				_ = e.memStore.Save("auto-"+hex.EncodeToString(sum[:6]), mem)
				log.Debugf("background review saved memory: %s", mem)
				e.debugOutput(fmt.Sprintf("  \x1b[2mlearned memory: %s\x1b[0m\n", reviewTruncate(mem, 50)))
			}
		}
		if strings.HasPrefix(line, "SKILL:") {
			skill := strings.TrimSpace(strings.TrimPrefix(line, "SKILL:"))
			if skill != "" && e.skillMgr != nil {
				parts := strings.SplitN(skill, "|", 2)
				if len(parts) == 2 {
					name := strings.TrimSpace(parts[0])
					content := strings.TrimSpace(parts[1])
					// A learned skill is shown to the model in later turns just
					// like a memory, so it gets the same injection screening.
					if err := memory.ScreenContent(name + "\n" + content); err != nil {
						log.Warnf("background review skill %q refused: %v", name, err)
						continue
					}
					e.skillMgr.Register(skills.Skill{Name: name, Prompt: content})
					log.Debugf("background review saved skill: %s", name)
					e.debugOutput(fmt.Sprintf("  \x1b[2mlearned skill: %s\x1b[0m\n", name))
				}
			}
		}
	}
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
		content := clipRunes(m.Content, 200)
		switch m.Role {
		case "user":
			sb.WriteString("用户: " + content + "\n")
		case "assistant":
			sb.WriteString("助手: " + content + "\n")
			for _, tc := range m.ToolCalls {
				if path, ok := tc.Input["filePath"].(string); ok {
					sb.WriteString("  → " + tc.Name + "(" + path + ")\n")
				} else if cmd, ok := tc.Input["command"].(string); ok {
					sb.WriteString("  → bash(" + clipRunes(cmd, 80) + ")\n")
				}
			}
		case "tool":
			// Tool results are left out, as the memory extractor leaves them
			// out: they are what files, pages and commands said (secrets,
			// injected instructions), not what the user wants remembered.
		}
	}
	return sb.String()
}

func reviewTruncate(s string, max int) string {
	// max is a byte budget (it bounds prompt size), clipped on a rune boundary.
	return textutil.ClipBytes(s, max, "...")
}
