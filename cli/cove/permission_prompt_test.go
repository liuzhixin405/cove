package main

import (
	"bytes"
	"errors"
	"path/filepath"
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
		in      string
		allow   bool
		always  bool
		persist bool
	}{
		{"y", true, false, false},
		{"Y", true, false, false},
		{"\r\ny\r\n", true, false, false},
		{"yes", true, false, false},
		{"是", true, false, false},
		{"a", true, true, false},
		{"ALWAYS", true, true, false},
		{"p", true, true, true},
		{"P", true, true, true},
		{"permanent", true, true, true},
		{"永久", true, true, true},
		{"n", false, false, false},
		{"no", false, false, false},
		{"", false, false, false},
		{"what?", false, false, false},
	}
	for _, c := range cases {
		allow, always, persist := permissionAnswerDecision(c.in)
		if allow != c.allow || always != c.always || persist != c.persist {
			t.Errorf("permissionAnswerDecision(%q) = (%v,%v,%v), want (%v,%v,%v)", c.in, allow, always, persist, c.allow, c.always, c.persist)
		}
	}
}

// persistingRules records what a "p" answer asked the engine to persist.
// Like *engine.Engine, a successful persist also installs the rules in the
// manager; a separate AddPermissionRule is counted in added.
type persistingRules struct {
	managerRules
	persisted []permission.Rule
	scopes    []string
	scope     string
	calls     int
	added     int
	err       error
}

func (p *persistingRules) AddPermissionRule(d permission.Decision, r permission.Rule) {
	p.added++
	p.managerRules.AddPermissionRule(d, r)
}

func (p *persistingRules) PersistPermissionRules(rs []permission.Rule, scope string) error {
	p.calls++
	if p.err != nil {
		return p.err
	}
	p.persisted = append(p.persisted, rs...)
	p.scopes = append(p.scopes, scope)
	for _, r := range rs {
		p.AddRule(permission.DAllow, r)
	}
	return nil
}

// A successful "p" leaves installing the rule to the persister (which
// registers it as a disk rule dropped on /cd); the prompt must not add a
// second, session-only copy that would outlive /cd.
func TestPermanentAnswerAddsNoSessionCopy(t *testing.T) {
	dir := t.TempDir()
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: permission.ProjectRoot(dir)}
	if allow, _ := answerPrompt(t, rules, "bash", map[string]any{"command": "go test ./..."}, "p"); !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if rules.added != 0 {
		t.Fatalf("prompt added %d session rules besides the persisted one", rules.added)
	}
}

// When writing policies.json fails, "p" degrades to a session rule and says so.
func TestPermanentAnswerPersistFailureFallsBackToSessionRule(t *testing.T) {
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: "/p", err: errors.New("disk full")}
	allow, out := answerPrompt(t, rules, "bash", map[string]any{"command": "go test ./..."}, "p")
	if !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if rules.added != 1 {
		t.Fatalf("session fallback rules added = %d, want 1", rules.added)
	}
	if d := checkCommand(m, "bash", "go test ./x"); d != permission.DAllow {
		t.Errorf("session fallback rule missing: %v", d)
	}
	if !strings.Contains(out, "未能写入，仅本次会话有效") || !strings.Contains(out, "disk full") {
		t.Errorf("failure not reported:\n%s", out)
	}
}

func (p *persistingRules) PermissionScope() string { return p.scope }

// Answering "p" allows the call and persists the rule scoped to the current
// project; the persister (the engine) installs it for this session, so the
// rule applies right away without a separate session copy.
func TestPermanentAnswerPersistsRuleForThisProject(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: permission.ProjectRoot(dir)}
	allow, out := answerPrompt(t, rules, "bash", map[string]any{"command": "go test ./..."}, "p")
	if !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if !strings.Contains(out, "[p] 永久允许") {
		t.Errorf("prompt does not offer [p]:\n%s", out)
	}
	if !strings.Contains(out, "[a] 本次会话总是允许") || !strings.Contains(out, "[n] 拒绝") {
		t.Errorf("prompt lost the [a]/[n] options:\n%s", out)
	}
	if d := checkCommand(m, "bash", "go test -run X ./pkg"); d != permission.DAllow {
		t.Errorf("session rule missing after \"p\": %v", d)
	}
	if len(rules.persisted) != 1 || rules.persisted[0].CommandPrefix != "go test" || rules.persisted[0].ToolPattern != "bash" {
		t.Fatalf("persisted = %+v, want one bash/go test rule", rules.persisted)
	}
	if want := permission.ProjectRoot(dir); !permission.SameProject(rules.scopes[0], want) {
		t.Errorf("scope = %q, want project root %q", rules.scopes[0], want)
	}
}

// The confirmation names the file the rule went to: policies.json in the
// config directory, which COVE_CONFIG_DIR moves away from ~/.cove.
func TestPermanentAnswerNamesTheActualPoliciesFile(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", cfgDir)
	dir := t.TempDir()
	t.Chdir(dir)
	rules := &persistingRules{managerRules: managerRules{permission.NewManager(permission.Default)}, scope: permission.ProjectRoot(dir)}
	_, out := answerPrompt(t, rules, "bash", map[string]any{"command": "go test ./..."}, "p")
	if want := filepath.Join(cfgDir, "policies.json"); !strings.Contains(out, want) {
		t.Fatalf("confirmation does not name %s:\n%s", want, out)
	}
	if strings.Contains(out, "~/.cove/policies.json") {
		t.Fatalf("confirmation still names ~/.cove/policies.json:\n%s", out)
	}
}

// Without a persister (or for an unscopable command) "p" still only allows
// once or for the session; nothing is written.
func TestPermanentAnswerWithoutPersisterFallsBackToSession(t *testing.T) {
	m := permission.NewManager(permission.Default)
	allow, _ := answerPrompt(t, managerRules{m}, "bash", map[string]any{"command": "go test ./..."}, "p")
	if !allow {
		t.Fatal("answer \"p\" must allow the current call")
	}
	if d := checkCommand(m, "bash", "go test ./x"); d != permission.DAllow {
		t.Errorf("session rule missing: %v", d)
	}
	rules := &persistingRules{managerRules: managerRules{permission.NewManager(permission.Default)}}
	if allow, _ := answerPrompt(t, rules, "bash", map[string]any{"command": "sudo go test"}, "p"); !allow {
		t.Fatal("\"p\" on an unscopable command must still allow once")
	}
	if len(rules.persisted) != 0 {
		t.Errorf("persisted a rule for an unscopable command: %+v", rules.persisted)
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
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
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

// All prefixes of one answer are persisted in a single call, scoped to the
// engine's project (PermissionScope), not to os.Getwd.
func TestPermanentAnswerPersistsAllPrefixesInOneCall(t *testing.T) {
	m := permission.NewManager(permission.Default)
	rules := &persistingRules{managerRules: managerRules{m}, scope: "/engine/project"}
	if allow, _ := answerPrompt(t, rules, "bash", map[string]any{"command": "go test ./... | tee out.txt"}, "p"); !allow {
		t.Fatal("answer \"p\" must allow the call")
	}
	if rules.calls != 1 || len(rules.persisted) != 2 {
		t.Fatalf("persist calls = %d, rules = %+v; want one call with 2 rules", rules.calls, rules.persisted)
	}
	if rules.scopes[0] != "/engine/project" {
		t.Errorf("scope = %q, want the engine's PermissionScope", rules.scopes[0])
	}
}

// /cd looks for PolicyLoadError on the engine view it is given; the real
// program hands it a replEngineAdapter.
var _ interface{ PolicyLoadError() error } = replEngineAdapter{}
