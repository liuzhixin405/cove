package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/fsatomic"
	"github.com/liuzhixin405/cove/internal/textutil"
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
	_ = os.MkdirAll(dir, 0700)
	return &SessionNotes{
		path:    filepath.Join(dir, "session_notes.md"),
		entries: make([]NoteEntry, 0),
	}
}

// NewGlobal creates session notes in the global config dir.
func NewGlobal() *SessionNotes {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cove")
	_ = os.MkdirAll(dir, 0700)
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

// maxNotesBytes caps the notes file.
const maxNotesBytes = 25600

func (s *SessionNotes) writeToDisk() error {
	// Keep the newest entries that fit. The file is per project and every
	// session loads, extends and rewrites it, so it only grows; clipping the
	// rendered file used to cut off its end — the newest notes — and the next
	// Load skipped the marker, so past the cap every new note was lost while
	// the oldest stayed forever. Dropping the oldest also bounds s.entries.
	content := s.render()
	for len(content) > maxNotesBytes && len(s.entries) > 1 {
		excess := len(content) - maxNotesBytes
		drop := 0
		for freed := 0; drop < len(s.entries)-1 && freed < excess; drop++ {
			freed += len(s.entries[drop].Text) + len("- [15:04] \n")
		}
		s.entries = append([]NoteEntry(nil), s.entries[drop:]...)
		content = s.render()
	}
	// A single entry larger than the cap: clip it on a rune boundary, since
	// notes are largely Chinese here and a byte-slice would cut a rune in half.
	content = textutil.ClipBytes(content, maxNotesBytes, "\n... [truncated]\n")
	// Atomic replace: a crash mid-write would otherwise leave a half-written
	// notes file, which Load then parses as the session's entire history.
	return fsatomic.WriteFile(s.path, []byte(content), 0644)
}

// render formats the entries as the notes file.
func (s *SessionNotes) render() string {
	var sb strings.Builder
	sb.WriteString("# Session Notes\n\n")
	fmt.Fprintf(&sb, "_Last updated: %s_\n\n", time.Now().Format("2006-01-02 15:04"))

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
		fmt.Fprintf(&sb, "## %s\n\n", titleCase(cat+"s"))
		for _, e := range entries {
			fmt.Fprintf(&sb, "- [%s] %s\n", e.Timestamp.Format("15:04"), e.Text)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// Load reads existing notes from disk (for resuming sessions).
//
// Reading the file happens outside the lock (it can block on IO), but the
// entries slice is only touched while holding it — Load used to append to
// s.entries with no lock at all, racing every Add/Flush from the engine's
// background goroutines.
func (s *SessionNotes) Load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var loaded []NoteEntry
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
		loaded = append(loaded, NoteEntry{
			Timestamp: time.Now(),
			Category:  cat,
			Text:      text,
		})
	}

	if len(loaded) == 0 {
		return
	}
	s.mu.Lock()
	s.entries = append(s.entries, loaded...)
	s.mu.Unlock()
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
		fmt.Fprintf(&sb, "- [%s] %s\n", e.Category, e.Text)
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
