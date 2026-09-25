package dream

import (
	"context"
	"os"
	"testing"
)

// Fix round 1 (item 2): RunNow reports how many sessions the run reviews.
func TestRunNowReturnsSessionCount(t *testing.T) {
	r, dir := statusTestRunner(t, "")
	for _, id := range []string{"a", "b", "c"} {
		touchSession(t, dir, id)
	}
	r.provider = nil
	// Without a provider the run does not start, but the count is known.
	n, err := r.RunNow(context.Background())
	if err == nil {
		t.Fatal("RunNow without a provider succeeded")
	}
	if n != 3 {
		t.Fatalf("RunNow counted %d sessions, want 3", n)
	}
}

// Fix round 1 (item 6): at exit a running consolidation is cancelled and the
// lock's timestamp rolled back, so a run killed midway is not counted as done.
func TestCancelActiveRollsBackLock(t *testing.T) {
	statusTestRunner(t, "")
	prior, ok, err := TryAcquireConsolidationLock()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	task := NewTask(2, prior, cancel)
	t.Cleanup(func() {
		if task.CurrentStatus() == StatusRunning {
			task.Fail()
		}
	})

	if !CancelActive() {
		t.Fatal("CancelActive found no running task")
	}
	if ctx.Err() == nil {
		t.Fatal("the task's context was not cancelled")
	}
	if task.CurrentStatus() != StatusFailed {
		t.Fatalf("task status = %s, want failed", task.CurrentStatus())
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock not rolled back (a first run's lock is removed): stat err %v", err)
	}
	// The run's own goroutine finishing later must not flip the status back.
	task.Complete()
	if task.CurrentStatus() != StatusFailed {
		t.Fatal("a finished task was re-finished")
	}
	if CancelActive() {
		t.Fatal("CancelActive with nothing running reported a task")
	}
}
