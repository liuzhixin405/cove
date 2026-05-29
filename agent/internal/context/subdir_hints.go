package context

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// HintFiles that are looked for in subdirectories.
var hintFileNames = []string{
	"AGENTS.md", "CLAUDE.md", ".cursorrules", ".agentgo.md",
}

// SubdirHints tracks discovered subdirectory context files.
type SubdirHints struct {
	mu      sync.Mutex
	seen    map[string]bool   // directories already checked
	loaded  map[string]string // path → content
	workDir string
}

// NewSubdirHints creates a new subdirectory hints tracker.
func NewSubdirHints(workDir string) *SubdirHints {
	return &SubdirHints{
		seen:    make(map[string]bool),
		loaded:  make(map[string]string),
		workDir: workDir,
	}
}

// CheckPath extracts directory from a file path and discovers hint files.
// Returns any newly found hint content to inject, or empty string.
func (h *SubdirHints) CheckPath(path string) string {
	if path == "" {
		return ""
	}

	// Resolve to absolute
	if !filepath.IsAbs(path) {
		path = filepath.Join(h.workDir, path)
	}

	// Get the directory (if path is a file, use its parent)
	dir := path
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		dir = filepath.Dir(path)
	}

	return h.checkDir(dir)
}

// CheckCommand extracts paths from a shell command and discovers hints.
func (h *SubdirHints) CheckCommand(cmd string) string {
	var results []string
	// Extract path-like tokens from the command
	for _, token := range strings.Fields(cmd) {
		if strings.HasPrefix(token, "-") {
			continue
		}
		if strings.Contains(token, "/") || strings.Contains(token, "\\") {
			if hint := h.CheckPath(token); hint != "" {
				results = append(results, hint)
			}
		}
	}
	return strings.Join(results, "\n")
}

func (h *SubdirHints) checkDir(dir string) string {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Walk up to 5 parent levels looking for hint files
	var newHints []string
	current := filepath.Clean(dir)
	workClean := filepath.Clean(h.workDir)

	for depth := 0; depth < 5; depth++ {
		if current == workClean || current == filepath.Dir(current) {
			break // reached workspace root or filesystem root
		}

		if h.seen[current] {
			break // already checked from here up
		}
		h.seen[current] = true

		for _, name := range hintFileNames {
			fp := filepath.Join(current, name)
			if _, exists := h.loaded[fp]; exists {
				continue
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				continue
			}
			content := strings.TrimSpace(string(data))
			if content == "" {
				continue
			}
			// Limit size to prevent context explosion
			if len(content) > 2000 {
				content = content[:2000] + "\n[...truncated]"
			}
			h.loaded[fp] = content
			relPath, _ := filepath.Rel(h.workDir, fp)
			if relPath == "" {
				relPath = fp
			}
			newHints = append(newHints, "\n[Context from "+relPath+"]\n"+content)
		}

		current = filepath.Dir(current)
	}

	if len(newHints) == 0 {
		return ""
	}
	return strings.Join(newHints, "\n")
}

// AllLoaded returns all discovered hint file paths.
func (h *SubdirHints) AllLoaded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var paths []string
	for p := range h.loaded {
		paths = append(paths, p)
	}
	return paths
}
