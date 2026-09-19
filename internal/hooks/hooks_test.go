package hooks

import (
	"context"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

// register installs hooks on the manager. The package exposes no Register
// method (Manager.hooks is only ever read), so the in-package test writes the
// map through the same mutex Fire uses. See the report: production code has no
// way to add a hook.
func register(m *Manager, configs ...HookConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, h := range configs {
		m.hooks[h.Event] = append(m.hooks[h.Event], h)
	}
}

// helperCommand returns the path used as HookConfig.Command: this very test
// binary, which TestMain turns into the requested helper mode.
func helperCommand(t *testing.T, mode string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	t.Setenv(helperModeEnv, mode)
	return exe
}

// newLocalListener gives the helper process something to connect back to, so
// "the hook started" is observable without sleeping.
func newLocalListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	t.Setenv(helperAddrEnv, ln.Addr().String())
	return ln
}

// acceptWithin fails the test instead of hanging if the hook never ran.
func acceptWithin(t *testing.T, ln net.Listener, d time.Duration) net.Conn {
	t.Helper()
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("accept: %v", r.err)
		}
		t.Cleanup(func() { r.conn.Close() })
		return r.conn
	case <-time.After(d):
		t.Fatalf("hook process never connected back within %v", d)
		return nil
	}
}

// fireAsync runs Fire on another goroutine so a hook that hangs fails the test
// with a clear message instead of blocking it forever.
func fireAsync(t *testing.T, m *Manager, ctx context.Context, event HookEvent, target string, in HookInput) <-chan HookOutput {
	t.Helper()
	ch := make(chan HookOutput, 1)
	go func() { ch <- m.Fire(ctx, event, target, in) }()
	return ch
}

func waitOutput(t *testing.T, ch <-chan HookOutput, d time.Duration) HookOutput {
	t.Helper()
	select {
	case out := <-ch:
		return out
	case <-time.After(d):
		t.Fatalf("Fire did not return within %v", d)
		return HookOutput{}
	}
}

// ----------------------------------------------------------------------------
// Fire: sequential runtime hooks
// ----------------------------------------------------------------------------

func TestFireRunsMatchingSequentialRuntimeHook(t *testing.T) {
	m := NewManager()

	var mu sync.Mutex
	var seen []HookInput
	register(m, HookConfig{
		Event:      BeforeTool,
		Type:       HookRuntime,
		Sequential: true,
		RuntimeFn: func(in HookInput) (HookOutput, error) {
			mu.Lock()
			seen = append(seen, in)
			mu.Unlock()
			return HookOutput{Continue: true, Modified: true, Message: "ok"}, nil
		},
	})

	in := HookInput{Event: BeforeTool, ToolName: "Bash", ToolInput: map[string]any{"cmd": "ls"}, Model: "m1"}
	out := m.Fire(context.Background(), BeforeTool, "Bash", in)

	if !out.Continue {
		t.Error("Fire blocked although the hook allowed the action")
	}
	if !out.Modified {
		t.Error("Fire did not propagate Modified=true from the hook")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("hook ran %d times, want 1", len(seen))
	}
	if seen[0].ToolName != "Bash" || seen[0].Model != "m1" || seen[0].ToolInput["cmd"] != "ls" {
		t.Errorf("hook received %+v, want the input Fire was called with", seen[0])
	}
}

func TestFireWithNoRegisteredHooksAllowsAction(t *testing.T) {
	m := NewManager()
	out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})
	if !out.Continue || out.Modified || out.Message != "" {
		t.Fatalf("Fire with no hooks = %+v, want {Continue:true}", out)
	}
}

func TestFireOnlyRunsHooksForTheRequestedEvent(t *testing.T) {
	m := NewManager()
	var before, after int
	register(m,
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) { before++; return HookOutput{Continue: true}, nil }},
		HookConfig{Event: AfterTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) { after++; return HookOutput{Continue: true}, nil }},
	)

	m.Fire(context.Background(), AfterTool, "Bash", HookInput{Event: AfterTool})

	if before != 0 {
		t.Errorf("BeforeTool hook ran %d times for an AfterTool event", before)
	}
	if after != 1 {
		t.Errorf("AfterTool hook ran %d times, want 1", after)
	}
}

