package dream

import (
	"os"
	"strings"
	"testing"
	"time"
)

// A run's completion is two steps: markConsolidationDone (the lock becomes
// a done marker and this process stops owning it), then task.Complete.
// CancelActive between the two (the process exiting right as a run
// finished) failed the task and called RollbackConsolidationLock, which
// never asked whether this process still owned the lock: the finished
// run's done marker and timestamp were removed, so its sessions were
// reviewed all over again by the next run. A rollback after the lock was
// released is a no-op now.
func TestRollbackAfterCompletionLeavesDoneMarker(t *testing.T) {
	statusTestRunner(t, "")
	prior, ok, err := TryAcquireConsolidationLock()
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	task := NewTask(1, prior, nil)
	if !task.claimCompletion() {
		t.Fatal("claimCompletion refused")
	}
	markConsolidationDone()
	stamped, err := os.Stat(lockPath())
	if err != nil {
		t.Fatal(err)
	}

	// CancelActive's sequence, interleaved before task.Complete.
	if !task.Fail() {
		t.Fatal("Fail refused: the task is still running until Complete")
	}
	if err := RollbackConsolidationLock(task.PriorMtime); err != nil {
		t.Fatalf("rollback of a lock this process no longer owns: %v", err)
	}
	task.Complete()

	info, data, err := readLock(lockPath())
	if err != nil {
		t.Fatalf("the completed run's lock was removed: %v", err)
	}
	if !strings.HasPrefix(string(data), lockDonePrefix) {
		t.Fatalf("lock body = %q, want the done marker", data)
	}
	if !info.ModTime().Equal(stamped.ModTime()) {
		t.Fatalf("last consolidation time moved from %v to %v", stamped.ModTime(), info.ModTime())
	}
}

// The same, with a prior timestamp: the rollback would have rewound the
// completed run's lock to the previous consolidation time.
func TestRollbackAfterCompletionKeepsTimestamp(t *testing.T) {
	statusTestRunner(t, "")
	if err := RecordConsolidation(); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(lockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	prior, ok, err := TryAcquireConsolidationLock()
	if err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	if !prior.Equal(time.UnixMilli(old.UnixMilli())) {
		t.Fatalf("prior = %v, want %v", prior, old)
	}
	markConsolidationDone()
	if err := RollbackConsolidationLock(prior); err != nil {
		t.Fatal(err)
	}
	at, err := ReadLastConsolidatedAt()
	if err != nil {
		t.Fatal(err)
	}
	if at.Sub(old) < time.Hour {
		t.Fatalf("last consolidation time rewound to the prior run's %v", at)
	}
}

// A run that did not complete still rolls its lock back.
func TestRollbackOfOwnedLockStillRollsBack(t *testing.T) {
	statusTestRunner(t, "")
	if _, ok, err := TryAcquireConsolidationLock(); err != nil || !ok {
		t.Fatalf("acquire = %v, %v", ok, err)
	}
	if err := RollbackConsolidationLock(time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("owned lock not rolled back (stat err %v)", err)
	}
}
