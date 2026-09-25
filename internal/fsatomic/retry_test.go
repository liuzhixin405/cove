package fsatomic

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var errBusy = errors.New("sharing violation (test)")

// withRename swaps the rename and retry predicate for one test.
func withRename(t *testing.T, fn func(string, string) error) {
	t.Helper()
	oldRename, oldRetry, oldSleep := rename, retryableRename, sleep
	rename = fn
	retryableRename = func(err error) bool { return errors.Is(err, errBusy) }
	var slept []time.Duration
	sleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() {
		rename, retryableRename, sleep = oldRename, oldRetry, oldSleep
		sleepLog = nil
	})
	sleepLog = &slept
}

var sleepLog *[]time.Duration

// On Windows the rename fails while another process has the target open
// (a second cove reading index.json); a short retry lets it through.
func TestWriteFileRetriesTransientRenameFailure(t *testing.T) {
	calls := 0
	withRename(t, func(from, to string) error {
		calls++
		if calls < 3 {
			return errBusy
		}
		return os.Rename(from, to)
	})
	path := filepath.Join(t.TempDir(), "f.json")
	if err := WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "ok" {
		t.Fatalf("content = %q", data)
	}
	if calls != 3 {
		t.Errorf("rename called %d times, want 3", calls)
	}
	if got := *sleepLog; len(got) != 2 || got[0] != 10*time.Millisecond || got[1] != 20*time.Millisecond {
		t.Errorf("backoff = %v, want [10ms 20ms]", got)
	}
}

func TestWriteFileGivesUpAfterThreeRetries(t *testing.T) {
	calls := 0
	withRename(t, func(string, string) error { calls++; return errBusy })
	dir := t.TempDir()
	if err := WriteFile(filepath.Join(dir, "f.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("WriteFile succeeded though every rename failed")
	}
	if calls != 4 {
		t.Errorf("rename called %d times, want 1 + 3 retries", calls)
	}
	if got := *sleepLog; len(got) != 3 || got[2] != 40*time.Millisecond {
		t.Errorf("backoff = %v, want [10ms 20ms 40ms]", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("temp file left behind: %v", entries)
	}
}

func TestWriteFileDoesNotRetryPermanentErrors(t *testing.T) {
	calls := 0
	withRename(t, func(string, string) error { calls++; return errors.New("permanent") })
	if err := WriteFile(filepath.Join(t.TempDir(), "f.json"), []byte("x"), 0o600); err == nil {
		t.Fatal("want an error")
	}
	if calls != 1 {
		t.Errorf("a permanent error was retried: %d calls", calls)
	}
}
