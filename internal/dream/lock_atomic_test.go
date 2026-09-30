package dream

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func lockTestHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(memoryDir(), 0o700); err != nil {
		t.Fatal(err)
	}
}

// raceAcquire runs n concurrent TryAcquireConsolidationLock calls released
// together and returns how many acquired.
func raceAcquire(t *testing.T, n int) int {
	t.Helper()
	var won atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, ok, err := TryAcquireConsolidationLock(); err == nil && ok {
				won.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return int(won.Load())
}

// Acquire used to be stat -> read -> truncate+write -> read back: two
// workers exiting together both saw a dead holder, both wrote, and both
// read back "their" PID (or, in one process, the same PID) — both ran.
func TestAcquireIsExclusiveWithoutLockFile(t *testing.T) {
	lockTestHome(t)
	if got := raceAcquire(t, 16); got != 1 {
		t.Fatalf("%d concurrent acquires of a missing lock succeeded, want exactly 1", got)
	}
}

func TestAcquireIsExclusiveOverStaleLock(t *testing.T) {
	lockTestHome(t)
	// A finished run's lock: a dead PID, stamped two hours ago.
	if err := os.WriteFile(lockPath(), []byte("2147483646"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath(), old, old); err != nil {
		t.Fatal(err)
	}
	if got := raceAcquire(t, 16); got != 1 {
		t.Fatalf("%d concurrent takeovers of a stale lock succeeded, want exactly 1", got)
	}
	if _, err := os.Stat(lockPath() + takeoverSuffix); !os.IsNotExist(err) {
		t.Fatalf("takeover guard left behind (stat err %v)", err)
	}
}

// A reader hitting a lock file mid-write (created, PID not yet in it) used
// to parse an empty PID as a dead holder and steal the lock.
func TestAcquireTreatsFreshEmptyLockAsHeld(t *testing.T) {
	lockTestHome(t)
	if err := os.WriteFile(lockPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := TryAcquireConsolidationLock(); err != nil || ok {
		t.Fatalf("acquire over a just-created empty lock = %v, %v; want blocked", ok, err)
	}
}

// A rolled-back lock is empty but carries an old timestamp: it is free.
func TestAcquireReclaimsRolledBackLock(t *testing.T) {
	lockTestHome(t)
	prior := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	if err := os.WriteFile(lockPath(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lockPath(), prior, prior); err != nil {
		t.Fatal(err)
	}
	got, ok, err := TryAcquireConsolidationLock()
	if err != nil || !ok {
		t.Fatalf("acquire over a rolled-back lock = %v, %v; want acquired", ok, err)
	}
	if !got.Equal(prior) {
		t.Fatalf("priorMtime = %v, want %v", got, prior)
	}
	// Rolling this run back restores exactly that timestamp.
	if err := RollbackConsolidationLock(got); err != nil {
		t.Fatal(err)
	}
	if at, _ := ReadLastConsolidatedAt(); !at.Equal(prior) {
		t.Fatalf("after rollback the lock says %v, want %v", at, prior)
	}
	matches, _ := filepath.Glob(filepath.Join(memoryDir(), ".cove-tmp-*"))
	if len(matches) != 0 {
		t.Fatalf("temp files left behind: %v", matches)
	}
}
