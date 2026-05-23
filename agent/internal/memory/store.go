package memory

import (
	"os"
	"path/filepath"
	"strings"
)

type Store struct {
	dirs []string
}

func NewStore() *Store {
	home, _ := os.UserHomeDir()
	return &Store{
		dirs: []string{
			filepath.Join(home, ".agentgo", "memory"),
		},
	}
}

func (s *Store) AddDir(dir string) {
	s.dirs = append(s.dirs, dir)
}

func (s *Store) All() []Entry {
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
	return sb.String()
}

type Entry struct {
	Name    string
	Path    string
	Content string
	Project bool
}

func (s *Store) Save(name, content string) error {
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
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
}

func (s *Store) Delete(name string) error {
	for _, d := range s.dirs {
		path := filepath.Join(d, name)
		if _, err := os.Stat(path); err == nil {
			return os.Remove(path)
		}
	}
	return nil
}

