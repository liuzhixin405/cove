package engine

import (
	"context"
	"fmt"
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

// SetThreshold overrides the trigger threshold (0.0–1.0).
func (cc *ChatCompressor) SetThreshold(t float64) { cc.tokenThreshold = t }

// SetKeepFraction overrides how many recent messages to preserve.
func (cc *ChatCompressor) SetKeepFraction(f float64) { cc.keepFraction = f }

// Disable turns off compression entirely.
func (cc *ChatCompressor) Disable() { cc.enabled = false }

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

	splitIdx := len(messages) - keepCount
	// Never split inside an assistant→tool pair
	for splitIdx > 1 && splitIdx < len(messages) {
		if messages[splitIdx].Role == "tool" {
			splitIdx--
		} else {
			break
		}
	}
	if splitIdx <= 1 {
		return &CompressResult{}, messages // nothing worth summarizing
	}

	history := messages[1:splitIdx]
	if len(history) < 4 {
		return &CompressResult{}, messages
	}

	summary, err := cc.generateSummary(ctx, history, tryChat)
	if err != nil {
		log.Warnf("compressor: summary generation failed, falling back to truncation: %v", err)
		// Fallback: simple truncation
		result := &CompressResult{
			Compressed:   true,
			OldCount:     len(messages),
			NewCount:     len(history) + keepCount,
			TokenSavings: 0,
		}
		truncated := make([]api.Message, 0, 2+keepCount)
		truncated = append(truncated, messages[0]) // system
		truncated = append(truncated, api.Message{
			Role:    "user",
			Content: "[Context truncated due to length. Continue the task.]",
		})
		truncated = append(truncated, messages[splitIdx:]...)
		result.NewCount = len(truncated)
		return result, truncated
	}

	// Build compressed message list
	compressed := make([]api.Message, 0, 2+keepCount)
	compressed = append(compressed, messages[0]) // system / first message
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
			messages[i].Content = messages[i].Content[:100] + "..."
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
			if len(content) > 100 {
				content = content[:100] + "..."
			}
		} else if len(content) > 250 {
			content = content[:250] + "..."
		}
		summaryInput.WriteString(content)
		if len(m.ToolCalls) > 0 {
			for _, tc := range m.ToolCalls {
				if path, ok := tc.Input["filePath"].(string); ok {
					summaryInput.WriteString(fmt.Sprintf(" → %s(%s)", tc.Name, path))
				} else if cmd, ok := tc.Input["command"].(string); ok {
					if len(cmd) > 60 {
						cmd = cmd[:60] + "..."
					}
					summaryInput.WriteString(fmt.Sprintf(" → bash(%s)", cmd))
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

// countTokens is declared in engine.go (1 token ≈ 4 chars)
