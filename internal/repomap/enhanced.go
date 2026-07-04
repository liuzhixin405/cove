package repomap

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DiffResult captures repository changes since last scan.
type DiffResult struct {
	Added    []string
	Removed  []string
	Modified []string
}

// Summary returns a human-readable diff summary.
func (dr *DiffResult) Summary() string {
	var parts []string
	if len(dr.Added) > 0 {
		parts = append(parts, "+"+strings.Join(truncateList(dr.Added, 5), ", "))
	}
	if len(dr.Modified) > 0 {
		parts = append(parts, "~"+strings.Join(truncateList(dr.Modified, 5), ", "))
	}
	if len(dr.Removed) > 0 {
		parts = append(parts, "-"+strings.Join(truncateList(dr.Removed, 5), ", "))
	}
	if len(parts) == 0 {
		return "no changes"
	}
	return strings.Join(parts, "; ")
}

func truncateList(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	result := make([]string, max+1)
	copy(result, items[:max])
	result[max] = "..."
	return result
}

// EnhancedGenerator wraps the base RepoMap Generator with incremental change
// detection and full-output caching, so unchanged turns are free and changed
// turns still get the real AST-derived, reference-ranked map instead of a
// flat alphabetical file list.
//
// Historically this type's own buildMap() reimplemented output generation
// from scratch using only a sorted file-path list — no symbol extraction, no
// reference ranking — and because Engine always constructs an
// EnhancedGenerator (see internal/engine/engine.go), that flat version
// silently and unconditionally shadowed the real repomap.Generator output
// every single turn; repomap.Generator's richer result was computed once at
// session start (internal/context/context.go's Collect()) and then never
// actually used. gen (a *Generator) now does the real parsing/ranking work,
// reusing its own file-level parseCache so unchanged files aren't
// re-parsed; EnhancedGenerator's job is purely the higher-level
// "did anything change at all, and if not, skip regeneration entirely"
// decision plus surfacing a change summary.
type EnhancedGenerator struct {
	root       string
	mu         sync.RWMutex
	fileMTimes map[string]time.Time // path -> last known mtime
	cache      string               // cached repo map output
	cacheValid bool
	gen        *Generator // real AST parsing + reference-count ranking
}

// NewEnhancedGenerator creates an enhanced repo map generator.
func NewEnhancedGenerator(root string) *EnhancedGenerator {
	return &EnhancedGenerator{
		root:       root,
		fileMTimes: make(map[string]time.Time),
		gen:        NewGenerator(root),
	}
}

// GenerateIncremental produces a repo map, using incremental updates when possible.
// Returns the map text and any diff from the previous state.
func (eg *EnhancedGenerator) GenerateIncremental(maxFiles int) (string, *DiffResult) {
	eg.mu.Lock()
	defer eg.mu.Unlock()

	current := eg.scanFiles()
	diff := eg.computeDiff(current)

	// If no changes and cache is valid, return cached result — the common
	// case, and the reason this type exists: skip re-parsing/re-ranking
	// entirely rather than just skipping the final formatting step.
	if eg.cacheValid && !diffHasChanges(diff) {
		return eg.cache, &DiffResult{}
	}

	wasValidBefore := eg.cacheValid
	eg.fileMTimes = current

	mapText := FormatFileMaps(eg.gen.BuildRanked(maxFiles))

	// Surface what changed since the last time the model saw this map,
	// rather than discarding that information (the one genuine advantage
	// the old flat-listing implementation had).
	if wasValidBefore && diffHasChanges(diff) {
		mapText += "\nChanges since last check: " + diff.Summary() + "\n"
	}

	eg.cache = mapText
	eg.cacheValid = true

	return mapText, diff
}

// scanFiles walks the repo and returns file -> mtime map.
func (eg *EnhancedGenerator) scanFiles() map[string]time.Time {
	result := make(map[string]time.Time)
	filepath.Walk(eg.root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") && name != "." {
				return filepath.SkipDir
			}
			if name == "node_modules" || name == "vendor" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		// Only track source files
		ext := strings.ToLower(filepath.Ext(path))
		if isSourceFile(ext) {
			rel, _ := filepath.Rel(eg.root, path)
			result[rel] = info.ModTime()
		}
		return nil
	})
	return result
}

// computeDiff compares current file state against previous.
func (eg *EnhancedGenerator) computeDiff(current map[string]time.Time) *DiffResult {
	dr := &DiffResult{}

	// Added and modified
	for path, mtime := range current {
		oldMtime, existed := eg.fileMTimes[path]
		if !existed {
			dr.Added = append(dr.Added, path)
		} else if mtime.After(oldMtime) {
			dr.Modified = append(dr.Modified, path)
		}
	}

	// Removed
	for path := range eg.fileMTimes {
		if _, exists := current[path]; !exists {
			dr.Removed = append(dr.Removed, path)
		}
	}

	sort.Strings(dr.Added)
	sort.Strings(dr.Modified)
	sort.Strings(dr.Removed)
	return dr
}

func isSourceFile(ext string) bool {
	switch ext {
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".java",
		".c", ".cpp", ".h", ".hpp", ".cs", ".rb", ".php", ".swift",
		".md", ".yaml", ".yml", ".json", ".toml", ".xml", ".sql",
		".sh", ".bash", ".ps1", ".dockerfile", ".makefile":
		return true
	}
	return false
}

func diffHasChanges(d *DiffResult) bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Modified) > 0
}
