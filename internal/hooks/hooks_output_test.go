package hooks

import (
	"context"
	"testing"
	"time"
)

// TestRunCommandJSONWithoutContinueDoesNotBlock covers a hook that prints a
// JSON object with only a message. "continue" is the veto; leaving it out must
// mean "no veto". It used to decode into a zero HookOutput, whose Continue is
// false, so a hook that merely wanted to add a note blocked every tool call.
func TestRunCommandJSONWithoutContinueDoesNotBlock(t *testing.T) {
	m := NewManager()
	cmdPath := helperCommand(t, modeJSONNoContinue)

	out, err := m.runCommand(context.Background(), cmdPath, HookInput{Event: BeforeTool})
	if err != nil {
		t.Fatalf("runCommand error = %v", err)
	}
	if !out.Continue {
		t.Errorf("Continue = false for a hook that never asked to block: %+v", out)
	}
	if out.Message != "note from hook" {
		t.Errorf("Message = %q, want the hook's message", out.Message)
	}
}

// TestFireReturnsWhenHookLeavesBackgroundProcess covers a hook that starts a
// background process (a notifier, a daemon) and exits. The background process
// inherits the hook's stdout, and reading stdout to EOF used to wait for it
// too, so the tool call stalled until that unrelated process exited, forever
// for a sequential hook without a Timeout.
func TestFireReturnsWhenHookLeavesBackgroundProcess(t *testing.T) {
	m := NewManager()
	ln := newLocalListener(t)
	cmdPath := helperCommand(t, modeSpawnBackground)

	register(m, HookConfig{
		Event:      BeforeTool,
		Type:       HookCommand,
		Command:    cmdPath,
		Sequential: true,
	})

	done := fireAsync(t, m, context.Background(), BeforeTool, "bash", HookInput{Event: BeforeTool})

	// The grandchild really is running and holding the pipe; closing this
	// connection at cleanup is what lets it exit.
	acceptWithin(t, ln, 30*time.Second)

	out := waitOutput(t, done, 20*time.Second)
	if !out.Continue {
		t.Errorf("Fire = %+v, want Continue=true", out)
	}
}
