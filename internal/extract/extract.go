package extract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
)

// Runner manages automatic memory extraction after each turn.
type Runner struct {
	provider    api.Provider
	model       string
	memoryDir   string
	lastExtract time.Time
	OnSave      func(count int) // optional callback when memories are saved
}

// NewRunner creates an extract memories runner.
func NewRunner(provider api.Provider, model string) *Runner {
	home, _ := os.UserHomeDir()
	return &Runner{
		provider:  provider,
		model:     model,
		memoryDir: filepath.Join(home, ".cove", "memory"),
	}
}

// minExtractInterval prevents extraction from firing on every single turn.
const minExtractInterval = 2 * time.Minute

// Extract analyzes the recent conversation and saves any important memories.
// Should be called after each turn ends (runs as a background goroutine).
func (r *Runner) Extract(ctx context.Context, messages []api.Message) {
	// Throttle: don't extract more often than every 2 minutes
	if time.Since(r.lastExtract) < minExtractInterval {
		return
	}
	r.lastExtract = time.Now()

	// Need at least a few messages to extract from
	if len(messages) < 4 {
		return
	}

	// Take the last N messages as context (not the entire history)
	window := messages
	if len(window) > 20 {
		window = window[len(window)-20:]
	}

	prompt := buildExtractionPrompt(r.memoryDir, window)

	resp, err := r.provider.Chat(ctx, api.ChatRequest{
		Model:      r.model,
		Messages:   []api.Message{{Role: "user", Content: prompt}},
		SystemBase: extractSystemPrompt,
		MaxTokens:  4000,
	})
	if err != nil {
		log.Warnf("[extractMemories] API error: %v", err)
		return
	}

	// Parse and save memories from the response
	memories := parseExtractResponse(resp.Content)
	if len(memories) == 0 {
		return
	}

	os.MkdirAll(r.memoryDir, 0700)
	saved := 0
	for _, m := range memories {
		if m.Name == "" || m.Content == "" {
			continue
		}
		// Validate: memory must not be too large
		if len(m.Content) > 5000 {
			m.Content = m.Content[:5000] + "\n... [truncated]"
		}
		path := filepath.Join(r.memoryDir, sanitizeFilename(m.Name))
		// Deduplication: skip if >80% similar to existing memory
		if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
			existingStr := string(existing)
			if similarity(existingStr[:min(100, len(existingStr))], m.Content[:min(100, len(m.Content))]) > 0.8 {
				// Merge instead of duplicate
				m.Append = true
			}
		}
		if m.Append {
			existing, _ := os.ReadFile(path)
			if len(existing) > 0 {
				m.Content = string(existing) + "\n" + m.Content
			}
		}
		// Enforce per-file size limit (10KB)
		if len(m.Content) > 10240 {
			m.Content = m.Content[:10240] + "\n... [truncated to 10KB]"
		}
		if err := os.WriteFile(path, []byte(m.Content), 0644); err != nil {
			log.Warnf("[extractMemories] write failed: %v", err)
			continue
		}
		saved++
	}
	if saved > 0 {
		log.Debugf("[extractMemories] saved %d memories", saved)
		if r.OnSave != nil {
			r.OnSave(saved)
		}
	}
}

const extractSystemPrompt = `You are a memory extraction agent. Your job is to identify important facts, decisions, and context from a conversation that would be useful in future sessions.

Rules:
- Only extract DURABLE facts (things that will still be true next week)
- Skip transient info (current errors being debugged, temporary states)
- Skip things already obvious from the codebase
- Prefer updating existing memory files over creating new ones
- Each memory should be a standalone fact, not a conversation fragment
- Use descriptive filenames like "project-architecture.md" or "api-conventions.md"

Reply in this format (0 or more entries):
---MEMORY---
FILE: <filename.md>
MODE: write|append
CONTENT:
<content>
---END---`

type memoryEntry struct {
	Name    string
	Content string
	Append  bool
}

