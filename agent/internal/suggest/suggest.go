package suggest

import (
	"context"
	"strings"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/log"
)

// Runner generates follow-up action suggestions after each turn.
type Runner struct {
	provider api.Provider
	model    string
}

// NewRunner creates a prompt suggestion runner.
func NewRunner(provider api.Provider, model string) *Runner {
	return &Runner{provider: provider, model: model}
}

// Suggestion is a proposed next action for the user.
type Suggestion struct {
	Text string
}

// Generate produces suggestions based on recent conversation.
// Returns up to 3 short suggestions or nil if nothing useful.
func (r *Runner) Generate(ctx context.Context, messages []api.Message) []Suggestion {
	if len(messages) < 2 {
		return nil
	}

	// Only look at the last few messages
	window := messages
	if len(window) > 10 {
		window = window[len(window)-10:]
	}

	var sb strings.Builder
	sb.WriteString("Based on this conversation, suggest 1-3 natural follow-up actions the user might want to take next. Be brief (under 60 chars each). If there's nothing obvious, reply NONE.\n\n")
	for _, m := range window {
		content := m.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		sb.WriteString("[" + m.Role + "] " + content + "\n")
	}
	sb.WriteString("\nFormat: one suggestion per line, prefixed with '- '")

	resp, err := r.provider.Chat(ctx, api.ChatRequest{
		Model:      r.model,
		Messages:   []api.Message{{Role: "user", Content: sb.String()}},
		SystemBase: "You suggest brief follow-up coding actions. Be concise. Each suggestion should be actionable and under 60 characters.",
		MaxTokens:  300,
	})
	if err != nil {
		log.Debugf("[suggest] API error: %v", err)
		return nil
	}

	return parseSuggestions(resp.Content)
}

func parseSuggestions(response string) []Suggestion {
	if strings.TrimSpace(response) == "NONE" {
		return nil
	}

	var suggestions []Suggestion
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- ") {
			text := strings.TrimPrefix(line, "- ")
			if len(text) > 0 && len(text) <= 80 {
				suggestions = append(suggestions, Suggestion{Text: text})
			}
		}
		if len(suggestions) >= 3 {
			break
		}
	}
	return suggestions
}
