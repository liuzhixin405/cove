package engine

import (
	"context"
	"fmt"
	"github.com/liuzhixin405/cove-agent/internal/textutil"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/log"
)

// CompressResult holds metrics about a compression operation.
type CompressResult struct {
	Compressed   bool   // whether compression was performed
	Summary      string // the generated summary text
	OldCount     int    // message count before compression
	NewCount     int    // message count after compression
	TokenSavings int    // estimated tokens saved
	// Summarized reports that layer 2 replaced the older history with a
	// model-written summary (not just trimmed tool output or truncated).
	Summarized bool
	// Reason says, for the user, why nothing (or only a fallback) was done.
	Reason string
}

// Minimum history sizes. A forced compression (/compact, limit 0) works on
// much shorter histories than the automatic one.
const (
	compressMinMessages      = 12
	forceCompressMinMessages = 4
)

// Summary-input character budgets per message (generateSummary). The first
// real user message usually states the whole task; at 250 characters the
// summary lost the original requirements.
const (
	summaryFirstUserRunes = 2000
	summaryUserRunes      = 600
	summaryAssistantRunes = 250
	summaryToolRunes      = 100
)

// ChatCompressor handles context window compression.
// Two-layer design:
//
//	Layer 1: truncate old tool results to 1-line summaries (free, no API call)
//	Layer 2: AI-powered summarization of middle conversation (API call)
type ChatCompressor struct {
	enabled        bool
	tokenThreshold float64 // fraction of the limit at which to trigger (default 1.0: the limit is the trigger)
	keepFraction   float64 // fraction of recent messages to keep intact (default 0.3)
}

// NewChatCompressor creates a compressor with sensible defaults.
func NewChatCompressor() *ChatCompressor {
	return &ChatCompressor{
		enabled: true,
		// The limit passed in is already the trigger point (see
		// api.CompactionTrigger, which also leaves room for the reply), and
		// the count is the provider's real prompt size, system prompt and
		// tools included (token_count.go). It used to be half of an estimate
		// that missed Chinese text and the system prompt.
		tokenThreshold: 1.0,
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
// A tokenLimit of 0 or less forces the summary layer (the /compact command):
// layer-1 trimming alone does not end it, and 4 messages are enough.
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

	force := tokenLimit <= 0
	minMsgs := compressMinMessages
	if force {
		minMsgs = forceCompressMinMessages
	}
	if len(messages) < minMsgs {
		return &CompressResult{Reason: fmt.Sprintf("对话只有 %d 条消息，至少 %d 条才能压缩", len(messages), minMsgs)}, messages
	}

	originalCount := len(messages)
	originalTokens := tokenCount
	// tokenCount covers what the request sends besides the messages (system
	// prompt, tools), and may be the provider's figure; keep that share when
	// re-estimating the trimmed messages.
	overhead := tokenCount - countTokens(messages)
	if overhead < 0 {
		overhead = 0
	}

	// ─ Layer 1: Trim old tool results ─
	// A dedupe stub (masker.go) past the trim boundary names a result
	// before it; trimming that result would leave the stub pointing at
	// nothing, so its content moves into the stub first.
	trimKeep := int(float64(len(messages)) * cc.keepFraction)
	restoreDedupedAcross(messages, trimCutoff(len(messages), trimKeep))
	// trimmed: layer 1 changed the history in place. Every early return
	// below used to report Compressed=false after it, so compact skipped
	// the cleanup a rewritten history needs and /compact said "可摘要的历史
	// 太短" about a history it had just trimmed.
	trimmed := cc.trimOldToolResults(messages, trimKeep)
	tokenCount = overhead + countTokens(messages)
	// notSummarized is the result of an early return: Compressed when
	// layer 1 did trim, with why the summary layer did not run.
	notSummarized := func(reason string) *CompressResult {
		if !trimmed {
			return &CompressResult{Reason: reason}
		}
		return &CompressResult{
			Compressed:   true,
			OldCount:     originalCount,
			NewCount:     len(messages),
			TokenSavings: originalTokens - tokenCount,
			Reason:       "仅裁剪了旧工具输出；" + reason,
		}
	}
	if !force && !cc.NeedsCompression(tokenCount, tokenLimit) {
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
	minKeep, minHistory := 6, 4
	if force {
		minKeep, minHistory = 1, 2
	}
	if keepCount < minKeep {
		keepCount = minKeep
	}
	if keepCount > len(messages)-2 {
		keepCount = len(messages) - 2
	}

	// IMPORTANT: the system prompt is supplied separately via ChatRequest.SystemBase;
	// messages[0] is the first *user* turn, NOT a system message. So we must not keep
	// messages[0] as a pseudo-system anchor — doing so left the original first user
	// message in place AND prepended a summary user message, producing two consecutive
	// user turns (which the model API rejects with a 400, breaking every long chat).
	splitIdx := chooseCompressionSplitAssistant(messages, keepCount)
	if splitIdx <= 0 {
		// no clean assistant boundary — nothing safe to summarize
		return notSummarized("找不到可安全切分的助手回复边界"), messages
	}

	history := messages[:splitIdx]
	if len(history) < minHistory {
		return notSummarized("可摘要的历史太短"), messages
	}
	// The summarized part goes; a stub in the kept tail must not point
	// into it.
	restoreDedupedAcross(messages, splitIdx)

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
			Content: "<compress summary=\"context-truncated\">\n" + originalRequestBlock(messages) + "[Context truncated due to length. Continue the task.]\n</compress>",
			// Engine text, not a request: a later compaction took it for
			// the user's request and nested its <original_request> block.
			Synthetic: true,
		})
		truncated = append(truncated, messages[splitIdx:]...)
		return &CompressResult{
			Compressed:   true,
			OldCount:     len(messages),
			NewCount:     len(truncated),
			TokenSavings: 0,
			Reason:       "摘要生成失败，已改为截断旧历史",
		}, truncated
	}

	// Build compressed message list: a single summary user turn followed by the
	// assistant-anchored tail.
	compressed := make([]api.Message, 0, 1+keepCount)
	compressed = append(compressed, api.Message{
		Role:      "user",
		Content:   "<compress summary=\"conversation-history\">\n" + originalRequestBlock(messages) + summary + "\n\n[Continue the task from where you left off.]\n</compress>",
		Synthetic: true,
	})
	compressed = append(compressed, messages[splitIdx:]...)

	newTokens := overhead + countTokens(compressed)
	result := &CompressResult{
		Compressed:   true,
		Summary:      summary,
		OldCount:     len(messages),
		NewCount:     len(compressed),
		TokenSavings: tokenCount - newTokens,
		Summarized:   true,
	}

	log.Debugf("compressor: %d tokens/%d msgs -> %d tokens/%d msgs (kept tail %d)",
		tokenCount, len(messages), newTokens, len(compressed), keepCount)

	return result, compressed
}

