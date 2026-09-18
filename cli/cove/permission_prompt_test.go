package main

import (
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/repl"
)

func TestPermissionAnswerDecision(t *testing.T) {
	cases := []struct {
		in     string
		allow  bool
		always bool
	}{
		{"y", true, false},
		{"Y", true, false},
		{"\r\ny\r\n", true, false},
		{"yes", true, false},
		{"是", true, false},
		{"a", true, true},
		{"ALWAYS", true, true},
		{"n", false, false},
		{"no", false, false},
		{"", false, false},
		{"what?", false, false},
	}
	for _, c := range cases {
		allow, always := permissionAnswerDecision(c.in)
		if allow != c.allow || always != c.always {
			t.Errorf("permissionAnswerDecision(%q) = (%v,%v), want (%v,%v)", c.in, allow, always, c.allow, c.always)
		}
	}
}

// TestAskToolPermissionRelay covers the plumbing the REPL loop depends on: the
// prompt registers a channel the loop can take, and the answer line it writes
// back becomes the decision returned to the engine. A nil engine is enough here
// because neither the allow nor the timeout path touches it.
func TestAskToolPermissionRelay(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh() })

	done := make(chan bool, 1)
	go func() {
		done <- askToolPermission(nil, "write", map[string]any{"file_path": "a.go"}, "")
	}()

	answerCh := waitForPermInputCh(t)
	answerCh <- "y"

	select {
	case allow := <-done:
		if !allow {
			t.Fatal("answer \"y\" must allow the tool call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("askToolPermission did not return after the answer was relayed")
	}
}

func TestAskToolPermissionDeniesWhenNotInteractive(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = false
	t.Cleanup(func() { replInteractive = oldInteractive })

	if askToolPermission(nil, "bash", map[string]any{"command": "echo hi"}, "") {
		t.Fatal("a non-interactive run must deny gated tools, not block on a prompt")
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("no input channel should be registered when there is no interactive loop")
	}
}

func TestAskToolPermissionTimesOut(t *testing.T) {
	oldInteractive, oldTimeout := replInteractive, permissionPromptTimeout
	replInteractive = true
	permissionPromptTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		replInteractive = oldInteractive
		permissionPromptTimeout = oldTimeout
		repl.ClearPermInputCh()
	})

	// Nothing drains the relayed channel, so the prompt stays unanswered and
	// must fall back to a denial instead of blocking forever.
	start := time.Now()
	allow := askToolPermission(nil, "bash", map[string]any{"command": "echo hi"}, "")
	if allow {
		t.Fatal("an unanswered prompt must deny the tool call")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout path took too long; the deadline is not being honoured")
	}
	if ch := repl.TakePermInputCh(); ch != nil {
		t.Fatal("the input channel must be unregistered after a timeout")
	}
}

// TestInstallPermissionPromptWiresHandler checks the REPL hookup: the engine
// must end up with a prompt callback, and answering "a" must both allow the
// call and register a session rule without panicking.
func TestInstallPermissionPromptWiresHandler(t *testing.T) {
	oldInteractive := replInteractive
	replInteractive = true
	t.Cleanup(func() { replInteractive = oldInteractive; repl.ClearPermInputCh() })

	eng, err := engine.New(engine.Config{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	installPermissionPrompt(eng)
	if eng.PermissionPrompt == nil {
		t.Fatal("installPermissionPrompt left the engine without a prompt callback")
	}

	done := make(chan bool, 1)
	go func() {
		done <- eng.PermissionPrompt("write", map[string]any{"file_path": "a.go"}, "write a.go")
	}()

	answerCh := waitForPermInputCh(t)
	answerCh <- "a"

	select {
	case allow := <-done:
		if !allow {
			t.Fatal("answer \"a\" (always) must allow the tool call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("engine prompt did not return after the answer was relayed")
	}
}

// waitForPermInputCh polls for the channel askToolPermission registers; the call
// runs on another goroutine, so its registration is not synchronous.
func waitForPermInputCh(t *testing.T) chan<- string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ch := repl.TakePermInputCh(); ch != nil {
			return ch
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no permission input channel was registered")
	return nil
}
