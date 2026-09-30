package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/shell"
)

// A command that exits 0 while a background child still holds stdout used to
// come back as "exec: WaitDelay expired before I/O complete", reported as a
// start failure with its output dropped.
func TestBashBackgroundChildKeepsOutputAndExitCode(t *testing.T) {
	t.Parallel() // waits out the 5s WaitDelay
	if shell.Default().Kind != shell.Bash {
		t.Skip("needs a bash shell")
	}
	res, err := NewBashTool().Call(context.Background(),
		Input{"command": "echo hello; sleep 8 & echo done", "timeout": float64(30000)},
		Context{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("clean exit with a background child flagged as an error: %q", res.Data)
	}
	for _, want := range []string{"hello", "done", "background process"} {
		if !strings.Contains(res.Data, want) {
			t.Errorf("result lacks %q: %q", want, res.Data)
		}
	}
	if strings.Contains(res.Data, "WaitDelay") || strings.Contains(res.Data, "exit code") {
		t.Errorf("result reports a failure: %q", res.Data)
	}
}

func TestShellResultOutputHeldOpen(t *testing.T) {
	var out, errb boundedBuffer
	out.Write([]byte("server listening\n"))
	res := formatShellResult(Input{}, shell.Bash, &out, &errb, 0, errOutputHeldOpen, time.Second)
	if res.IsError {
		t.Errorf("flagged as an error: %q", res.Data)
	}
	if !strings.Contains(res.Data, "server listening") || !strings.Contains(res.Data, "background process") {
		t.Errorf("output or note missing: %q", res.Data)
	}
}
