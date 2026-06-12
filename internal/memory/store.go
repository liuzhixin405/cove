package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// MaxIndexLines is the maximum number of lines for any single memory file.
	MaxIndexLines = 200
	// MaxEntryBytes is the maximum size in bytes per memory entry.
	MaxEntryBytes = 25 * 1024 // 25KB
	// MaxTotalBytes is the maximum total size across all memory files.
	MaxTotalBytes = 100 * 1024 // 100KB
)

type Store struct {
	dirs []string

	// BM25 keyword search (no embedding required)
	bm25 *BM25

	// Cache to avoid repeated disk reads on every system prompt build
	mu          sync.Mutex
	cachedAll   []Entry
	cacheTime   time.Time
	cacheTTL    time.Duration
	promptCache string
	promptDirty bool
}

func NewStore() *Store {
	home, _ := os.UserHomeDir()
	return &Store{
		dirs: []string{
			filepath.Join(home, ".cove", "memory"),
		},
		cacheTTL:    30 * time.Second,
		promptDirty: true,
	}
}

func (s *Store) AddDir(dir string) {
	s.dirs = append(s.dirs, dir)
}

func (s *Store) All() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Return cached if fresh
	if s.cachedAll != nil && time.Since(s.cacheTime) < s.cacheTTL {
		return s.cachedAll
	}

	var entries []Entry
	seen := map[string]bool{}
	for _, dir := range s.dirs {
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			path := filepath.Join(dir, f.Name())
			if seen[path] {
				continue
			}
			seen[path] = true
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			entries = append(entries, Entry{
				Name:    f.Name(),
				Path:    path,
				Content: string(data),
			})
		}
	}

	cwd, _ := os.Getwd()
	if cwd != "" {
		s.loadCLAUDEMD(cwd, &entries, &seen)
		s.loadCLAUDEMD(filepath.Join(cwd, ".claude"), &entries, &seen)
	}

	s.cachedAll = entries
	s.cacheTime = time.Now()
	s.promptDirty = true
	return entries
}

