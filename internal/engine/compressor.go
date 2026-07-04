package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
)

// CompressResult holds metrics about a compression operation.
type CompressResult struct {
	Compressed   bool   // whether compression was performed
	Summary      string // the generated summary text
	OldCount     int    // message count before compression
	NewCount     int    // message count after compression
	TokenSavings int    // estimated tokens saved
}

// ChatCompressor handles context window compression.
// Two-layer design:
//
//	Layer 1: truncate old tool results to 1-line summaries (free, no API call)
//	Layer 2: AI-powered summarization of middle conversation (API call)
type ChatCompressor struct {
	enabled        bool
	tokenThreshold float64 // fraction of model limit at which to trigger (default 0.5)
	keepFraction   float64 // fraction of recent messages to keep intact (default 0.3)
}

// NewChatCompressor creates a compressor with sensible defaults.
func NewChatCompressor() *ChatCompressor {
	return &ChatCompressor{
		enabled:        true,
		tokenThreshold: 0.5,
		keepFraction:   0.3,
	}
}

// NeedsCompression returns true if the given token count exceeds the threshold.
func (cc *ChatCompressor) NeedsCompression(tokenCount, tokenLimit int) bool {
	if !cc.enabled || tokenLimit <= 0 {
		return false
	}
	return float64(tokenCount) >= float64(tokenLimit)*cc.tokenThreshold
}

// Compress runs the two-layer compression pipeline.
// Returns a CompressResult and the compressed message list.
// If no compression is needed or possible, returns the original messages unchanged.
func (cc *ChatCompressor) Compress(
	ctx context.Context,
	messages []api.Message,
	tokenCount int,
	tokenLimit int,
	tryChat func(context.Context, api.ChatRequest) (*api.ChatResponse, error),
) (*CompressResult, []api.Message) {
	if !cc.enabled {
		return &CompressResult{}, messages
	}

	if len(messages) < 12 {
		return &CompressResult{}, messages
	}

	originalCount := len(messages)
	originalTokens := tokenCount

	// ─ Layer 1: Trim old tool results ─
	cc.trimOldToolResults(messages, int(float64(len(messages))*cc.keepFraction))
	tokenCount = countTokens(messages)
	if !cc.NeedsCompression(tokenCount, tokenLimit) {
		log.Debugf("compressor: layer1 trimming sufficient (%d tokens)", tokenCount)
		return &CompressResult{
			Compressed:   true,
			OldCount:     originalCount,
			NewCount:     len(messages),
			TokenSavings: originalTokens - tokenCount,
		}, messages
	}

	// ─ Layer 2: AI summarization ─
	// Find split point: preserve recent messages
	keepCount := int(float64(len(messages)) * cc.keepFraction)
	if keepCount < 6 {
		keepCount = 6
	}
	if keepCount > len(messages)-2 {
		keepCount = len(messages) - 2
	}

	// IMPORTANT: the system prompt is supplied separately via ChatRequest.SystemBase;
	// messages[0] is the first *user* turn, NOT a system message. So we must not keep
	// messages[0] as a pseudo-system anchor — doing so left the original first user
	// message in place AND prepended a summary user message, producing two consecutive
	// user turns (which the model API rejects with a 400, breaking every long chat).
	//
	// Anchor the kept tail on an assistant turn so the rebuilt sequence stays valid:
	//   [user(summary)] → [assistant ...] → [tool result ...] → ...
	// Landing on a "tool" message would orphan a tool_result (its tool_use was
	// dropped); landing on a "user" message would put two user turns back-to-back.
	splitIdx := len(messages) - keepCount
	for splitIdx > 0 && splitIdx < len(messages) && messages[splitIdx].Role != "assistant" {
		splitIdx--
	}
	if splitIdx <= 0 {
		return &CompressResult{}, messages // no clean assistant boundary — nothing safe to summarize
	}

	history := messages[:splitIdx]
	if len(history) < 4 {
		return &CompressResult{}, messages
	}

	summary, err := cc.generateSummary(ctx, history, tryChat)
	if err == nil {
		if ok, reason := validateSummaryQuality(summary, history); !ok {
			// The summary itself looks unreliable (too short/long, or it
			// dropped every file the conversation actually touched) — this
			// matters more for fast/mid-tier models, which are more likely
			// to produce a shallow or hallucinated summary under a tight
			// token budget. Treat it the same as a hard failure: better to
			// fall back to plain truncation (which loses detail but can't
			// silently misinform the model) than to keep a bad summary.
			log.Warnf("compressor: rejecting low-quality summary, falling back to truncation: %s", reason)
			err = fmt.Errorf("summary failed quality check: %s", reason)
		}
	}
	if err != nil {
		log.Warnf("compressor: summary generation failed, falling back to truncation: %v", err)
		// Fallback: simple truncation. Same invariant — a single user message then
		// the assistant-anchored tail; no leftover messages[0].
		truncated := make([]api.Message, 0, 1+keepCount)
		truncated = append(truncated, api.Message{
			Role:    "user",
			Content: "[Context truncated due to length. Continue the task.]",
		})
		truncated = append(truncated, messages[splitIdx:]...)
		return &CompressResult{
			Compressed:   true,
			OldCount:     len(messages),
			NewCount:     len(truncated),
			TokenSavings: 0,
		}, truncated
	}

	// Build compressed message list: a single summary user turn followed by the
	// assistant-anchored tail.
	compressed := make([]api.Message, 0, 1+keepCount)
	compressed = append(compressed, api.Message{
		Role:    "user",
		Content: "[Conversation Summary]\n" + summary + "\n\n[Continue the task from where you left off.]",
	})
	compressed = append(compressed, messages[splitIdx:]...)

	newTokens := countTokens(compressed)
	result := &CompressResult{
		Compressed:   true,
		Summary:      summary,
		OldCount:     len(messages),
		NewCount:     len(compressed),
		TokenSavings: tokenCount - newTokens,
	}

	log.Debugf("compressor: %d tokens/%d msgs -> %d tokens/%d msgs (kept tail %d)",
		tokenCount, len(messages), newTokens, len(compressed), keepCount)

	return result, compressed
}