// TestFireBlockingHookShortCircuits is the core safety property: a hook that
// says no must stop the action AND stop later hooks from running.
func TestFireBlockingHookShortCircuits(t *testing.T) {
	m := NewManager()

	var ran []string
	register(m,
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				ran = append(ran, "first")
				return HookOutput{Continue: true, Modified: true}, nil
			}},
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				ran = append(ran, "blocker")
				return HookOutput{Continue: false, Message: "nope"}, nil
			}},
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				ran = append(ran, "never")
				return HookOutput{Continue: true}, nil
			}},
	)

	out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})

	if out.Continue {
		t.Error("Fire returned Continue=true although a hook blocked the action")
	}
	if out.Message != "nope" {
		t.Errorf("Message = %q, want the blocking hook's message %q", out.Message, "nope")
	}
	want := []string{"first", "blocker"}
	if strings.Join(ran, ",") != strings.Join(want, ",") {
		t.Errorf("hooks ran %v, want %v (hooks after the blocker must be skipped)", ran, want)
	}
}

func TestFireSkipsHookThatReturnsAnError(t *testing.T) {
	m := NewManager()

	var later int
	register(m,
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				return HookOutput{Continue: false, Message: "should be ignored"}, context.DeadlineExceeded
			}},
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) { later++; return HookOutput{Continue: true}, nil }},
	)

	out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})

	// A failing hook must fail open (not block the user's action) and must not
	// stop the remaining hooks.
	if !out.Continue {
		t.Error("an erroring hook blocked the action; hooks must fail open")
	}
	if out.Message != "" {
		t.Errorf("Message = %q, want empty: the output of an erroring hook must be discarded", out.Message)
	}
	if later != 1 {
		t.Errorf("subsequent hook ran %d times, want 1", later)
	}
}

func TestFireAggregatesModifiedAcrossHooks(t *testing.T) {
	m := NewManager()
	register(m,
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) { return HookOutput{Continue: true, Modified: true}, nil }},
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) { return HookOutput{Continue: true, Modified: false}, nil }},
	)

	out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})
	if !out.Continue {
		t.Fatal("Fire blocked unexpectedly")
	}
	if !out.Modified {
		t.Error("Modified must stay true once any hook set it")
	}
}

func TestExecuteHookHandlesNilRuntimeFnAndUnknownType(t *testing.T) {
	m := NewManager()

	out, err := m.executeHook(context.Background(), HookConfig{Type: HookRuntime}, HookInput{})
	if err != nil {
		t.Errorf("nil RuntimeFn returned error %v, want nil", err)
	}
	if !out.Continue {
		t.Error("nil RuntimeFn must not block the action")
	}

	out, err = m.executeHook(context.Background(), HookConfig{Type: HookType(99)}, HookInput{})
	if err == nil {
		t.Error("unknown hook type returned nil error")
	}
	if !out.Continue {
		t.Error("unknown hook type must fail open, not block the action")
	}
}

// ----------------------------------------------------------------------------
// Matcher
// ----------------------------------------------------------------------------

func TestMatcherFiltersByTarget(t *testing.T) {
	cases := []struct {
		name    string
		matcher string
		target  string
		want    bool
	}{
		{"empty matcher matches everything", "", "Bash", true},
		{"empty matcher matches the empty target", "", "", true},
		{"anchored match", "^Bash$", "Bash", true},
		{"anchored non-match", "^Bash$", "BashTool", false},
		{"unanchored is a substring search", "Bash", "MyBashTool", true},
		{"alternation", "^(Read|Write)$", "Write", true},
		{"alternation miss", "^(Read|Write)$", "Edit", false},
		{"invalid regex never matches", "[", "[", false},
		{"invalid regex never matches empty target", "*invalid(", "", false},
	}

	m := NewManager()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.matches(HookConfig{Matcher: tc.matcher}, tc.target)
			if got != tc.want {
				t.Fatalf("matches(%q, %q) = %v, want %v", tc.matcher, tc.target, got, tc.want)
			}
		})
	}
}

