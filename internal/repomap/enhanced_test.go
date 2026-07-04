package repomap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEnhancedGenerator_UsesRealRankedOutput is a regression test for the
// bug where EnhancedGenerator (the generator actually wired into
// Engine.SystemPrompt) silently produced a flat, unranked file-name list
// instead of the real AST-derived, reference-ranked map that
// repomap.Generator produces. If this test ever starts failing because the
// output goes back to being just file paths with no function signatures,
// that regression has reappeared.
func TestEnhancedGenerator_UsesRealRankedOutput(t *testing.T) {
	tempDir := t.TempDir()
	goCode := `package testpkg

type Widget struct {
	Name string
}

func NewWidget(name string) *Widget {
	return &Widget{Name: name}
}
`
	if err := os.WriteFile(filepath.Join(tempDir, "widget.go"), []byte(goCode), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	eg := NewEnhancedGenerator(tempDir)
	mapText, diff := eg.GenerateIncremental(50)

	if !strings.Contains(mapText, "func NewWidget(name string)") {
		t.Fatalf("expected real function signature in output (ranked generator), got flat listing:\n%s", mapText)
	}
	if !strings.Contains(mapText, "type Widget struct") {
		t.Fatalf("expected struct definition in output, got:\n%s", mapText)
	}
	if diff == nil {
		t.Fatalf("expected a non-nil diff on first generation")
	}
}

// TestEnhancedGenerator_CachesWhenNothingChanged verifies the "did anything
// change at all" fast path is preserved: a second call with no file changes
// must return the exact same cached text without needing to re-derive it.
func TestEnhancedGenerator_CachesWhenNothingChanged(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "a.go"), []byte("package a\nfunc A() {}\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	eg := NewEnhancedGenerator(tempDir)
	first, _ := eg.GenerateIncremental(50)
	second, diff := eg.GenerateIncremental(50)

	if first != second {
		t.Fatalf("expected identical cached output when nothing changed:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if diff == nil || diffHasChanges(diff) {
		t.Fatalf("expected an empty diff on the unchanged call, got: %+v", diff)
	}
}

// TestEnhancedGenerator_SurfacesChangeSummary verifies the one real
// advantage of the old implementation (change awareness) survived the fix:
// after a file is added, the next call's output mentions it.
func TestEnhancedGenerator_SurfacesChangeSummary(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "a.go"), []byte("package a\nfunc A() {}\n"), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	eg := NewEnhancedGenerator(tempDir)
	_, _ = eg.GenerateIncremental(50) // first call establishes the cached baseline

	// Ensure the new file's mtime is observably later than the first scan.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(tempDir, "b.go"), []byte("package a\nfunc B() {}\n"), 0644); err != nil {
		t.Fatalf("failed to write second test file: %v", err)
	}

	mapText, diff := eg.GenerateIncremental(50)
	if diff == nil || len(diff.Added) == 0 {
		t.Fatalf("expected the diff to report the newly added file, got: %+v", diff)
	}
	if !strings.Contains(mapText, "Changes since last check") {
		t.Fatalf("expected change summary to be surfaced in output, got:\n%s", mapText)
	}
}
