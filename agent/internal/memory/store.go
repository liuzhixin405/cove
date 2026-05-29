package memory

import (
	"fmt"
	"os"
	"path/filepath"
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
			filepath.Join(home, ".agentgo", "memory"),
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
