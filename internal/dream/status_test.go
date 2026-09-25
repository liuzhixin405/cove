package dream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// statusTestRunner points the home directory (where the consolidation lock
// and dream.json live) at a temp dir and returns a runner whose sessions dir
// is another temp dir. dream.json selects the threshold trigger, whose gates
// these tests exercise (session_end is the default; see worker_test.go).
func statusTestRunner(t *testing.T, current string) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", "")
	if err := os.MkdirAll(filepath.Join(home, ".cove"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".cove", "dream.json"), []byte(`{"trigger": "threshold"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := t.TempDir()
	r := &Runner{currentSession: current, memoryRoot: filepath.Join(home, ".cove", "memory"), sessionsDir: sessions}
	return r, sessions
}

func touchSession(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultsAreTwelveHoursThreeSessions(t *testing.T) {
	if defaults.MinHours != 12 || defaults.MinSessions != 3 {
		t.Fatalf("defaults = %+v, want 12h / 3 sessions", defaults)
	}
}

// With no consolidation on record HoursSinceLast is -1 (documented), and the
// session count excludes the session in use.
func TestStatusWithoutHistory(t *testing.T) {
	r, dir := statusTestRunner(t, "cur")
	touchSession(t, dir, "a")
	touchSession(t, dir, "b")
	touchSession(t, dir, "cur")

	st := r.Status()
	if !st.Enabled {
		t.Error("Enabled = false with default config")
	}
	if !st.LastConsolidatedAt.IsZero() {
		t.Errorf("LastConsolidatedAt = %v, want zero", st.LastConsolidatedAt)
	}
	if st.HoursSinceLast != -1 {
		t.Errorf("HoursSinceLast = %v, want -1 when never consolidated", st.HoursSinceLast)
	}
	if st.SessionsSinceLast != 2 {
		t.Errorf("SessionsSinceLast = %d, want 2 (current session excluded)", st.SessionsSinceLast)
	}
	if st.MinHours != 12 || st.MinSessions != 3 {
		t.Errorf("thresholds = %dh/%d, want 12h/3", st.MinHours, st.MinSessions)
	}
	if st.Running {
		t.Error("Running = true with no task")
	}
	if got := st.SessionsNeeded(); got != 1 {
		t.Errorf("SessionsNeeded = %d, want 1", got)
	}
	if got := st.HoursNeeded(); got != 0 {
		t.Errorf("HoursNeeded = %v, want 0 when never consolidated", got)
	}
}

func TestStatusCountsOnlySessionsSinceLastConsolidation(t *testing.T) {
	r, dir := statusTestRunner(t, "")
	touchSession(t, dir, "old")
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := RecordConsolidation(); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(lockPath(), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	touchSession(t, dir, "new")

	st := r.Status()
	if st.SessionsSinceLast != 1 {
		t.Errorf("SessionsSinceLast = %d, want 1", st.SessionsSinceLast)
	}
	if st.HoursSinceLast < 1.9 || st.HoursSinceLast > 2.1 {
		t.Errorf("HoursSinceLast = %v, want about 2", st.HoursSinceLast)
	}
	if got := st.HoursNeeded(); got < 9.9 || got > 10.1 {
		t.Errorf("HoursNeeded = %v, want about 10", got)
	}
}

// SetCurrentSession changes which session the gate excludes (a resumed
// session must not count as "another" session).
func TestSetCurrentSessionMovesExclusion(t *testing.T) {
	r, dir := statusTestRunner(t, "first")
	touchSession(t, dir, "first")
	touchSession(t, dir, "resumed")
	touchSession(t, dir, "other")
	if got := r.Status().SessionsSinceLast; got != 2 {
		t.Fatalf("before: SessionsSinceLast = %d, want 2", got)
	}
	r.SetCurrentSession("resumed")
	r.mu.Lock()
	cur := r.currentSession
	r.mu.Unlock()
	if cur != "resumed" {
		t.Fatalf("currentSession = %q, want resumed", cur)
	}
	r.SetCurrentSession("")
	if got := r.Status().SessionsSinceLast; got != 3 {
		t.Fatalf("no current: SessionsSinceLast = %d, want 3", got)
	}
}

// RunNow still takes the lock: a live holder makes it fail with ErrLockHeld.
func TestRunNowRespectsLock(t *testing.T) {
	r, _ := statusTestRunner(t, "")
	// The current process holds the lock (a live PID, fresh mtime).
	if err := RecordConsolidation(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunNow(context.Background()); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("RunNow = %v, want ErrLockHeld", err)
	}
}

// Without a provider RunNow fails and gives the lock back.
func TestRunNowWithoutProviderReleasesLock(t *testing.T) {
	r, _ := statusTestRunner(t, "")
	if _, err := r.RunNow(context.Background()); err == nil {
		t.Fatal("RunNow without a provider succeeded")
	}
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("lock left behind (stat err %v)", err)
	}
}

func TestRunNowRefusesWhenDisabled(t *testing.T) {
	r, _ := statusTestRunner(t, "")
	home, _ := os.UserHomeDir()
	_ = os.MkdirAll(filepath.Join(home, ".cove"), 0o700)
	if err := os.WriteFile(filepath.Join(home, ".cove", "dream.json"), []byte(`{"enabled": false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RunNow(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Fatalf("RunNow = %v, want ErrDisabled", err)
	}
	if r.Status().Enabled {
		t.Error("Status().Enabled = true with enabled:false")
	}
}

// Under -p the process exits right after the answer, which would kill a
// consolidation mid-run and leave the lock stamped as if it had finished.
func TestSuppressAutoSkipsAutoDream(t *testing.T) {
	r, dir := statusTestRunner(t, "")
	for _, id := range []string{"a", "b", "c", "d"} {
		touchSession(t, dir, id)
	}
	SuppressAuto("test")
	t.Cleanup(func() { SuppressAuto("") })
	if r.Status().Suppressed != "test" {
		t.Error("Status().Suppressed does not carry the reason")
	}
	r.ExecuteAutoDream(context.Background())
	if _, err := os.Stat(lockPath()); !os.IsNotExist(err) {
		t.Fatalf("auto dream took the lock while suppressed (stat err %v)", err)
	}
}

func TestCurrentRunnerIsLastCreated(t *testing.T) {
	statusTestRunner(t, "")
	r := NewRunner(nil, "m", "s1")
	if Current() != r {
		t.Fatal("Current() is not the runner NewRunner just created")
	}
}

func TestStatusFromDiskUsesHomeSessions(t *testing.T) {
	statusTestRunner(t, "")
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cove", "sessions")
	_ = os.MkdirAll(dir, 0o700)
	touchSession(t, dir, "x")
	if got := StatusFromDisk().SessionsSinceLast; got != 1 {
		t.Fatalf("StatusFromDisk().SessionsSinceLast = %d, want 1", got)
	}
}
