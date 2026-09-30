//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
)

// restartRequest is what a supervised cove writes to the file named by
// restartArgsEnv: its next command line and the directory it was in. The
// file used to hold only the arguments, and each child started in the
// supervisor's directory, so "/cd D:\other" then /restart came back in the
// directory cove was first started in.
type restartRequest struct {
	Args []string `json:"args"`
	Dir  string   `json:"dir,omitempty"`
}

// restartSelf restarts cove on Windows, which has no exec: the process
// cannot be replaced, only a child started on the same console.
//
// The first /restart turns this process into a supervisor that starts the
// new cove and waits for it. A cove started that way does not start another
// child when it is restarted (that would stack one waiting process per
// restart): it writes its next command line and working directory to the
// file named by restartArgsEnv and exits with restartExitCode, and the
// supervisor starts it again there. Any other exit ends the supervisor with
// the same code.
func restartSelf(exe string, args []string) (int, error) {
	if file := os.Getenv(restartArgsEnv); file != "" {
		dir, _ := os.Getwd()
		data, err := json.Marshal(restartRequest{Args: args, Dir: dir})
		if err == nil {
			err = os.WriteFile(file, data, 0o600)
		}
		if err != nil {
			return 0, err
		}
		return restartExitCode, nil
	}

	// Ctrl+C and Ctrl+Break reach every process on the console; they are the
	// child's to handle. signal.Ignore used to be called here, which does not
	// protect a Windows process: Go's console handler reports an ignored
	// signal as unhandled, and Windows' default handler then ends the
	// supervisor (exit 0xc000013a) under a child that was only running a
	// command. A channel that is Notified and drained makes the runtime
	// report the event as handled for as long as the supervisor lives.
	stopSignals := absorbConsoleSignals()
	defer stopSignals()

	file := filepath.Join(os.TempDir(), "cove-restart-"+strconv.Itoa(os.Getpid())+".json")
	defer func() { _ = os.Remove(file) }()
	env := append(os.Environ(), restartArgsEnv+"="+file)
	dir := ""
	for {
		_ = os.Remove(file)
		cmd := exec.Command(exe, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = env
		cmd.Dir = dir
		err := cmd.Run()
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			return 0, err // it did not start
		}
		code := cmd.ProcessState.ExitCode()
		if code != restartExitCode {
			return code, nil
		}
		data, err := os.ReadFile(file)
		if err != nil {
			// Exit code 75 without the file is not a restart request.
			return code, nil
		}
		var next restartRequest
		if err := json.Unmarshal(data, &next); err != nil {
			return 0, err
		}
		args = next.Args
		// A directory removed since the child left it would make the start
		// fail; stay where the last child started instead.
		if next.Dir != "" {
			if st, err := os.Stat(next.Dir); err == nil && st.IsDir() {
				dir = next.Dir
			}
		}
	}
}

// absorbConsoleSignals receives the console's interrupt events (Ctrl+C and
// Ctrl+Break, both os.Interrupt) and drops them, so they do not end this
// process; the returned function stops that. Closing the console is left to
// the default action: the supervisor dying then does not end the child,
// which gets the same event and saves its session itself.
func absorbConsoleSignals() (stop func()) {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, os.Interrupt)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