// TestFireAppliesMatcherToSequentialHooks checks the filter through the public
// entry point. Sequential hooks are used so completion is observable without
// any timing assumptions.
func TestFireAppliesMatcherToSequentialHooks(t *testing.T) {
	m := NewManager()

	var mu sync.Mutex
	ran := map[string]int{}
	mk := func(name, matcher string) HookConfig {
		return HookConfig{
			Event: BeforeTool, Type: HookRuntime, Sequential: true, Matcher: matcher,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				mu.Lock()
				ran[name]++
				mu.Unlock()
				return HookOutput{Continue: true}, nil
			},
		}
	}
	register(m,
		mk("all", ""),
		mk("bash-only", "^Bash$"),
		mk("write-only", "^Write$"),
		mk("broken", "(unclosed"),
	)

	m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool, ToolName: "Bash"})

	mu.Lock()
	defer mu.Unlock()
	if ran["all"] != 1 {
		t.Errorf("empty-matcher hook ran %d times, want 1", ran["all"])
	}
	if ran["bash-only"] != 1 {
		t.Errorf("matching hook ran %d times, want 1", ran["bash-only"])
	}
	if ran["write-only"] != 0 {
		t.Errorf("non-matching hook ran %d times, want 0", ran["write-only"])
	}
	if ran["broken"] != 0 {
		t.Errorf("hook with an invalid regex ran %d times, want 0", ran["broken"])
	}
}

// TestFireInvalidMatcherDoesNotBlockTheAction pins the fail-open behaviour: a
// typo in a Matcher must not deny every tool call.
func TestFireInvalidMatcherDoesNotBlockTheAction(t *testing.T) {
	m := NewManager()
	register(m, HookConfig{
		Event: BeforeTool, Type: HookRuntime, Sequential: true, Matcher: "([a-z",
		RuntimeFn: func(HookInput) (HookOutput, error) {
			return HookOutput{Continue: false, Message: "deny"}, nil
		},
	})

	out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})
	if !out.Continue {
		t.Fatalf("a hook with an invalid Matcher blocked the action: %+v", out)
	}
}

// ----------------------------------------------------------------------------
// runCommand
// ----------------------------------------------------------------------------

func TestRunCommandSendsHookInputAsJSONOnStdin(t *testing.T) {
	m := NewManager()
	cmdPath := helperCommand(t, modeEchoInput)

	in := HookInput{
		Event:     BeforeTool,
		ToolName:  "Write",
		Model:     "claude-test",
		ToolInput: map[string]any{"path": "/tmp/x"},
		// Messages is tagged json:"-" so conversation content never leaks to an
		// external hook process.
		Messages: []api.Message{{Role: "user", Content: "secret"}, {Role: "assistant", Content: "more"}},
	}

	out, err := m.runCommand(context.Background(), cmdPath, in)
	if err != nil {
		t.Fatalf("runCommand error = %v", err)
	}

	want := "event=BeforeTool tool=Write model=claude-test path=/tmp/x messages=0"
	if out.Message != want {
		t.Fatalf("hook saw %q, want %q", out.Message, want)
	}
	if !out.Continue || !out.Modified {
		t.Errorf("out = %+v, want the JSON HookOutput the hook printed (Continue and Modified true)", out)
	}
}

func TestRunCommandParsesBlockingJSONOutput(t *testing.T) {
	m := NewManager()
	cmdPath := helperCommand(t, modeBlock)

	out, err := m.runCommand(context.Background(), cmdPath, HookInput{Event: BeforeTool})
	if err != nil {
		t.Fatalf("runCommand error = %v", err)
	}
	if out.Continue {
		t.Error("Continue = true, want false: the hook printed Continue=false")
	}
	if out.Message != "denied by command hook" {
		t.Errorf("Message = %q, want the hook's message", out.Message)
	}
}

func TestRunCommandTreatsNonJSONStdoutAsNonBlockingMessage(t *testing.T) {
	m := NewManager()
	cmdPath := helperCommand(t, modeNonJSON)

	out, err := m.runCommand(context.Background(), cmdPath, HookInput{Event: BeforeTool})
	if err != nil {
		t.Fatalf("runCommand error = %v", err)
	}
	if !out.Continue {
		t.Error("non-JSON output must be treated as non-blocking")
	}
	if out.Message != "not json at all" {
		t.Errorf("Message = %q, want the raw stdout text", out.Message)
	}
	if out.Modified {
		t.Error("Modified must be false for non-JSON output")
	}
}

func TestRunCommandFailsOpenOnCommandError(t *testing.T) {
	m := NewManager()
	cmdPath := helperCommand(t, modeFail)

	out, err := m.runCommand(context.Background(), cmdPath, HookInput{Event: BeforeTool})
	if err == nil {
		t.Fatal("runCommand returned nil error for a command that exited 3")
	}
	if !strings.Contains(err.Error(), "hook command") {
		t.Errorf("error = %v, want it wrapped with 'hook command'", err)
	}
	if !out.Continue {
		t.Error("a failing hook command must not block the action")
	}
}

