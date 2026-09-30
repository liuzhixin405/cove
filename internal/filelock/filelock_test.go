package filelock

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireExcludesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	release, err := Acquire(path, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path, 50*time.Millisecond, time.Minute); !errors.Is(err, ErrTimeout) {
		t.Fatalf("second Acquire while held = %v, want ErrTimeout", err)
	}
	release()
	release() // idempotent
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("lock left behind after release (stat err %v)", err)
	}
	r2, err := Acquire(path, 50*time.Millisecond, time.Minute)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	r2()
}

// Concurrent holders never overlap.
func TestAcquireMutualExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	var inside, maxInside atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				release, err := Acquire(path, 10*time.Second, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				n := inside.Add(1)
				if n > maxInside.Load() {
					maxInside.Store(n)
				}
				time.Sleep(time.Millisecond)
				inside.Add(-1)
				release()
			}
		}()
	}
	wg.Wait()
	if maxInside.Load() != 1 {
		t.Fatalf("%d holders at once, want 1", maxInside.Load())
	}
}

// A lock left by a crashed holder is taken over once it is stale.
func TestAcquireBreaksStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	if err := os.WriteFile(path, []byte("pid 2147483646\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release, err := Acquire(path, time.Second, 30*time.Second)
	if err != nil {
		t.Fatalf("stale lock not taken over: %v", err)
	}
	release()
	if m, _ := filepath.Glob(path + ".stale-*"); len(m) != 0 {
		t.Fatalf("moved stale lock left behind: %v", m)
	}
}

// A holder whose lock was taken over as stale must not remove the new
// holder's lock on release.
func TestReleaseKeepsSomeoneElsesLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	release, err := Acquire(path, time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("pid 1\ntoken other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("release removed another holder's lock: %v", err)
	}
}

// The lock excludes another process, not just another goroutine: the helper
// process takes it and holds it until told to stop.
func TestAcquireExcludesOtherProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), MemoryLockName)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")
	cmd.Env = append(os.Environ(), "FILELOCK_HELPER_PATH="+path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	sc := bufio.NewScanner(stdout)
	for sc.Scan() && sc.Text() != "held" {
	}
	if _, err := Acquire(path, 100*time.Millisecond, time.Minute); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Acquire while another process holds the lock = %v, want ErrTimeout", err)
	}
	_ = stdin.Close() // helper releases and exits
	release, err := Acquire(path, 5*time.Second, time.Minute)
	if err != nil {
		t.Fatalf("Acquire after the other process released: %v", err)
	}
	release()
}

func TestHelperHoldLock(t *testing.T) {
	path := os.Getenv("FILELOCK_HELPER_PATH")
	if path == "" {
		t.Skip("helper process only")
	}
	release, err := Acquire(path, 5*time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("held\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n') // until the parent closes stdin
	release()
}
