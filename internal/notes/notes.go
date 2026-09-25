package notes

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/config"
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

// New creates a session notes manager for projectDir. The notes live in the
// project's data directory (~/.cove/projects/<hash>/session_notes.md); they
// used to be written into a .cove directory inside the repository itself,
// and a file left there by an older version is moved over.
func New(projectDir string) *SessionNotes {
	dir, err := config.ProjectDataDir(projectDir)
	if err != nil {
		// No config directory: keep the notes in memory only.
		return &SessionNotes{entries: make([]NoteEntry, 0)}
	}
	path := filepath.Join(dir, notesFileName)
	migrateLegacyNotes(filepath.Join(projectDir, ".cove"), path)
	return &SessionNotes{path: path, entries: make([]NoteEntry, 0)}
}

const notesFileName = "session_notes.md"

// migrateLegacyNotes moves <project>/.cove/session_notes.md to path, unless
// path already exists (then the newer file wins and the old one is removed
// only if it holds the same notes), and removes the .cove directory when
// nothing else is in it.
func migrateLegacyNotes(legacyDir, path string) {
	legacy := filepath.Join(legacyDir, notesFileName)
	data, err := os.ReadFile(legacy)
	if err != nil {
		return
	}
	if cur, err := os.ReadFile(path); err == nil {
		if string(cur) != string(data) {
			return
		}
	} else if err := fsatomic.WriteFile(path, data, 0o644); err != nil {
		return
	}
	if err := os.Remove(legacy); err != nil {
		return
	}
	if rest, err := os.ReadDir(legacyDir); err == nil && len(rest) == 0 {
		_ = os.Remove(legacyDir)
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

// Add records a new note. A note with the same category and text as one
// already kept is dropped: the same decision used to fill the file.
func (s *SessionNotes) Add(category, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hasLocked(category, text) {
		return
	}
	s.entries = append(s.entries, NoteEntry{
		Timestamp: time.Now(),
		Category:  category,
		Text:      text,
	})
	s.modified = true
}

func (s *SessionNotes) hasLocked(category, text string) bool {
	for _, e := range s.entries {
		if e.Category == category && e.Text == text {
			return true
		}
	}
	return false
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
	if !s.modified || len(s.entries) == 0 || s.path == "" {
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
			freed += len(s.entries[drop].Text) + len("- ["+noteTimeLayout+"] \n")
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
			fmt.Fprintf(&sb, "- [%s] %s\n", e.Timestamp.Format(noteTimeLayout), e.Text)
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
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	// Lines of the old "- [15:04] text" format carry no date: they get the
	// file's modification day.
	day := time.Now()
	if info, err := os.Stat(s.path); err == nil {
		day = info.ModTime()
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
		// Parse "- [2006-01-02 15:04] text" (or the old "- [15:04] text")
		closeBracket := strings.Index(trimmed, "] ")
		if closeBracket < 0 {
			continue
		}
		text := strings.TrimSpace(trimmed[closeBracket+2:])
		cat := currentCategory
		if cat == "" {
			cat = "task"
		}
		loaded = append(loaded, NoteEntry{
			Timestamp: parseNoteTime(trimmed[len("- ["):closeBracket], day),
			Category:  cat,
			Text:      text,
		})
	}

	if len(loaded) == 0 {
		return
	}
	s.mu.Lock()
	for _, e := range loaded {
		if !s.hasLocked(e.Category, e.Text) {
			s.entries = append(s.entries, e)
		}
	}
	s.mu.Unlock()
}

// noteTimeLayout is how a note's time is written. It used to be "15:04"
// alone, and every loaded note was then stamped with the load time.
const noteTimeLayout = "2006-01-02 15:04"

// parseNoteTime reads a note's bracketed time: a full date and time, or the
// old hour and minute on day. Unparseable text gives day itself.
func parseNoteTime(s string, day time.Time) time.Time {
	if t, err := time.ParseInLocation(noteTimeLayout, s, time.Local); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	}
	return day
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
