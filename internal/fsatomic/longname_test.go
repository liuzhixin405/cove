package fsatomic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The temp file was named ".cove-tmp-<base>.<random>", 21 bytes more than the
// destination. File systems cap a name at 255 bytes, so a 250-byte name that
// os.WriteFile handled made CreateTemp fail ("filename syntax is incorrect" on
// Windows, ENAMETOOLONG elsewhere) and write/edit refused the file.
func TestWriteFileHandlesNamesNearTheLengthLimit(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{234, 235, 250, 255} {
		name := strings.Repeat("n", n-4) + ".txt"
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Skipf("this file system refuses a %d-byte name itself: %v", n, err)
		}
		if err := WriteFile(path, []byte("new"), 0o644); err != nil {
			t.Fatalf("WriteFile(%d-byte name): %v", n, err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != "new" {
			t.Fatalf("%d-byte name: content = %q, %v", n, data, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if IsTempName(e.Name()) {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

// A shortened temp name still carries the prefix directory scans skip, stays
// under the limit with CreateTemp's random suffix added, and differs between
// two long names that share a head (so two writers cannot collide on it).
func TestTempPatternStaysWithinNameLimit(t *testing.T) {
	long := strings.Repeat("a", 300)
	p := tempPattern(long)
	if !IsTempName(p) {
		t.Errorf("pattern %q lacks the temp prefix", p)
	}
	if len(p)+tempRandomLen > maxNameBytes {
		t.Errorf("pattern is %d bytes; with the random suffix it exceeds %d", len(p), maxNameBytes)
	}
	if other := tempPattern(strings.Repeat("a", 299) + "b"); other == p {
		t.Errorf("two long names shortened to the same pattern %q", p)
	}
	if short := tempPattern("f.json"); short != tempPrefix+"f.json." {
		t.Errorf("short name pattern = %q, want unchanged form", short)
	}
	// The cut must not split a rune: a name of 3-byte characters.
	cjk := tempPattern(strings.Repeat("中", 100))
	if !strings.HasPrefix(cjk, tempPrefix+"中") || strings.ContainsRune(cjk, '�') {
		t.Errorf("pattern for a CJK name = %q", cjk)
	}
}