func TestRunCommandFailsOpenOnMissingBinary(t *testing.T) {
	m := NewManager()
	out, err := m.runCommand(context.Background(), "definitely-not-a-real-cove-hook-binary", HookInput{Event: BeforeTool})
	if err == nil {
		t.Fatal("runCommand returned nil error for a missing binary")
	}
	if !out.Continue {
		t.Error("a missing hook binary must not block the action")
	}
}

func TestRunCommandRespectsAlreadyCanceledContext(t *testing.T) {
	m := NewManager()
	cmdPath := helperCommand(t, modeEchoInput)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := m.runCommand(ctx, cmdPath, HookInput{Event: BeforeTool})
	if err == nil {
		t.Fatal("runCommand with a canceled context returned nil error")
	}
	if !out.Continue {
		t.Error("a canceled hook must not block the action")
	}
}

// ----------------------------------------------------------------------------
// Timeouts
// ----------------------------------------------------------------------------

// TestFireSequentialCommandHookHonoursTimeout proves a hook command that hangs
// is killed at its Timeout instead of stalling the turn forever.
func TestFireSequentialCommandHookHonoursTimeout(t *testing.T) {
	m := NewManager()
	ln := newLocalListener(t)
	cmdPath := helperCommand(t, modeDialHang)

	// The timeout must outlast process startup, not just the dial: the helper
	// is a re-exec of this test binary, which on Windows — and especially
	// under -race — routinely takes several hundred milliseconds. A 300ms
	// budget killed the child before it could connect, so the test failed for
	// a reason unrelated to the contract. The helper then sleeps 10 minutes,
	// so any bound far below that still proves the timeout fired.
	const timeout = 5 * time.Second
	register(m, HookConfig{
		Event:      BeforeTool,
		Type:       HookCommand,
		Command:    cmdPath,
		Sequential: true,
		Timeout:    timeout,
	})

	start := time.Now()
	done := fireAsync(t, m, context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})

	conn := acceptWithin(t, ln, 30*time.Second) // the hook process really started
	out := waitOutput(t, done, 30*time.Second)  // and Fire came back
	elapsed := time.Since(start)

	if !out.Continue {
		t.Errorf("a hook killed by its timeout blocked the action: %+v", out)
	}
	if elapsed < timeout/2 {
		t.Errorf("Fire returned after %v, before the %v timeout could have elapsed: the command cannot have run", elapsed, timeout)
	}

	// The killed process closes its socket; reading proves it is really gone
	// rather than lingering for the rest of the session.
	assertConnClosed(t, conn, timeout+30*time.Second)
}

// TestFireAsyncHookIsNotAwaited checks the fire-and-forget contract: Fire must
// return while the async hook is still running.
func TestFireAsyncHookIsNotAwaited(t *testing.T) {
	m := NewManager()

	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	register(m, HookConfig{
		Event:      SessionStart,
		Type:       HookRuntime,
		Sequential: false,
		RuntimeFn: func(HookInput) (HookOutput, error) {
			close(started)
			<-release
			close(finished)
			return HookOutput{Continue: true}, nil
		},
	})

	done := fireAsync(t, m, context.Background(), SessionStart, "", HookInput{Event: SessionStart})

	// Fire must not wait for the hook, which is still parked on <-release.
	out := waitOutput(t, done, 30*time.Second)
	if !out.Continue {
		t.Errorf("async hook affected the Fire result: %+v", out)
	}

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("async hook never started")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("async hook never finished")
	}
}

// TestFireAsyncHookIgnoresBlockingResult documents that an async hook cannot
// deny an action: its Continue=false is discarded because nobody waits for it.
func TestFireAsyncHookIgnoresBlockingResult(t *testing.T) {
	m := NewManager()

	ran := make(chan struct{})
	register(m, HookConfig{
		Event:      BeforeTool,
		Type:       HookRuntime,
		Sequential: false,
		RuntimeFn: func(HookInput) (HookOutput, error) {
			close(ran)
			return HookOutput{Continue: false, Message: "too late"}, nil
		},
	})

	out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool})
	if !out.Continue || out.Message != "" {
		t.Fatalf("Fire = %+v, want {Continue:true} — an async hook must not be able to block", out)
	}
	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("async hook never ran")
	}
}