// trimOldToolResults replaces verbose tool results in old messages with 1-line summaries.
// Messages within the keep boundary are left intact. It reports whether it
// changed any message.
// NOTE: this mutates the slice's content in place (intentional — Layer 1 is cheap, no copy needed).
func (cc *ChatCompressor) trimOldToolResults(messages []api.Message, keepCount int) bool {
	changed := false
	for i := 0; i < trimCutoff(len(messages), keepCount); i++ {
		if messages[i].Role == "tool" && len(messages[i].Content) > 300 {
			messages[i].Content = keepRunes(messages[i].Content, 100)
			changed = true
		}
	}
	return changed
}

// trimCutoff is the index before which trimOldToolResults trims.
func trimCutoff(n, keepCount int) int {
	cutoff := n - keepCount
	if cutoff < 1 {
		cutoff = 1
	}
	return cutoff
}

// generateSummary calls the model to produce a concise summary of old messages.
func (cc *ChatCompressor) generateSummary(
	ctx context.Context,
	messages []api.Message,
	tryChat func(context.Context, api.ChatRequest) (*api.ChatResponse, error),
) (string, error) {
	var summaryInput strings.Builder
	summaryInput.WriteString("Summarize this conversation history concisely. Structure:\n")
	summaryInput.WriteString("- The user's original request and its requirements (keep them specific)\n")
	summaryInput.WriteString("- Key decisions made\n")
	summaryInput.WriteString("- Files created/modified (paths)\n")
	summaryInput.WriteString("- Current task status\n")
	summaryInput.WriteString("- Errors encountered and resolutions\n")
	summaryInput.WriteString("- Important context for continuing\n\n")

	sawRequest := false
	for _, m := range messages {
		fmt.Fprintf(&summaryInput, "[%s] ", m.Role)
		content := m.Content
		switch {
		case m.Role == "tool":
			content = keepRunes(content, summaryToolRunes)
		case m.Role == "user" && !sawRequest && !looksSynthetic(m):
			// The original request: keep enough of it that the summary
			// can carry the requirements forward.
			sawRequest = true
			content = keepRunes(content, summaryFirstUserRunes)
		case m.Role == "user":
			content = keepRunes(content, summaryUserRunes)
		default:
			content = keepRunes(content, summaryAssistantRunes)
		}
		summaryInput.WriteString(content)
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if path := toolFilePath(tc); path != "" {
					fmt.Fprintf(&summaryInput, " → %s(%s)", tc.Name, path)
				} else if cmd, ok := tc.Input["command"].(string); ok {
					fmt.Fprintf(&summaryInput, " → bash(%s)", keepRunes(cmd, 60))
				} else {
					fmt.Fprintf(&summaryInput, " → %s()", tc.Name)
				}
			}
		}
		summaryInput.WriteString("\n")
	}

	req := api.ChatRequest{
		SystemBase: "You are a conversation summarizer. Be concise and factual.",
		Messages:   []api.Message{{Role: "user", Content: summaryInput.String()}},
		// Room for a reasoning model's thinking as well as the summary; the
		// prompt itself keeps the summary short.
		MaxTokens: backgroundMaxTokens,
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

// toolFilePath is the file a tool call targets, through every key alias the
// file tools accept, or "" when the call names no file. For the file tools
// themselves (read, write, edit) it is toolTargetPath, "path" included; for
// every other tool only the explicit file keys count, because grep's and
// glob's "path" is the directory they search, not a file they touch.
func toolFilePath(tc api.ToolCall) string {
	switch strings.ToLower(tc.Name) {
	case "read", "write", "edit":
		return toolTargetPath(tc.Input)
	}
	for _, k := range []string{"filePath", "file_path", "filepath", "file"} {
		if v, ok := tc.Input[k].(string); ok && v != "" {
			return filepath.Clean(v)
		}
	}
	return ""
}

// toolKeyArg is the one argument that identifies a tool call: the file it
// targets (toolFilePath), else the first non-empty of its well-known keys.
// Loop detection, the compaction summary and the review snapshot all keyed on
// "filePath" alone, so an edit sent with file_path (which the tool accepts)
// looked like a bare "edit": ten edits to ten files tripped the loop detector,
// and summaries listed "edit()" with no file.
func toolKeyArg(tc api.ToolCall) string {
	if p := toolFilePath(tc); p != "" {
		return p
	}
	for _, k := range []string{"command", "pattern", "query", "url", "name", "title", "message", "path"} {
		if v, ok := tc.Input[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// writeClaimKey is the key dispatchTools claims a write/edit path under:
// absolute against the project directory, cleaned, and case-folded on
// Windows, whose file system does not tell "X.go" from "x.go". Two
// spellings of one file must claim the same key, or two edits to it run at
// once and one change is lost.
func writeClaimKey(path, cwd string) string {
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// lastRequestIndex is the index of the latest genuine user request in msgs
// (not engine text: turn notes, nudges, compaction summaries), or -1.
func lastRequestIndex(msgs []api.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == "user" && !looksSynthetic(m) && strings.TrimSpace(m.Content) != "" {
			return i
		}
	}
	return -1
}

// originalRequest is the request a compaction keeps verbatim: the latest
// genuine user message, the one the current turn works on. It used to be
// the first one, which in a session of several tasks is task #1, not the
// current request; and compaction summaries were not recognized as engine
// text, so each compaction nested the previous <original_request> block
// inside its own. A history whose only request was already summarized away
// keeps the one the earlier compaction carried.
func originalRequest(messages []api.Message) string {
	if i := lastRequestIndex(messages); i >= 0 {
		return strings.TrimSpace(messages[i].Content)
	}
	for _, m := range messages {
		if m.Role == "user" {
			if r := carriedRequest(m.Content); r != "" {
				return r
			}
		}
	}
	return ""
}

// carriedRequest is the request inside a compaction message's
// <original_request> block, or "". Blocks nested by earlier versions are
// read from the innermost one, the request itself.
func carriedRequest(content string) string {
	if !strings.HasPrefix(strings.TrimSpace(content), "<compress") {
		return ""
	}
	end := strings.Index(content, "</original_request>")
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(content[:end], "<original_request>")
	if start < 0 {
		return ""
	}
	return strings.TrimSpace(content[start+len("<original_request>") : end])
}

// originalRequestBlock is the <original_request> block every compaction
// message starts with (the current request, see originalRequest), or ""
// when the history has no genuine request. A
// summary can misstate the task and a truncation drops it entirely (a real
// session continued from "[Context truncated]" alone); the request itself is
// short and is kept verbatim, so the model can always re-read what it was
// asked to do.
func originalRequestBlock(messages []api.Message) string {
	orig := originalRequest(messages)
	if orig == "" {
		return ""
	}
	return "<original_request>\n" + keepRunes(orig, summaryFirstUserRunes) + "\n</original_request>\n\n"
}

// keepRunes keeps the first n runes of s and marks the cut with "...": the
// summary budgets (summaryFirstUserRunes and the rest) count content kept,
// so the result is n runes plus the marker. Unlike textutil.ClipRunes, whose
// limit includes the ellipsis; it was called clipRunes too.
func keepRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return textutil.HeadRunes(s, n) + "..."
}

// countTokens is declared in token_count.go
