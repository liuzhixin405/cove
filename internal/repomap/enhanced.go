package repomap

import (
	"fmt"
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

// EnhancedGenerator wraps the base RepoMap with incremental updates and caching.
// It tracks file modification times to avoid re-scanning unchanged files.
type EnhancedGenerator struct {
	root      string
	mu        sync.RWMutex
	fileMTimes map[string]time.Time // path -> last known mtime
	cache      string               // cached repo map output
	cacheValid bool
}

// NewEnhancedGenerator creates an enhanced repo map generator.
func NewEnhancedGenerator(root string) *EnhancedGenerator {
	return &EnhancedGenerator{
		root:       root,
		fileMTimes: make(map[string]time.Time),
	}
}

// GenerateIncremental produces a repo map, using incremental updates when possible.
// Returns the map text and any diff from the previous state.
func (eg *EnhancedGenerator) GenerateIncremental(maxFiles int) (string, *DiffResult) {
	eg.mu.Lock()
	defer eg.mu.Unlock()

	current := eg.scanFiles()
	diff := eg.computeDiff(current)

	// If no changes and cache is valid, return cached result
	if eg.cacheValid && !diffHasChanges(diff) {
		return eg.cache, &DiffResult{}
	}

	// Regenerate: build a new map from the file list
	eg.fileMTimes = current
	mapText := eg.buildMap(current, maxFiles)
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

// buildMap constructs a simple repo map from the file list.
func (eg *EnhancedGenerator) buildMap(files map[string]time.Time, maxFiles int) string {
	var sorted []string
	for f := range files {
		sorted = append(sorted, f)
	}
	sort.Strings(sorted)

	if len(sorted) > maxFiles {
		sorted = sorted[:maxFiles]
	}

	var sb strings.Builder
	sb.WriteString("Repository structure:\n")
	for _, f := range sorted {
		sb.WriteString("  ")
		sb.WriteString(f)
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\n(%d files total)", len(files)))

	return sb.String()
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
