package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SessionNotes maintains an auto-updating session_notes.md file that tracks
// key context from the current conversation. This helps the agent recall
// important decisions and context even after compaction.
type SessionNotes struct {
	mu       sync.Mutex
	path     string
	entries  []NoteEntry
	modified bool
}

// NoteEntry is a single note item.
type NoteEntry struct {
	Timestamp time.Time
	Category  string // "decision", "discovery", "error", "task"
	Text      string
}

// New creates a session notes manager. Notes are stored per-project.
func New(projectDir string) *SessionNotes {
	dir := filepath.Join(projectDir, ".cove")
	os.MkdirAll(dir, 0700)
	return &SessionNotes{
		path:    filepath.Join(dir, "session_notes.md"),
		entries: make([]NoteEntry, 0),
	}
}

// NewGlobal creates session notes in the global config dir.
func NewGlobal() *SessionNotes {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cove")
	os.MkdirAll(dir, 0700)
	return &SessionNotes{
		path:    filepath.Join(dir, "session_notes.md"),
		entries: make([]NoteEntry, 0),
	}
}

// Add records a new note.
func (s *SessionNotes) Add(category, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, NoteEntry{
		Timestamp: time.Now(),
		Category:  category,
		Text:      text,
	})
	s.modified = true
}

// AddDecision records a key decision.
func (s *SessionNotes) AddDecision(text string) { s.Add("decision", text) }

// AddDiscovery records a code/project discovery.
func (s *SessionNotes) AddDiscovery(text string) { s.Add("discovery", text) }

// AddError records a notable error and resolution.
func (s *SessionNotes) AddError(text string) { s.Add("error", text) }

// AddTask records task progress.
func (s *SessionNotes) AddTask(text string) { s.Add("task", text) }

// Flush writes the notes to disk if modified.
func (s *SessionNotes) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.modified || len(s.entries) == 0 {
		return nil
	}
	s.modified = false
	return s.writeToDisk()
}

func (s *SessionNotes) writeToDisk() error {
	var sb strings.Builder
	sb.WriteString("# Session Notes\n\n")
	sb.WriteString(fmt.Sprintf("_Last updated: %s_\n\n", time.Now().Format("2006-01-02 15:04")))

	// Group by category
	categories := []string{"decision", "task", "discovery", "error"}
	grouped := make(map[string][]NoteEntry)
	for _, e := range s.entries {
		grouped[e.Category] = append(grouped[e.Category], e)
	}

	for _, cat := range categories {
		entries := grouped[cat]
		if len(entries) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("## %s\n\n", titleCase(cat+"s")))
		for _, e := range entries {
			sb.WriteString(fmt.Sprintf("- [%s] %s\n", e.Timestamp.Format("15:04"), e.Text))
		}
		sb.WriteString("\n")
	}

	content := sb.String()
	// Enforce max size (25KB)
	if len(content) > 25600 {
		content = content[:25600] + "\n... [truncated]\n"
	}
	return os.WriteFile(s.path, []byte(content), 0644)
}

// Load reads existing notes from disk (for resuming sessions).
func (s *SessionNotes) Load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var currentCategory string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		// Detect category headers like "## Decisions"
		if strings.HasPrefix(trimmed, "## ") {
			header := strings.ToLower(strings.TrimPrefix(trimmed, "## "))
			switch {
			case strings.Contains(header, "decision"):
				currentCategory = "decision"
			case strings.Contains(header, "task"):
				currentCategory = "task"
			case strings.Contains(header, "discover"):
				currentCategory = "discovery"
			case strings.Contains(header, "error"):
				currentCategory = "error"
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "- [") {
			continue
		}
		// Parse "- [15:04] text"
		closeBracket := strings.Index(trimmed, "] ")
		if closeBracket < 0 {
			continue
		}
		text := trimmed[closeBracket+2:]
		cat := currentCategory
		if cat == "" {
			cat = "task"
		}
		s.entries = append(s.entries, NoteEntry{
			Timestamp: time.Now(),
			Category:  cat,
			Text:      text,
		})
	}
}

// Content returns the current notes as a string for system prompt injection.
func (s *SessionNotes) Content() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n<session_notes>\n")
	// Show last 20 entries max
	start := 0
	if len(s.entries) > 20 {
		start = len(s.entries) - 20
	}
	for _, e := range s.entries[start:] {
		sb.WriteString(fmt.Sprintf("- [%s] %s\n", e.Category, e.Text))
	}
	sb.WriteString("</session_notes>\n")
	return sb.String()
}
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
