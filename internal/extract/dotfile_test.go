package extract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A model-chosen FILE: name kept its leading dot, so "FILE: .consolidate-lock"
// was written into the memory directory's bookkeeping: the dream lock (a
// second consolidation could start), the memory write lock or the extraction
// record. Memories are never dot files (the store skips them), so leading
// dots are stripped.
func TestSanitizeFilenameStripsLeadingDots(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{".consolidate-lock", "consolidate-lock.md"},
		{".consolidate-lock.takeover", "consolidate-lock.takeover"},
		{".memory-write.lock", "memory-write.lock"},
		{".last-extraction.json", "last-extraction.json"},
		{".hidden.md", "hidden.md"},
		{"...dots.md", "dots.md"},
		{"...", "memory.md"},
		{"dir/.hidden", "hidden.md"},
	} {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.HasPrefix(sanitizeFilename(tc.in), ".") {
			t.Errorf("sanitizeFilename(%q) = %q is a dot file", tc.in, sanitizeFilename(tc.in))
		}
	}
}

// End to end: the model names the consolidation lock; the lock is untouched
// and the fact lands in a plain memory file.
func TestExtractNeverWritesDotFiles(t *testing.T) {
	p := &fakeProvider{response: memoryBlock(".consolidate-lock", "write", "a fact the model filed under the lock's name")}
	r, dir := newTestRunner(t, p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".consolidate-lock")
	if err := os.WriteFile(lock, []byte("4242"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.Extract(context.Background(), conversation(6))

	if data, err := os.ReadFile(lock); err != nil || string(data) != "4242" {
		t.Fatalf("consolidation lock = %q, %v; want untouched", data, err)
	}
	entries := memoryFiles(t, dir)
	if len(entries) != 1 || entries[0] != "consolidate-lock.md" {
		t.Fatalf("memory files = %v, want [consolidate-lock.md]", entries)
	}
}
