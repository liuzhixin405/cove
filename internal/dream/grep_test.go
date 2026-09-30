package dream

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGrepFiles_FindsMatches(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("alpha\nbeta gamma\ndelta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles(context.Background(), "beta", dir)
	if !strings.Contains(got, "a.md") || !strings.Contains(got, ":2:") || !strings.Contains(got, "beta gamma") {
		t.Fatalf("grepFiles missed the match, got:\n%s", got)
	}
}

func TestGrepFiles_NoMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := grepFiles(context.Background(), "zzz", dir); got != "No matches found" {
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
		_ = grepFiles(context.Background(), p, dir) // must not execute anything
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("shell injection was executed: marker file %s got created", marker)
	}
}

// A line over bufio.Scanner's 1MB limit stopped the scan with ErrTooLong,
// unchecked: everything after it was dropped and "No matches found" came
// back. Lines of any length are now read.
func TestGrepFiles_SearchesPastVeryLongLine(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("x", 1<<20+10) + "\nneedle after the long line\n"
	if err := os.WriteFile(filepath.Join(dir, "long.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles(context.Background(), "needle", dir)
	if !strings.Contains(got, ":2:needle after the long line") {
		t.Fatalf("match after a >1MB line missed, got:\n%.200s", got)
	}
}

// Files over 2MB used to be skipped entirely; they are streamed now.
func TestGrepFiles_SearchesLargeFiles(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("filler text ", 10) + "\n"
	body := strings.Repeat(line, (3<<20)/len(line)) + "needle at the end\n"
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := grepFiles(context.Background(), "needle", dir); !strings.Contains(got, "needle at the end") {
		t.Fatalf("match in a 3MB file missed, got:\n%.200s", got)
	}
}

// A newline-less file (a .vhdx, a zip, a .git pack, a minified bundle) was
// read by ReadString('\n') as one line built whole in memory: a multi-GB
// file crashed the process running dream. A line is now held in bounded
// windows; a match inside the long line (even one straddling two windows)
// is still found.
func TestGrepFiles_LongLineMemoryIsBounded(t *testing.T) {
	dir := t.TempDir()
	const size = 24 << 20
	body := make([]byte, size)
	for i := range body {
		body[i] = 'x'
	}
	at := 64*1024 - 3 // across the first window boundary
	copy(body[at:], "needle")
	if err := os.WriteFile(filepath.Join(dir, "bundle.min.js"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	body = nil
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := grepFiles(context.Background(), "needle", dir)
	runtime.ReadMemStats(&after)
	if !strings.Contains(got, ":1:") {
		t.Fatalf("match inside the long line missed, got:\n%.300s", got)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
		t.Fatalf("searching a %dMB newline-less file allocated %dMB", size>>20, alloc>>20)
	}
}

// Binary files (NUL in the first bytes) are skipped, and said so.
func TestGrepFiles_SkipsBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.vhdx"), []byte("needle\x00\x00\x01needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles(context.Background(), "needle", dir)
	if strings.Contains(got, "disk.vhdx:") {
		t.Fatalf("binary file searched, got:\n%s", got)
	}
	if !strings.Contains(got, "could not be fully searched") || !strings.Contains(got, "binary") {
		t.Fatalf("skipped binary file not reported, got:\n%s", got)
	}
}

// .git and node_modules are skipped when walking a tree, but searched when
// they are the path asked for.
func TestGrepFiles_SkipsHeavyDirsUnlessTargeted(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{".git", "node_modules"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "f.txt"), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := grepFiles(context.Background(), "needle", dir)
	if strings.Contains(got, "f.txt:") {
		t.Fatalf("heavy directory searched while walking, got:\n%s", got)
	}
	if !strings.Contains(got, ".git") || !strings.Contains(got, "node_modules") {
		t.Fatalf("skipped directories not reported, got:\n%s", got)
	}
	if got := grepFiles(context.Background(), "needle", filepath.Join(dir, ".git")); !strings.Contains(got, "f.txt:1:needle") {
		t.Fatalf("explicitly targeted .git not searched, got:\n%s", got)
	}
}

// The bytes one call scans are bounded; the search says where it stopped.
func TestGrepFiles_TotalBytesBounded(t *testing.T) {
	dir := t.TempDir()
	orig := grepMaxTotalBytes
	grepMaxTotalBytes = 64 << 10
	t.Cleanup(func() { grepMaxTotalBytes = orig })
	line := strings.Repeat("filler ", 20) + "\n"
	body := strings.Repeat(line, (256<<10)/len(line)) + "needle at the end\n"
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles(context.Background(), "needle", dir)
	if strings.Contains(got, "needle at the end") {
		t.Fatal("search ran past its byte budget")
	}
	if !strings.Contains(got, "could not be fully searched") || !strings.Contains(got, "big.md") {
		t.Fatalf("budget stop not reported, got:\n%s", got)
	}
}

// A cancelled context (dreamRunTimeout ran out) stops the walk: it used to
// run on over a huge tree while the worker held the consolidation lock.
func TestGrepFiles_StopsOnCancelledContext(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.md", i)), []byte("needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := grepFiles(ctx, "needle", dir)
	if strings.Count(got, ":1:needle") == 50 {
		t.Fatal("cancelled search still searched every file")
	}
	if !strings.Contains(got, "cancel") {
		t.Fatalf("cancellation not reported, got:\n%s", got)
	}
}

// A file that could not be read is reported, not silently counted as
// "no match".
func TestGrepFiles_ReportsUnsearchedFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := grepOpen
	t.Cleanup(func() { grepOpen = orig })
	grepOpen = func(string) (*os.File, error) { return nil, os.ErrPermission }
	if err := os.WriteFile(filepath.Join(sub, "a.md"), []byte("needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := grepFiles(context.Background(), "needle", dir)
	if !strings.Contains(got, "could not be fully searched") || !strings.Contains(got, "a.md") {
		t.Fatalf("unreadable file not reported, got:\n%s", got)
	}
}
