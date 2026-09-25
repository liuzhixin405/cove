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

// maxHintFileBytes caps one injected hint file. It was 2000 bytes, which cut
// most real AGENTS.md files in half.
const maxHintFileBytes = 12 * 1024

// maxHintCallBytes caps what one CheckPath or CheckCommand call injects;
// files that do not fit are shown by a later call.
const maxHintCallBytes = 24 * 1024

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
	budget := maxHintCallBytes
	return h.checkPath(path, &budget)
}

// checkPath is CheckPath within budget bytes (shared by a command's paths).
func (h *SubdirHints) checkPath(path string, budget *int) string {
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

	return h.checkDir(dir, budget)
}

// Reset forgets which directories and files were already shown. The engine
// calls it after compacting the history, which may have summarized the
// injected hints away, so the next touch of a directory shows them again.
func (h *SubdirHints) Reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = make(map[string]bool)
	h.loaded = make(map[string]string)
}

// CheckCommand extracts paths from a shell command and discovers hints.
func (h *SubdirHints) CheckCommand(cmd string) string {
	var results []string
	budget := maxHintCallBytes
	// Extract path-like tokens from the command
	for _, token := range strings.Fields(cmd) {
		if strings.HasPrefix(token, "-") {
			continue
		}
		if strings.Contains(token, "/") || strings.Contains(token, "\\") {
			if hint := h.checkPath(token, &budget); hint != "" {
				results = append(results, hint)
			}
		}
	}
	return strings.Join(results, "\n")
}

func (h *SubdirHints) checkDir(dir string, budget *int) string {
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
			// Already shown; a parent may still hold files an earlier
			// call's cap left out, so keep walking up.
			current = filepath.Dir(current)
			continue
		}
		skipped := false

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
			content = textutil.ClipBytes(content, maxHintFileBytes, "\n[...truncated]")
			relPath, _ := filepath.Rel(h.workDir, fp)
			if relPath == "" {
				relPath = fp
			}
			hint := "\n[Context from " + relPath + "]\n" + content
			if len(hint) > *budget {
				// Over this call's cap: left for a later call.
				skipped = true
				continue
			}
			*budget -= len(hint)
			h.loaded[fp] = content
			newHints = append(newHints, hint)
		}
		if skipped {
			break // not seen: a later call picks up what did not fit
		}
		h.seen[current] = true

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
