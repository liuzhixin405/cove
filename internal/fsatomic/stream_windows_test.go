//go:build windows

package fsatomic

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A ':' in the file name names an NTFS stream. WriteFile used to create the
// temp file .cove-tmp-<head> plus a stream on it, fail at the rename, and
// remove only the stream, so an empty .cove-tmp-<head> stayed forever. It
// must refuse before creating anything, and leave an existing host file
// ("host" for "host:x") alone.
func TestWriteFileRefusesStreamNames(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host.jsonl")
	if err := os.WriteFile(host, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ab:c", "host.jsonl:x", "a:b:c.jsonl"} {
		err := WriteFile(filepath.Join(dir, name), []byte("data"), 0o600)
		if !errors.Is(err, ErrStreamName) {
			t.Errorf("WriteFile(%q) err = %v, want ErrStreamName", name, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "host.jsonl" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir after refused writes = %q, want only host.jsonl", names)
	}
	if got, _ := os.ReadFile(host); string(got) != "host" {
		t.Errorf("host.jsonl = %q, want untouched", got)
	}
	// A drive letter is the volume, not part of the base name.
	if err := WriteFile(filepath.Join(dir, "plain.txt"), []byte("ok"), 0o600); err != nil {
		t.Errorf("WriteFile(plain.txt) = %v, want nil", err)
	}
}