func (s *Store) loadCLAUDEMD(dir string, entries *[]Entry, seen *map[string]bool) {
	path := filepath.Join(dir, "CLAUDE.md")
	if (*seen)[path] {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	(*seen)[path] = true
	*entries = append(*entries, Entry{
		Name:    "CLAUDE.md",
		Path:    path,
		Content: string(data),
		Project: true,
	})
}

// BuildPromptFor generates a prompt snippet with only memories relevant to
// the user's message, using BM25 keyword search (no embeddings required).
func (s *Store) BuildPromptFor(ctx context.Context, userMessage string) string {
	entries := s.All()
	if len(entries) == 0 {
		return ""
	}

	// Initialize BM25 index if needed
	s.mu.Lock()
	if s.bm25 == nil {
		s.bm25 = NewBM25(1.2, 0.75)
	}
	bm25 := s.bm25
	s.mu.Unlock()

	// Re-index with current entries
	now := time.Now()
	bm25.Clear()
	for i, e := range entries {
		bm25.Index(i, e.Content, now)
	}

	// Search for relevant memories
	results := bm25.Search(userMessage, 5)
	if len(results) == 0 {
		// No relevant memories found; include INDEX.md summary
		return s.buildIndexPrompt()
	}

	// Blend BM25 score with recency (48h decay)
	var blended []scoredAndEntry
	for _, r := range results {
		blended = append(blended, scoredAndEntry{
			entry: entries[r.ID],
			score: r.CombinedScore(now, 48),
		})
	}
	sort.Slice(blended, func(i, j int) bool {
		return blended[i].score > blended[j].score
	})

	// Build prompt with top results, deduplicated by memory name
	seen := make(map[string]bool)
	var sb strings.Builder
	sb.WriteString("\n\n<user_memories>\n")
	for _, b := range blended {
		if seen[b.entry.Name] {
			continue
		}
		seen[b.entry.Name] = true
		sb.WriteString("<memory>\n")
		sb.WriteString("<name>" + b.entry.Name + "</name>\n")
		sb.WriteString("<content>\n")
		sb.WriteString(b.entry.Content)
		sb.WriteString("\n</content>\n")
		sb.WriteString("</memory>\n")
	}
	sb.WriteString("</user_memories>\n")
	return sb.String()
}

type scoredAndEntry struct {
	entry Entry
	score float64
}

// buildIndexPrompt builds a prompt containing only the INDEX.md content.
func (s *Store) buildIndexPrompt() string {
	entries := s.All()
	for _, e := range entries {
		if strings.EqualFold(e.Name, "INDEX.md") {
			return "\n\n<user_memories>\n<memory>\n<name>" + e.Name + "</name>\n<content>\n" + e.Content + "\n</content>\n</memory>\n</user_memories>\n"
		}
	}
	return ""
}

func (s *Store) BuildPrompt() string {
	entries := s.All()
	if len(entries) == 0 {
		return ""
	}

	s.mu.Lock()
	if !s.promptDirty && s.promptCache != "" {
		cached := s.promptCache
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("\n\n<user_memories>\n")
	for _, e := range entries {
		sb.WriteString("<memory>\n")
		sb.WriteString("<name>" + e.Name + "</name>\n")
		sb.WriteString("<content>\n")
		sb.WriteString(e.Content)
		sb.WriteString("\n</content>\n")
		sb.WriteString("</memory>\n")
	}
	sb.WriteString("</user_memories>\n")

	result := sb.String()
	s.mu.Lock()
	s.promptCache = result
	s.promptDirty = false
	s.mu.Unlock()
	return result
}

// EntryMatch is one ranked memory entry returned by Search.
type EntryMatch struct {
	Entry Entry
	Score float64
}

// Search runs BM25 keyword retrieval over all memory entries and returns the
// top matches ranked by relevance (blended with recency). No embeddings are
// used. Results are deduplicated by entry name.
func (s *Store) Search(query string, topK int) []EntryMatch {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil
	}
	if topK <= 0 {
		topK = 5
	}
	entries := s.All()
	if len(entries) == 0 {
		return nil
	}

	s.mu.Lock()
	if s.bm25 == nil {
		s.bm25 = NewBM25(1.2, 0.75)
	}
	bm25 := s.bm25
	s.mu.Unlock()

	now := time.Now()
	bm25.Clear()
	for i, e := range entries {
		bm25.Index(i, e.Content, now)
	}

	scored := bm25.Search(query, topK*2)
	seen := make(map[string]bool)
	var results []EntryMatch
	for _, r := range scored {
		if r.ID < 0 || r.ID >= len(entries) {
			continue
		}
		e := entries[r.ID]
		if seen[e.Name] {
			continue
		}
		seen[e.Name] = true
		results = append(results, EntryMatch{Entry: e, Score: r.CombinedScore(now, 48)})
		if len(results) >= topK {
			break
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	return results
}

// Stats summarizes the memory store contents for observability.
type Stats struct {
	FileCount     int
	ProjectCount  int
	TotalBytes    int
	TotalLines    int
	MaxEntryBytes int
	MaxTotalBytes int
}

// Stats returns aggregate statistics over all memory entries.
func (s *Store) Stats() Stats {
	entries := s.All()
	st := Stats{
		MaxEntryBytes: MaxEntryBytes,
		MaxTotalBytes: MaxTotalBytes,
	}
	for _, e := range entries {
		st.FileCount++
		if e.Project {
			st.ProjectCount++
		}
		st.TotalBytes += len(e.Content)
		st.TotalLines += strings.Count(e.Content, "\n") + 1
	}
	return st
}

type Entry struct {
	Name    string
	Path    string
	Content string
	Project bool
}

func (s *Store) Save(name, content string) error {
	// Validate entry size
	if len(content) > MaxEntryBytes {
		return fmt.Errorf("memory entry %q exceeds max size (%d > %d bytes)", name, len(content), MaxEntryBytes)
	}
	// Validate line count
	lines := strings.Count(content, "\n") + 1
	if lines > MaxIndexLines {
		return fmt.Errorf("memory entry %q exceeds max lines (%d > %d)", name, lines, MaxIndexLines)
	}

	var dir string
	for _, d := range s.dirs {
		if _, err := os.Stat(d); err == nil {
			dir = d
			break
		}
	}
	if dir == "" {
		dir = s.dirs[0]
		os.MkdirAll(dir, 0700)
	}

	// Check total size would not exceed limit
	totalSize := 0
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err == nil {
			if e.Name() == name {
				continue // replacing this file, don't count old size
			}
			totalSize += int(info.Size())
		}
	}
	if totalSize+len(content) > MaxTotalBytes {
		return fmt.Errorf("total memory size would exceed limit (%d + %d > %d bytes)", totalSize, len(content), MaxTotalBytes)
	}

	err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	if err == nil {
		s.invalidateCache()
	}
	return err
}

// TruncateEntry truncates content to fit within limits.
func TruncateEntry(content string) string {
	// Truncate by lines
	lines := strings.Split(content, "\n")
	if len(lines) > MaxIndexLines {
		lines = lines[:MaxIndexLines]
		content = strings.Join(lines, "\n") + "\n... [truncated to 200 lines]"
	}
	// Truncate by bytes
	if len(content) > MaxEntryBytes {
		content = content[:MaxEntryBytes-50] + "\n... [truncated to 25KB]"
	}
	return content
}

func (s *Store) Delete(name string) error {
	for _, d := range s.dirs {
		path := filepath.Join(d, name)
		if _, err := os.Stat(path); err == nil {
			err := os.Remove(path)
			if err == nil {
				s.invalidateCache()
			}
			return err
		}
	}
	return nil
}

func (s *Store) invalidateCache() {
	s.mu.Lock()
	s.cachedAll = nil
	s.cacheTime = time.Time{}
	s.promptDirty = true
	s.promptCache = ""
	s.mu.Unlock()
}
