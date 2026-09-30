//go:build windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// restartHelperEnv makes the test binary act as the restarted cove: the
// value is a directory where it counts its starts.
const restartHelperEnv = "COVE_RESTART_TEST_HELPER"

// restartHelperCdEnv makes the first start change to this directory before
// asking for the restart, the way /cd does.
const restartHelperCdEnv = "COVE_RESTART_TEST_CD"

// TestRestartHelperProcess is not a test: it is the child the supervisor
// test starts. The first start asks for a restart the way a supervised cove
// does; the second records its working directory and exits with 7.
func TestRestartHelperProcess(t *testing.T) {
	dir := os.Getenv(restartHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	counter := filepath.Join(dir, "starts")
	data, _ := os.ReadFile(counter)
	n, _ := strconv.Atoi(string(data))
	n++
	_ = os.WriteFile(counter, []byte(strconv.Itoa(n)), 0o600)
	if n == 1 {
		if cd := os.Getenv(restartHelperCdEnv); cd != "" {
			if err := os.Chdir(cd); err != nil {
				os.Exit(1)
			}
		}
		code, err := restartSelf(os.Args[0], os.Args[1:])
		if err != nil {
			os.Exit(1)
		}
		os.Exit(code)
	}
	wd, _ := os.Getwd()
	_ = os.WriteFile(filepath.Join(dir, "cwd"), []byte(wd), 0o600)
	os.Exit(7)
}

// The supervisor starts the child again when it asks for a restart, and
// ends with the exit code of the one that finally exits.
func TestRestartSupervisorRestartsTheChild(t *testing.T) {
	if os.Getenv(restartHelperEnv) != "" || os.Getenv(restartArgsEnv) != "" {
		t.Skip("inside a helper process")
	}
	dir := t.TempDir()
	t.Setenv(restartHelperEnv, dir)
	code, err := restartSelf(os.Args[0], []string{"-test.run=^TestRestartHelperProcess$"})
	if err != nil {
		t.Fatalf("restartSelf: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7 (the second start's)", code)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "starts")); string(data) != "2" {
		t.Fatalf("child started %s times, want 2", data)
	}
}

// /cd then /restart: the new cove starts in the directory the old one was
// in, not in the one the supervisor was started from.
func TestRestartSupervisorKeepsTheChildsDirectory(t *testing.T) {
	if os.Getenv(restartHelperEnv) != "" || os.Getenv(restartArgsEnv) != "" {
		t.Skip("inside a helper process")
	}
	dir := t.TempDir()
	other := t.TempDir()
	t.Setenv(restartHelperEnv, dir)
	t.Setenv(restartHelperCdEnv, other)
	code, err := restartSelf(os.Args[0], []string{"-test.run=^TestRestartHelperProcess$"})
	if err != nil || code != 7 {
		t.Fatalf("restartSelf = %d, %v; want 7", code, err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "cwd"))
	want, _ := filepath.EvalSymlinks(other)
	gotDir, _ := filepath.EvalSymlinks(string(got))
	if !sameDir(gotDir, want) {
		t.Fatalf("restarted child ran in %q, want %q", got, other)
	}
}

// sameDir compares Windows paths, which are case-insensitive.
func sameDir(a, b string) bool {
	return a != "" && strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// A supervised cove hands its next command line and directory to the
// supervisor instead of starting a child of its own.
func TestRestartSupervisedWritesArgs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "args.json")
	t.Setenv(restartArgsEnv, file)
	code, err := restartSelf("unused.exe", []string{"-r", "abc"})
	if err != nil {
		t.Fatalf("restartSelf: %v", err)
	}
	if code != restartExitCode {
		t.Fatalf("exit code = %d, want %d", code, restartExitCode)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var req restartRequest
	if err := json.Unmarshal(data, &req); err != nil || len(req.Args) != 2 || req.Args[1] != "abc" {
		t.Fatalf("args file = %s (%v)", data, err)
	}
	wd, _ := os.Getwd()
	if req.Dir != wd {
		t.Fatalf("args file dir = %q, want %q", req.Dir, wd)
	}
}

// restartBreakHelperEnv makes the test binary a supervisor whose child
// handles Ctrl+Break itself and exits 7 a moment later.
const restartBreakHelperEnv = "COVE_RESTART_BREAK_HELPER"

// TestRestartBreakHelperProcess is not a test. As the supervisor (no
// restartArgsEnv yet) it runs restartSelf on itself; as the child it
// catches Ctrl+Break like cove does while a command runs, says it is
// ready, and exits 7 after a while.
func TestRestartBreakHelperProcess(t *testing.T) {
	dir := os.Getenv(restartBreakHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	if os.Getenv(restartArgsEnv) == "" {
		code, err := restartSelf(os.Args[0], os.Args[1:])
		if err != nil {
			os.Exit(1)
		}
		os.Exit(code)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	_ = os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600)
	time.Sleep(1500 * time.Millisecond)
	os.Exit(7)
}

// Ctrl+Break (or Ctrl+C) while the child runs a command reaches the
// supervisor too. signal.Ignore let Windows' default handler end the
// supervisor with 0xc000013a; it must survive and wait for the child.
func TestRestartSupervisorSurvivesCtrlBreak(t *testing.T) {
	if os.Getenv(restartBreakHelperEnv) != "" || os.Getenv(restartArgsEnv) != "" {
		t.Skip("inside a helper process")
	}
	gen := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestartBreakHelperProcess$")
	cmd.Env = append(os.Environ(), restartBreakHelperEnv+"="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("child never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	const ctrlBreakEvent = 1
	if r, _, err := gen.Call(ctrlBreakEvent, uintptr(cmd.Process.Pid)); r == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Skipf("no console to send Ctrl+Break on: %v", err)
	}
	_ = cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Fatalf("supervisor exit code = %#x, want 7 (the child's); the Ctrl+Break killed it", uint32(code))
	}
}
