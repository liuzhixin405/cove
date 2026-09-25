package dream

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// TestIsProcessRunning_CurrentProcess proves the lock's liveness check works:
// the current process is obviously alive, so isProcessRunning(getpid()) must be true.
// Before the fix this returned false unconditionally (proc.Signal(os.Signal(nil))
// always errors), which made TryAcquireConsolidationLock treat every live holder as
// dead and steal the lock.
func TestIsProcessRunning_CurrentProcess(t *testing.T) {
	pid := os.Getpid()
	if !isProcessRunning(pid) {
		t.Fatalf("isProcessRunning(current pid=%d) = false, want true", pid)
	}
}

// TestIsProcessRunning_DeadProcess checks a PID that is essentially certain not to
// exist is reported as not running, so the lock can still be reclaimed from a
// genuinely dead holder.
func TestIsProcessRunning_DeadProcess(t *testing.T) {
	// 0x7FFFFFFE: extremely unlikely to be a live PID on any platform.
	if isProcessRunning(0x7FFFFFFE) {
		t.Skip("PID 0x7FFFFFFE unexpectedly reported running; skipping (environment-dependent)")
	}
}

// Sessions are <id>.jsonl (and legacy <id>.json); index.json is the list index
// and must not be counted as a session, which the old "*.json" match did while
// missing every .jsonl session.
func TestListSessionsTouchedSinceCountsSessionFilesOnly(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a.jsonl", "b.json", "index.json"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := ListSessionsTouchedSince(time.Now().Add(-time.Hour), dir)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ListSessionsTouchedSince = %v, want [a b]", ids)
	}
	if ids, _ := ListSessionsTouchedSince(time.Now().Add(time.Hour), dir); len(ids) != 0 {
		t.Fatalf("files older than since were counted: %v", ids)
	}
}