// TestFireAsyncCommandHookSurvivesCallerContextCancellation is the regression
// test for the detached-context contract: the caller's ctx is the turn's
// context and dies as soon as the turn ends, so async hooks must NOT inherit
// it. Timeout is left at 0 to also cover the defaultAsyncHookTimeout fallback:
// a zero timeout must not mean "already expired".
func TestFireAsyncCommandHookSurvivesCallerContextCancellation(t *testing.T) {
	m := NewManager()
	ln := newLocalListener(t)
	cmdPath := helperCommand(t, modeDialExit)

	register(m, HookConfig{
		Event:      SessionEnd,
		Type:       HookCommand,
		Command:    cmdPath,
		Sequential: false,
		Timeout:    0,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the turn is already over before the hook is fired

	out := m.Fire(ctx, SessionEnd, "", HookInput{Event: SessionEnd})
	if !out.Continue {
		t.Fatalf("Fire = %+v, want Continue=true", out)
	}

	// If the async hook had inherited the canceled ctx, the process would have
	// been killed before it could connect back.
	acceptWithin(t, ln, 30*time.Second)
}

// TestFireAsyncCommandHookHonoursItsOwnTimeout is the other half of that
// contract: "fire and forget" must not mean "runs forever". A hanging async
// hook has to be abandoned, and its child process killed.
func TestFireAsyncCommandHookHonoursItsOwnTimeout(t *testing.T) {
	m := NewManager()
	ln := newLocalListener(t)
	cmdPath := helperCommand(t, modeDialHang)

	// The timeout has to outlast process startup, not just the dial: the helper
	// is a re-exec of this test binary, which on Windows routinely takes
	// several hundred milliseconds. A 300ms budget killed the child before it
	// could ever connect, so the test failed for a reason that had nothing to
	// do with the contract under test. What is asserted is that the hook IS
	// eventually abandoned — the helper sleeps 10 minutes, so any bound far
	// below that proves the timeout fired.
	const hookTimeout = 5 * time.Second
	register(m, HookConfig{
		Event:      AfterTool,
		Type:       HookCommand,
		Command:    cmdPath,
		Sequential: false,
		Timeout:    hookTimeout,
	})

	// Even a live caller context must not keep the hook alive past its timeout.
	out := m.Fire(context.Background(), AfterTool, "Bash", HookInput{Event: AfterTool})
	if !out.Continue {
		t.Fatalf("Fire = %+v, want Continue=true", out)
	}

	conn := acceptWithin(t, ln, 30*time.Second)
	if got := readMarker(t, conn, 30*time.Second); got != helperMarker {
		t.Fatalf("hook process sent %q, want %q", got, helperMarker)
	}
	// The helper is sleeping for 10 minutes. If the connection closes, the
	// process was killed — which can only be the hook's timeout firing.
	assertConnClosed(t, conn, hookTimeout+30*time.Second)
}

func TestDefaultAsyncHookTimeoutIsBounded(t *testing.T) {
	// The fallback exists so a hook with no Timeout cannot outlive the session;
	// a zero or absurd value would defeat that.
	if defaultAsyncHookTimeout <= 0 {
		t.Fatalf("defaultAsyncHookTimeout = %v, want a positive bound", defaultAsyncHookTimeout)
	}
	if defaultAsyncHookTimeout > 10*time.Minute {
		t.Fatalf("defaultAsyncHookTimeout = %v, too long to bound a hung hook", defaultAsyncHookTimeout)
	}
}

// ----------------------------------------------------------------------------
// Concurrency + legacy API
// ----------------------------------------------------------------------------

func TestFireIsSafeForConcurrentCallers(t *testing.T) {
	m := NewManager()

	var mu sync.Mutex
	calls := 0
	register(m,
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: true, Matcher: "^Bash$",
			RuntimeFn: func(HookInput) (HookOutput, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				return HookOutput{Continue: true}, nil
			}},
		HookConfig{Event: BeforeTool, Type: HookRuntime, Sequential: false,
			RuntimeFn: func(HookInput) (HookOutput, error) { return HookOutput{Continue: true}, nil }},
	)

	const goroutines, each = 8, 50
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < each; j++ {
				if out := m.Fire(context.Background(), BeforeTool, "Bash", HookInput{Event: BeforeTool}); !out.Continue {
					t.Error("Fire blocked unexpectedly under concurrency")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if want := goroutines * each; calls != want {
		t.Fatalf("sequential hook ran %d times, want %d", calls, want)
	}
}

func TestFireDispatchesOnToolNameTarget(t *testing.T) {
	m := NewManager()

	var mu sync.Mutex
	var got []HookInput
	register(m,
		HookConfig{Event: PreToolUse, Type: HookRuntime, Sequential: true, Matcher: "^Bash$",
			RuntimeFn: func(in HookInput) (HookOutput, error) {
				mu.Lock()
				got = append(got, in)
				mu.Unlock()
				return HookOutput{Continue: true}, nil
			}},
		HookConfig{Event: PreToolUse, Type: HookRuntime, Sequential: true, Matcher: "^Write$",
			RuntimeFn: func(HookInput) (HookOutput, error) {
				t.Error("hook matching a different tool ran")
				return HookOutput{Continue: true}, nil
			}},
	)

	m.Fire(context.Background(), PreToolUse, "Bash", HookInput{
		Event:     PreToolUse,
		ToolName:  "Bash",
		ToolInput: map[string]any{"command": "echo hi"},
	})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("hook ran %d times, want 1", len(got))
	}
	if got[0].Event != PreToolUse {
		t.Errorf("Event = %q, want %q", got[0].Event, PreToolUse)
	}
	if got[0].ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", got[0].ToolName)
	}
	if got[0].ToolInput["command"] != "echo hi" {
		t.Errorf("ToolInput = %v, want command=echo hi", got[0].ToolInput)
	}
}

func TestFireWithEmptyTargetSkipsToolSpecificHooks(t *testing.T) {
	m := NewManager()

	var mu sync.Mutex
	ran := map[string]int{}
	register(m,
		HookConfig{Event: SessionStart, Type: HookRuntime, Sequential: true,
			RuntimeFn: func(HookInput) (HookOutput, error) {
				mu.Lock()
				ran["any"]++
				mu.Unlock()
				return HookOutput{Continue: true}, nil
			}},
		HookConfig{Event: SessionStart, Type: HookRuntime, Sequential: true, Matcher: "^Bash$",
			RuntimeFn: func(HookInput) (HookOutput, error) {
				mu.Lock()
				ran["bash"]++
				mu.Unlock()
				return HookOutput{Continue: true}, nil
			}},
	)

	m.Fire(context.Background(), SessionStart, "", HookInput{Event: SessionStart})

	mu.Lock()
	defer mu.Unlock()
	if ran["any"] != 1 {
		t.Errorf("unmatched hook ran %d times, want 1", ran["any"])
	}
	if ran["bash"] != 0 {
		t.Errorf("tool-specific hook ran %d times for a nil payload, want 0", ran["bash"])
	}
}

func TestLegacyEventAliases(t *testing.T) {
	// engine.go fires PreToolUse/PostToolUse while hooks are registered under
	// BeforeTool/AfterTool: the aliases must stay identical or those hooks
	// would silently never run.
	if PreToolUse != BeforeTool {
		t.Errorf("PreToolUse = %q, want it identical to BeforeTool (%q)", PreToolUse, BeforeTool)
	}
	if PostToolUse != AfterTool {
		t.Errorf("PostToolUse = %q, want it identical to AfterTool (%q)", PostToolUse, AfterTool)
	}
}

func TestCopyHooksReturnsAnIndependentSnapshot(t *testing.T) {
	m := NewManager()
	register(m, HookConfig{Event: BeforeTool, Type: HookRuntime, Matcher: "original"})

	snapshot := m.copyHooks(BeforeTool)
	if len(snapshot) != 1 {
		t.Fatalf("copyHooks returned %d hooks, want 1", len(snapshot))
	}
	snapshot[0].Matcher = "mutated"

	if again := m.copyHooks(BeforeTool); again[0].Matcher != "original" {
		t.Errorf("mutating the snapshot changed the manager's hook: Matcher = %q", again[0].Matcher)
	}
	if got := m.copyHooks(SessionEnd); got != nil {
		t.Errorf("copyHooks for an unused event = %v, want nil", got)
	}
}

// ----------------------------------------------------------------------------
// helpers for observing the helper process
// ----------------------------------------------------------------------------

func readMarker(t *testing.T, conn net.Conn, d time.Duration) string {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, len(helperMarker))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading the hook's marker: %v", err)
	}
	return string(buf[:n])
}

// assertConnClosed waits for the helper process's socket to close, which
// happens when the process dies. It is an event, not a sleep: if the hook is
// never abandoned, the read deadline expires and the test fails.
func assertConnClosed(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 64)
	for {
		_, err := conn.Read(buf)
		if err == nil {
			continue // drain the marker bytes, keep waiting for the close
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("hook process was still alive after %v: its timeout never abandoned it", d)
		}
		return // EOF / reset: the process is gone
	}
}
