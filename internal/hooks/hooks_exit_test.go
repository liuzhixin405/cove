package hooks

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/log"
)

// A BeforeTool hook that prints {"continue": false} and then exits non-zero
// (a script whose last command failed) used to have its output discarded:
// the exit error made Fire skip the hook and the tool ran despite the veto.
func TestFireHonoursVetoPrintedBeforeNonZeroExit(t *testing.T) {
	m := NewManager()
	register(m, HookConfig{Event: BeforeTool, Type: HookCommand, Command: helperCommand(t, modeBlockExitNonZero), Sequential: true})

	out := waitOutput(t, fireAsync(t, context.Background(), m, BeforeTool, "bash", HookInput{Event: BeforeTool}), 30*time.Second)
	if out.Continue {
		t.Fatalf("Fire = %+v, want the printed veto honoured", out)
	}
	if out.Message != "vetoed then failed" {
		t.Errorf("Message = %q, want the hook's message", out.Message)
	}
}

// Exit code 2 is the Claude Code PreToolUse convention for "block", with the
// reason on stderr; the PreToolUse alias of BeforeTool is accepted, so the
// convention is too.
func TestFireTreatsExitCode2AsBlock(t *testing.T) {
	m := NewManager()
	register(m, HookConfig{Event: PreToolUse, Type: HookCommand, Command: helperCommand(t, modeExit2), Sequential: true})

	out := waitOutput(t, fireAsync(t, context.Background(), m, BeforeTool, "bash", HookInput{Event: BeforeTool}), 30*time.Second)
	if out.Continue {
		t.Fatalf("Fire = %+v, want exit code 2 to block", out)
	}
	if !strings.Contains(out.Message, "blocked: touches production") {
		t.Errorf("Message = %q, want the hook's stderr", out.Message)
	}
}

// Any other non-zero exit without a veto still fails open.
func TestFireFailsOpenOnOtherNonZeroExit(t *testing.T) {
	m := NewManager()
	register(m, HookConfig{Event: BeforeTool, Type: HookCommand, Command: helperCommand(t, modeFail), Sequential: true})
	out := waitOutput(t, fireAsync(t, context.Background(), m, BeforeTool, "bash", HookInput{Event: BeforeTool}), 30*time.Second)
	if !out.Continue {
		t.Fatalf("Fire = %+v, want a failing hook without a veto not to block", out)
	}
}

// A hook killed by its timeout does not block, even when it printed a veto
// before hanging (the timeout decides, not a half-finished run), but the
// timeout is logged so the user can see the hook never finished.
func TestFireTimeoutDoesNotBlockButIsLogged(t *testing.T) {
	var buf lockedBuffer
	log.SetWriter(&buf)
	t.Cleanup(func() { log.SetWriter(io.Discard) })

	m := NewManager()
	ln := newLocalListener(t)
	const timeout = 5 * time.Second
	register(m, HookConfig{Event: BeforeTool, Type: HookCommand, Command: helperCommand(t, modePrintThenHang), Sequential: true, Timeout: timeout})

	done := fireAsync(t, context.Background(), m, BeforeTool, "bash", HookInput{Event: BeforeTool})
	acceptWithin(t, ln, 30*time.Second)
	out := waitOutput(t, done, 60*time.Second)
	if !out.Continue {
		t.Errorf("a hook killed by its timeout blocked the action: %+v", out)
	}
	if got := buf.String(); !strings.Contains(got, "timed out") {
		t.Errorf("log = %q, want the timeout reported", got)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