// trimOldToolResults replaces verbose tool results in old messages with 1-line summaries.
// Messages within the keep boundary are left intact.
// NOTE: this mutates the slice's content in place (intentional — Layer 1 is cheap, no copy needed).
func (cc *ChatCompressor) trimOldToolResults(messages []api.Message, keepCount int) {
	cutoff := len(messages) - keepCount
	if cutoff < 1 {
		cutoff = 1
	}
	for i := 0; i < cutoff; i++ {
		if messages[i].Role == "tool" && len(messages[i].Content) > 300 {
			messages[i].Content = clipRunes(messages[i].Content, 100)
		}
	}
}

// generateSummary calls the model to produce a concise summary of old messages.
func (cc *ChatCompressor) generateSummary(
	ctx context.Context,
	messages []api.Message,
	tryChat func(context.Context, api.ChatRequest) (*api.ChatResponse, error),
) (string, error) {
	var summaryInput strings.Builder
	summaryInput.WriteString("Summarize this conversation history concisely. Structure:\n")
	summaryInput.WriteString("- Key decisions made\n")
	summaryInput.WriteString("- Files created/modified (paths)\n")
	summaryInput.WriteString("- Current task status\n")
	summaryInput.WriteString("- Errors encountered and resolutions\n")
	summaryInput.WriteString("- Important context for continuing\n\n")

	for _, m := range messages {
		summaryInput.WriteString(fmt.Sprintf("[%s] ", m.Role))
		content := m.Content
		if m.Role == "tool" {
			content = clipRunes(content, 100)
		} else {
			content = clipRunes(content, 250)
		}
		summaryInput.WriteString(content)
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if path, ok := tc.Input["filePath"].(string); ok {
					summaryInput.WriteString(fmt.Sprintf(" → %s(%s)", tc.Name, path))
				} else if cmd, ok := tc.Input["command"].(string); ok {
					summaryInput.WriteString(fmt.Sprintf(" → bash(%s)", clipRunes(cmd, 60)))
				} else {
					summaryInput.WriteString(fmt.Sprintf(" → %s()", tc.Name))
				}
			}
		}
		summaryInput.WriteString("\n")
	}

	req := api.ChatRequest{
		SystemBase: "You are a conversation summarizer. Be concise and factual.",
		Messages:   []api.Message{{Role: "user", Content: summaryInput.String()}},
		MaxTokens:  600,
	}

	resp, err := tryChat(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// validateSummaryQuality does a lightweight sanity check on a Layer-2
// compression summary before it's allowed to replace real conversation
// history. This is a cheap heuristic, not a semantic correctness check —
// it exists because a bad summary is worse than no summary: it silently
// causes "amnesia" for the rest of the session instead of visibly failing.
// Fast/mid-tier models are more likely than top-tier ones to produce a
// shallow or hallucinated summary under a tight token budget, which is
// exactly the failure mode this guards against.
func validateSummaryQuality(summary string, history []api.Message) (ok bool, reason string) {
	trimmed := strings.TrimSpace(summary)
	if len(trimmed) < 40 {
		return false, fmt.Sprintf("summary too short (%d chars)", len(trimmed))
	}
	if len(trimmed) > 6000 {
		return false, fmt.Sprintf("summary suspiciously long (%d chars), likely malformed", len(trimmed))
	}

	paths := distinctToolPaths(history)
	if len(paths) > 0 {
		lowerSummary := strings.ToLower(trimmed)
		covered := 0
		for _, p := range paths {
			if strings.Contains(lowerSummary, strings.ToLower(filepath.Base(p))) {
				covered++
			}
		}
		if covered == 0 {
			return false, fmt.Sprintf("summary mentions none of the %d file(s) touched in this history", len(paths))
		}
		coverage := float64(covered) / float64(len(paths))
		if len(paths) >= 3 && coverage < 0.3 {
			return false, fmt.Sprintf("summary covers only %.0f%% of %d files touched in history", coverage*100, len(paths))
		}
	}

	return true, ""
}

// distinctToolPaths collects the distinct file paths referenced by tool
// calls anywhere in history, in first-seen order.
func distinctToolPaths(history []api.Message) []string {
	seen := make(map[string]bool)
	var paths []string
	for _, m := range history {
		for _, tc := range m.ToolCalls {
			if p := toolTargetPath(tc.Input); p != "" && !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	return paths
}

// toolTargetPath extracts the file path a write/edit tool call targets, honoring
// the same key aliases the tools accept (filePath, file_path, path, filepath,
// file) and normalizing the result so two spellings of the same path compare
// equal. Returns "" if no path key is present.
func toolTargetPath(input map[string]any) string {
	for _, k := range []string{"filePath", "file_path", "path", "filepath", "file"} {
		if v, ok := input[k].(string); ok && v != "" {
			return filepath.Clean(v)
		}
	}
	return ""
}

// clipRunes truncates s to at most n runes (not bytes), appending "..." if it
// was shortened. Byte-slicing (s[:n]) would cut multi-byte UTF-8 sequences mid
// character and corrupt Chinese/emoji text, which this codebase produces heavily.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// countTokens is declared in engine.go (1 token ≈ 4 chars)
