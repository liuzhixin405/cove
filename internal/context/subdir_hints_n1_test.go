package context

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A hint file up to 12KB is injected whole (the cap used to be 2000 bytes,
// which cut most real AGENTS.md files in half).
func TestSubdirHintsKeepTwelveKilobytes(t *testing.T) {
	work := t.TempDir()
	sub := filepath.Join(work, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 11*1024) + "END-MARKER"
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := NewSubdirHints(work).CheckPath(filepath.Join(sub, "a.go"))
	if !strings.Contains(got, "END-MARKER") || strings.Contains(got, "truncated") {
		t.Fatalf("an 11KB hint file was truncated (len %d)", len(got))
	}
}

// Reset forgets what was shown, so after compaction (which may have
// summarized the hint away) the next touch injects it again.
func TestSubdirHintsResetShowsHintsAgain(t *testing.T) {
	work := t.TempDir()
	sub := filepath.Join(work, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("rules"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewSubdirHints(work)
	if h.CheckPath(filepath.Join(sub, "a.go")) == "" {
		t.Fatal("first touch: no hint")
	}
	if h.CheckPath(filepath.Join(sub, "b.go")) != "" {
		t.Fatal("second touch: hint repeated")
	}
	h.Reset()
	if !strings.Contains(h.CheckPath(filepath.Join(sub, "a.go")), "rules") {
		t.Fatal("after Reset: hint not shown again")
	}
}

// One call injects at most 24KB of hint files; what did not fit is shown by
// a later call.
func TestSubdirHintsPerCallCap(t *testing.T) {
	work := t.TempDir()
	deep := filepath.Join(work, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(work, "a"), filepath.Join(work, "a", "b"), deep} {
		for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
			if err := os.WriteFile(filepath.Join(d, name), []byte(strings.Repeat("r", 10*1024)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	h := NewSubdirHints(work)
	first := h.CheckPath(filepath.Join(deep, "x.go"))
	if len(first) > maxHintCallBytes+1024 || first == "" {
		t.Fatalf("first call injected %d bytes, cap %d", len(first), maxHintCallBytes)
	}
	second := h.CheckPath(filepath.Join(deep, "y.go"))
	if second == "" {
		t.Fatal("hints that did not fit were never shown")
	}
}