func buildExtractionPrompt(memDir string, messages []api.Message) string {
	var sb strings.Builder
	sb.WriteString("Review this recent conversation and extract any important information worth remembering for future sessions.\n\n")

	// Show existing memories so the model knows what's already saved
	sb.WriteString("## Existing memories:\n")
	entries, _ := os.ReadDir(memDir)
	if len(entries) == 0 {
		sb.WriteString("(none yet)\n")
	} else {
		for _, e := range entries {
			if !e.IsDir() {
				sb.WriteString(fmt.Sprintf("- %s\n", e.Name()))
			}
		}
	}

	sb.WriteString("\n## Recent conversation:\n")
	for _, m := range messages {
		role := m.Role
		content := m.Content
		if len(content) > 500 {
			content = content[:500] + "..."
		}
		if role == "tool" {
			if len(content) > 200 {
				content = content[:200] + "..."
			}
		}
		sb.WriteString(fmt.Sprintf("[%s] %s\n", role, content))
	}

	sb.WriteString("\nExtract important durable facts. If nothing worth saving, reply with just: NONE")
	return sb.String()
}

func parseExtractResponse(response string) []memoryEntry {
	if strings.TrimSpace(response) == "NONE" {
		return nil
	}

	var entries []memoryEntry
	parts := strings.Split(response, "---MEMORY---")
	for _, part := range parts[1:] { // skip first empty part
		endIdx := strings.Index(part, "---END---")
		if endIdx < 0 {
			endIdx = len(part)
		}
		block := part[:endIdx]

		var entry memoryEntry
		lines := strings.Split(block, "\n")
		contentStart := -1
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "FILE:") {
				entry.Name = strings.TrimSpace(line[5:])
			} else if strings.HasPrefix(line, "MODE:") {
				mode := strings.TrimSpace(line[5:])
				entry.Append = mode == "append"
			} else if strings.HasPrefix(line, "CONTENT:") {
				contentStart = i + 1
				break
			}
		}
		if contentStart > 0 && contentStart < len(lines) {
			entry.Content = strings.TrimSpace(strings.Join(lines[contentStart:], "\n"))
		}
		if entry.Name != "" && entry.Content != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

func sanitizeFilename(name string) string {
	// Remove path separators and dangerous characters
	name = filepath.Base(name)
	replacer := strings.NewReplacer(
		"/", "-", "\\", "-", ":", "-", "*", "-",
		"?", "-", "\"", "-", "<", "-", ">", "-", "|", "-",
	)
	name = replacer.Replace(name)
	if name == "" || name == "." || name == ".." {
		name = "memory.md"
	}
	// Ensure it has an extension
	if !strings.Contains(name, ".") {
		name += ".md"
	}
	return name
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// similarity computes a simple overlap coefficient between two strings.
func similarity(a, b string) float64 {
	a = strings.TrimSpace(strings.ToLower(a))
	b = strings.TrimSpace(strings.ToLower(b))
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if a == b {
		return 1
	}

	ar := []rune(a)
	br := []rune(b)
	dist := levenshtein(ar, br)
	maxLen := len(ar)
	if len(br) > maxLen {
		maxLen = len(br)
	}
	if maxLen == 0 {
		return 0
	}
	score := 1.0 - float64(dist)/float64(maxLen)

	if strings.Contains(a, b) || strings.Contains(b, a) {
		shorter := len(ar)
		longer := len(br)
		if shorter > longer {
			shorter, longer = longer, shorter
		}
		containment := float64(shorter) / float64(longer)
		if containment > score {
			score = containment
		}
	}

	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := 0; j <= len(b); j++ {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}

			insertCost := curr[j-1] + 1
			deleteCost := prev[j] + 1
			replaceCost := prev[j-1] + cost

			best := insertCost
			if deleteCost < best {
				best = deleteCost
			}
			if replaceCost < best {
				best = replaceCost
			}
			curr[j] = best
		}
		prev, curr = curr, prev
	}

	return prev[len(b)]
}
