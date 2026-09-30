package dream

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

// waitIdle waits for r's background run to return (r.lastRunAt is set
// after runDream, and so after the lock is released): ActiveTask is nil a
// moment earlier, as soon as the task is marked complete.
func waitIdle(t *testing.T, r *Runner) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		done := !r.lastRunAt.IsZero()
		r.mu.Unlock()
		if done {
			r.mu.Lock()
			r.lastRunAt = time.Time{}
			r.mu.Unlock()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("consolidation still running")
}

// A run that completed left this process's live PID in the lock, and a live
// PID counted as held for an hour: "/dream run" right after reported that
// another consolidation holds the lock. A completed run now marks the lock
// done, keeping its timestamp (the last consolidation time).
func TestRunNowAgainAfterCompletedRun(t *testing.T) {
	r, dir := statusTestRunner(t, "")
	touchSession(t, dir, "a")
	r.provider = &fakeProvider{}
	if _, err := r.RunNow(context.Background()); err != nil {
		t.Fatalf("first RunNow: %v", err)
	}
	waitIdle(t, r)
	stamped, err := ReadLastConsolidatedAt()
	if err != nil || stamped.IsZero() {
		t.Fatalf("lock not stamped by the completed run: %v %v", stamped, err)
	}
	// Give the mtime a chance to differ if completion restamped it.
	time.Sleep(20 * time.Millisecond)
	if _, err := r.RunNow(context.Background()); errors.Is(err, ErrLockHeld) {
		t.Fatal("RunNow after a completed run in this process: lock reported held")
	} else if err != nil {
		t.Fatalf("second RunNow: %v", err)
	}
	waitIdle(t, r)
}

func TestCompletedRunKeepsLockTimestamp(t *testing.T) {
	r, dir := statusTestRunner(t, "")
	touchSession(t, dir, "a")
	r.provider = &fakeProvider{}
	if _, err := r.RunNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The timestamp the acquisition stamped.
	info, err := os.Stat(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, r)
	after, err := ReadLastConsolidatedAt()
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(info.ModTime()) {
		t.Fatalf("completion moved the last consolidation time from %v to %v", info.ModTime(), after)
	}
	info2, data, err := readLock(lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if lockHeld(lockPath(), info2, data) {
		t.Fatalf("completed run's lock %q still counts as held", data)
	}
}

// A lock with this process's PID that no run of this process holds (left
// by RecordConsolidation, or by a run whose completion mark failed) is not
// held against this process.
func TestOwnPIDWithoutRunIsNotHeld(t *testing.T) {
	statusTestRunner(t, "")
	if err := os.MkdirAll(memoryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath(), []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := TryAcquireConsolidationLock(); err != nil || !ok {
		t.Fatalf("acquire over our own idle PID = %v, %v; want acquired", ok, err)
	}
	// Now it is held by a run of this process: a second acquire is refused.
	if _, ok, err := TryAcquireConsolidationLock(); err != nil || ok {
		t.Fatalf("second acquire while this process's run holds it = %v, %v; want blocked", ok, err)
	}
	_ = RollbackConsolidationLock(time.Time{})
}

// recoverDeadWorker rolled the lock back whenever dream-last.json named a
// dead running worker and the lock's mtime was near its start, without
// looking at whose PID the lock holds: a lock another worker had just taken
// over was rolled back under it, and two consolidations ran.
func TestRecoverDeadWorkerLeavesLockOfAnotherHolder(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	if err := os.MkdirAll(memoryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	live := os.Getppid() // the worker that took the lock over: alive, not us
	if err := os.WriteFile(lockPath(), []byte(strconv.Itoa(live)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLastRun(LastRun{Mode: "worker", PID: 1 << 30, StartedAt: time.Now(), Result: ResultRunning}); err != nil {
		t.Fatal(err)
	}
	recoverDeadWorker()
	data, err := os.ReadFile(lockPath())
	if err != nil {
		t.Fatalf("the live holder's lock was rolled back: %v", err)
	}
	if string(data) != strconv.Itoa(live) {
		t.Fatalf("lock = %q, want the live holder's PID", data)
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultFailed {
		t.Fatalf("dead worker's record = %+v, want failed", lr)
	}
}

// While another worker holds the takeover guard (it is replacing the lock),
// recovery does not touch the lock.
func TestRecoverDeadWorkerWaitsForTakeoverGuard(t *testing.T) {
	_, sessions := workerTestEnv(t)
	touchSession(t, sessions, "s")
	if err := os.MkdirAll(memoryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	dead := 1 << 30
	if err := os.WriteFile(lockPath(), []byte(strconv.Itoa(dead)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLastRun(LastRun{Mode: "worker", PID: dead, StartedAt: time.Now(), Result: ResultRunning}); err != nil {
		t.Fatal(err)
	}
	release, ok, err := acquireTakeoverGuard(lockPath() + takeoverSuffix)
	if err != nil || !ok {
		t.Fatalf("guard: %v %v", ok, err)
	}
	recoverDeadWorker()
	if _, err := os.Stat(lockPath()); err != nil {
		t.Fatalf("lock rolled back while a takeover was in progress: %v", err)
	}
	if lr, _ := ReadLastRun(); lr.Result != ResultRunning {
		t.Fatalf("record changed while recovery could not check the lock: %+v", lr)
	}
	release()
	recoverDeadWorker()
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("dead worker's own lock not rolled back once the guard was free (stat err %v)", err)
	}
}
