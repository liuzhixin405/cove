package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrepFiles_FindsMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("alpha\nbeta gamma\ndelta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles("beta", dir)
	if !strings.Contains(got, "a.md") || !strings.Contains(got, ":2:") || !strings.Contains(got, "beta gamma") {
		t.Fatalf("grepFiles missed the match, got:\n%s", got)
	}
}

func TestGrepFiles_NoMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := grepFiles("zzz", dir); got != "No matches found" {
		t.Fatalf("want %q, got %q", "No matches found", got)
	}
}

// TestGrepFiles_NoShellInjection locks in the C-3 fix: a pattern containing shell
// metacharacters must be treated as literal search data and NEVER executed. If the
// old shell-based implementation were reintroduced, the marker file would be created
// on a Unix shell and this test would fail.
func TestGrepFiles_NoShellInjection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "PWNED")
	payloads := []string{
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"; touch " + marker,
		"& echo x > " + marker,
		"| touch " + marker,
	}
	for _, p := range payloads {
		_ = grepFiles(p, dir) // must not execute anything
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("shell injection was executed: marker file %s got created", marker)
	}
}
