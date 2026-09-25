package diagnostic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/config"
)

// The integrity check removes empty session files. It must judge only session
// files — <id>.jsonl and legacy <id>.json — and leave index.json (the list
// index, which may legitimately be tiny) and everything else alone.
func TestSessionIntegrityChecksOnlySessionFiles(t *testing.T) {
	c, root := newTestChecker(t, config.DefaultConfig())
	dir := filepath.Join(root, ".cove", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a.jsonl":    "",   // empty session: corrupt
		"b.json":     "",   // empty legacy session: corrupt
		"c.jsonl":    "{}", // fine
		"index.json": "",   // not a session
		"notes.txt":  "",
	}
	for n, body := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := c.checkSessionIntegrity(context.Background())
	if res.Status != SevRecovered || res.Error == nil || !strings.Contains(res.Error.Detail, "2 ") {
		t.Fatalf("result = %+v (%+v), want 2 corrupt sessions recovered", res, res.Error)
	}
	for n, wantGone := range map[string]bool{"a.jsonl": true, "b.json": true, "c.jsonl": false, "index.json": false, "notes.txt": false} {
		_, err := os.Stat(filepath.Join(dir, n))
		if gone := os.IsNotExist(err); gone != wantGone {
			t.Errorf("%s removed=%v, want %v", n, gone, wantGone)
		}
	}
}
