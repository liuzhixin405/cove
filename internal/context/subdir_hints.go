package context

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// HintFiles that are looked for in subdirectories.
var hintFileNames = []string{
	"AGENTS.md", "CLAUDE.md", ".cursorrules", ".cove.md",
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
		// Only directories strictly inside the workspace. This used to stop
		// only on reaching the workspace root, so a path elsewhere (a repo
		// cloned into the temp dir, anything under home) walked up its own
		// tree and injected that tree's AGENTS.md/CLAUDE.md as instructions.
		if !strictlyInside(workClean, current) {
			break
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
			// ClipBytes, not content[:2000]: the byte cut split CJK characters
			// and injected invalid UTF-8.
			content = textutil.ClipBytes(content, 2000, "\n[...truncated]")
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

// strictlyInside reports whether dir is below root (not root itself).
// filepath.Rel compares case-insensitively on Windows, where D:\Repo and
// d:\repo are the same directory.
func strictlyInside(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || filepath.IsAbs(rel) {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
