package engine

import (
	"encoding/json"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/token"
)

// Context-size accounting.
//
// The count that drives compaction used to be len(bytes)/4 over the message
// text alone: it read 1000 Chinese characters (3000 bytes, ~1000 tokens) as
// 750, and it never saw the system prompt or the tool definitions, which are
// sent with every request. The provider reports the real prompt size with
// every response, so that figure is the anchor; only what was appended since
// is estimated, with the same estimator the truncation code uses.

// recordUsage anchors the count on the provider's figure for a request that
// carried the first sent messages of the history.
func (e *Engine) recordUsage(inputTokens, sent int) {
	if inputTokens <= 0 || sent < 0 || sent > len(e.messages) {
		e.invalidateUsage()
		return
	}
	e.lastInputTokens = inputTokens
	e.usageMsgCount = sent
}

// invalidateUsage drops the provider's figure. Called whenever history that
// request carried is rewritten (masking, compaction, a loaded session): the
// figure measured a prefix that no longer exists.
func (e *Engine) invalidateUsage() {
	e.lastInputTokens = 0
	e.usageMsgCount = 0
}

// setRequestOverhead records what every request carries besides the
// messages, for the estimate used before the provider has reported anything.
func (e *Engine) setRequestOverhead(systemPrompt string, tools []api.ToolDef) {
	e.requestOverhead = token.Estimate(systemPrompt) + estimateToolDefs(tools)
}

// updateTokenCount refreshes e.totalTokens: the provider's last reported
// prompt size plus an estimate of the messages appended since, or — with no
// usable report — an estimate of everything a request sends.
func (e *Engine) updateTokenCount() {
	if e.lastInputTokens > 0 && e.usageMsgCount <= len(e.messages) {
		e.totalTokens = e.lastInputTokens + countTokens(e.messages[e.usageMsgCount:])
		return
	}
	e.totalTokens = e.requestOverhead + countTokens(e.messages)
}

// countTokens estimates the tokens msgs occupy in a request: text, text
// parts, reasoning, thinking blocks and tool-call arguments.
func countTokens(msgs []api.Message) int {
	n := 0
	for i := range msgs {
		m := &msgs[i]
		n += 4 // role and message framing
		n += token.Estimate(m.Content)
		n += token.Estimate(m.ReasoningContent)
		for _, p := range m.Parts {
			n += token.Estimate(p.Text)
		}
		for _, tb := range m.ThinkingBlocks {
			n += token.Estimate(string(tb))
		}
		for j := range m.ToolCalls {
			tc := &m.ToolCalls[j]
			n += token.Estimate(tc.Name) + 4
			if len(tc.Input) > 0 {
				if raw, err := json.Marshal(tc.Input); err == nil {
					n += token.Estimate(string(raw))
				}
			}
		}
	}
	return n
}

// estimateToolDefs estimates the tokens the tool definitions take.
func estimateToolDefs(tools []api.ToolDef) int {
	n := 0
	for i := range tools {
		n += token.Estimate(tools[i].Name) + token.Estimate(tools[i].Description)
		if len(tools[i].InputSchema) > 0 {
			if raw, err := json.Marshal(tools[i].InputSchema); err == nil {
				n += token.Estimate(string(raw))
			}
		}
	}
	return n
}

// compactionSafetyMargin is the room kept free beyond the reply's MaxTokens.
const compactionSafetyMargin = api.CompactionSafetyMargin
