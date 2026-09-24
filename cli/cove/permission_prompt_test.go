package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/repl"
	"github.com/liuzhixin405/cove/internal/termui"
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
	// The engine opens its session store under HOME and project notes under
	// the working directory; this test used to create both in the user's
	// real ~/.cove and in the package directory.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(t.TempDir())

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

// managerRules lets the prompt add session rules straight to a Manager, so a
// test can ask the Manager what the "a" answer actually allows afterwards.
type managerRules struct{ *permission.Manager }

func (m managerRules) AddPermissionRule(d permission.Decision, r permission.Rule) { m.AddRule(d, r) }

// answerPrompt runs one prompt for toolName/input, answers it and returns the
// decision together with everything the prompt printed.
func answerPrompt(t *testing.T, rules permissionRuleAdder, toolName string, input map[string]any, answer string) (bool, string) {
	t.Helper()
	oldInteractive := replInteractive
	replInteractive = true
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	t.Cleanup(func() {
		replInteractive = oldInteractive
		termui.SetWriter(nil)
		repl.ClearPermInputCh()
	})

	done := make(chan bool, 1)
	go func() { done <- askToolPermission(rules, toolName, input, "") }()
	waitForPermInputCh(t) <- answer
	select {
	case allow := <-done:
		return allow, buf.String()
	case <-time.After(5 * time.Second):
		t.Fatal("askToolPermission did not return after the answer was relayed")
		return false, ""
	}
}

func checkCommand(m *permission.Manager, tool, cmd string) permission.Decision {
	d, _ := m.Check(tool, map[string]any{"command": cmd}, permission.DAsk)
	return d
}

// Answering "a" for a shell command must remember only that command's prefix:
// the old whole-tool rule let every later bash command, rm -rf included, run
// without a prompt.
func TestAlwaysAnswerScopesShellToolsToCommandPrefix(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "go test ./..."}, "a")
	if !allow {
		t.Fatal("answer \"a\" must allow the current call")
	}
	if !strings.Contains(out, `"go test" 开头的命令`) {
		t.Errorf("prompt output does not name the remembered prefix:\n%s", out)
	}

	if d := checkCommand(m, "bash", "go test -run TestX ./pkg"); d != permission.DAllow {
		t.Errorf("later go test = %v, want allow", d)
	}
	for _, cmd := range []string{"rm -rf src", "go test ./... && rm -rf x", "sudo go test"} {
		if d := checkCommand(m, "bash", cmd); d != permission.DAsk {
			t.Errorf("%q after allowing go test = %v, want ask", cmd, d)
		}
	}
}

func TestAlwaysAnswerKeepsWholeToolScopeForOtherTools(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, _ := answerPrompt(t, managerRules{m}, "write", map[string]any{"file_path": "a.go"}, "a")
	if !allow {
		t.Fatal("answer \"a\" must allow the current call")
	}
	if d, _ := m.Check("write", map[string]any{"file_path": "b.go"}, permission.DAsk); d != permission.DAllow {
		t.Errorf("write after \"a\" = %v, want allow", d)
	}
}

// A command no prefix can safely describe is allowed once and nothing is
// remembered, and the prompt says so instead of offering a scope it lacks.
func TestAlwaysAnswerForUnscopableCommandAllowsOnlyOnce(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, out := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "sudo go test"}, "a")
	if !allow {
		t.Fatal("answer \"a\" must still allow the current call")
	}
	if strings.Contains(out, "总是允许") {
		t.Errorf("prompt offered an always-allow scope for an unscopable command:\n%s", out)
	}
	if d := checkCommand(m, "bash", "sudo go test"); d != permission.DAsk {
		t.Errorf("sudo go test after \"a\" = %v, want ask", d)
	}
	if d := checkCommand(m, "bash", "ls"); d != permission.DAsk {
		t.Errorf("ls after \"a\" on sudo go test = %v, want ask", d)
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
