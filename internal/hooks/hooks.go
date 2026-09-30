package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/shell"
)

// HookEvent represents a lifecycle event that can trigger hooks.
type HookEvent string

const (
	BeforeTool   HookEvent = "BeforeTool"
	AfterTool    HookEvent = "AfterTool"
	SessionStart HookEvent = "SessionStart"
	SessionEnd   HookEvent = "SessionEnd"
)

// HookType distinguishes between in-process Go callbacks and external commands.
type HookType int

const (
	HookRuntime HookType = iota // Go function callback
	HookCommand                 // external command via stdin/stdout
)

// HookConfig defines a single hook: when it fires, how it runs.
type HookConfig struct {
	Event      HookEvent                           // which event triggers this hook
	Matcher    string                              // optional regex to filter by tool/model name (empty = all)
	Type       HookType                            // runtime or command
	Command    string                              // command path (for HookCommand)
	Shell      bool                                // Command is a command line for the user's shell, not a path
	RuntimeFn  func(HookInput) (HookOutput, error) // Go callback (for HookRuntime)
	Timeout    time.Duration                       // max execution time (0 = no limit)
	Sequential bool                                // true = must complete before continuing; false = fire-and-forget
}

// defaultAsyncHookTimeout bounds an async hook that declares no Timeout of its
// own, so a hung hook process cannot outlive the session.
const defaultAsyncHookTimeout = 60 * time.Second

// hookPipeWaitDelay is how long runCommand keeps reading a hook's stdout after
// the hook process has exited or been killed; see runCommand.
const hookPipeWaitDelay = 2 * time.Second

// HookInput is the data passed to a hook when it fires.
type HookInput struct {
	Event     HookEvent      `json:"event"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
	Model     string         `json:"model,omitempty"`
	// SessionID and Cwd identify the session for SessionStart/SessionEnd.
	SessionID string        `json:"session_id,omitempty"`
	Cwd       string        `json:"cwd,omitempty"`
	Messages  []api.Message `json:"-"`
}

// HookOutput is returned by a hook. A hook can block execution or modify inputs.
type HookOutput struct {
	Continue bool   // false = block the action that triggered this hook
	Message  string // a message for the AI or user
	Modified bool   // whether the input was modified
}

// Manager orchestrates all registered hooks.
type Manager struct {
	mu    sync.RWMutex
	hooks map[HookEvent][]HookConfig
}

// NewManager creates an empty hook manager.
func NewManager() *Manager {
	return &Manager{
		hooks: make(map[HookEvent][]HookConfig),
	}
}

// Fire synchronously runs all sequential hooks matching the given event + target.
// Returns aggregated HookOutput (all must Continue=true for the action to proceed).
// A command hook blocks by printing {"continue": false} (honoured whatever
// its exit code) or by exiting with code 2; a hook that errors or times out
// otherwise does not block (see failedHookOutput).
func (m *Manager) Fire(ctx context.Context, event HookEvent, target string, input HookInput) HookOutput {
	m.mu.RLock()
	hooks := m.copyHooks(event)
	m.mu.RUnlock()

	output := HookOutput{Continue: true}

	for _, h := range hooks {
		if !m.matches(h, target) {
			continue
		}

		// Non-sequential hooks run async (fire-and-forget).
		//
		// "Fire-and-forget" is about not waiting for the result, not about
		// running forever: the hook's own Timeout must still apply, and the
		// spawned process must still be cancellable. Using a bare
		// context.Background() discarded h.Timeout entirely, so a hook command
		// that hung kept its goroutine and its child process alive for the rest
		// of the session.
		//
		// The caller's ctx is deliberately NOT the parent — it is the turn's
		// context and is cancelled as soon as the turn ends, which would kill
		// every async hook the moment it was fired.
		if !h.Sequential {
			go func(h HookConfig) {
				// context.Background(), NOT the caller's ctx: deriving from ctx
				// makes the async hook die the instant the turn ends, which is
				// the opposite of fire-and-forget. The bound comes from the
				// hook's own Timeout, with a default so a hung hook still
				// cannot outlive the session.
				timeout := h.Timeout
				if timeout <= 0 {
					timeout = defaultAsyncHookTimeout
				}
				asyncCtx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				if _, err := m.executeHook(asyncCtx, h, input); err != nil {
					log.Warnf("async hook %s error: %v", h.Event, err)
				}
			}(h)
			continue
		}

		// Sequential hook — must wait for result
		var hookCtx context.Context
		var cancel context.CancelFunc
		if h.Timeout > 0 {
			hookCtx, cancel = context.WithTimeout(ctx, h.Timeout)
		} else {
			hookCtx = ctx
		}

		result, err := m.executeHook(hookCtx, h, input)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			// An erroring hook fails open. A command hook's veto printed
			// before a non-zero exit is not an error (see failedHookOutput).
			log.Warnf("hook %s error: %v", event, err)
			continue
		}

		if !result.Continue {
			output.Continue = false
			output.Message = result.Message
			return output
		}
		output.Modified = output.Modified || result.Modified
	}

	return output
}

// FireAndWait runs every hook matching event + target — async ones included —
// concurrently and waits for all of them, but not past ctx's deadline. It is for events
// fired as the process exits (SessionEnd): a fire-and-forget hook started
// then would be killed with the process before it did anything. Each hook
// still has its own Timeout (async ones defaultAsyncHookTimeout); nothing can
// be vetoed at exit, so the outputs are only logged.
func (m *Manager) FireAndWait(ctx context.Context, event HookEvent, target string, input HookInput) {
	m.mu.RLock()
	hooks := m.copyHooks(event)
	m.mu.RUnlock()

	var wg sync.WaitGroup
	for _, h := range hooks {
		if !m.matches(h, target) {
			continue
		}
		timeout := h.Timeout
		if timeout <= 0 && !h.Sequential {
			timeout = defaultAsyncHookTimeout
		}
		wg.Add(1)
		go func(h HookConfig, timeout time.Duration) {
			defer wg.Done()
			hookCtx, cancel := ctx, context.CancelFunc(func() {})
			if timeout > 0 {
				hookCtx, cancel = context.WithTimeout(ctx, timeout)
			}
			defer cancel()
			if _, err := m.executeHook(hookCtx, h, input); err != nil {
				log.Warnf("hook %s error: %v", event, err)
			}
		}(h, timeout)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Warnf("%s hooks still running at the deadline: %v", event, ctx.Err())
	}
}

// Has reports whether any hook is registered for event.
func (m *Manager) Has(event HookEvent) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.hooks[event]) > 0
}

// executeHook runs a single hook and returns its output.
func (m *Manager) executeHook(ctx context.Context, h HookConfig, input HookInput) (HookOutput, error) {
	switch h.Type {
	case HookRuntime:
		if h.RuntimeFn == nil {
			return HookOutput{Continue: true}, nil
		}
		return h.RuntimeFn(input)
	case HookCommand:
		if h.Shell {
			sh := shell.Default()
			return m.runProgram(ctx, input, sh.Path, sh.Args(h.Command)...)
		}
		return m.runCommand(ctx, h.Command, input)
	default:
		return HookOutput{Continue: true}, fmt.Errorf("unknown hook type: %v", h.Type)
	}
}

// runCommand executes an external program by path, with no arguments.
func (m *Manager) runCommand(ctx context.Context, cmdPath string, input HookInput) (HookOutput, error) {
	return m.runProgram(ctx, input, cmdPath)
}

// runProgram executes an external program, passing HookInput as JSON on stdin
// and reading HookOutput as JSON from stdout.
func (m *Manager) runProgram(ctx context.Context, input HookInput, cmdPath string, args ...string) (HookOutput, error) {
	inJSON, err := json.Marshal(input)
	if err != nil {
		return HookOutput{Continue: true}, fmt.Errorf("hook marshal: %w", err)
	}

	cmd := exec.CommandContext(ctx, cmdPath, args...)
	// Output reads stdout to EOF, and a background process the hook started
	// inherits that pipe, so without a bound the call waited for the
	// background process too: a hook that launched a notifier stalled the
	// tool call until the notifier exited. WaitDelay stops waiting on the
	// pipe shortly after the hook itself has exited or been killed.
	cmd.WaitDelay = hookPipeWaitDelay
	cmd.Stdin = nil // we'll use a pipe
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return HookOutput{Continue: true}, err
	}

	go func() {
		defer func() { _ = stdin.Close() }()
		_, _ = stdin.Write(inJSON)
	}()

	out, err := cmd.Output()
	// ErrWaitDelay alone means the hook exited cleanly and only a process it
	// left behind still held stdout; what the hook printed is its result.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return failedHookOutput(ctx, out, err)
	}

	// Continue defaults to true: "continue" is the veto, so a hook that
	// prints JSON without it has not asked to block. Decoding into a zero
	// HookOutput used to turn {"message":"..."} into a block.
	output := HookOutput{Continue: true}
	if err := json.Unmarshal(out, &output); err != nil {
		// If the command didn't return valid JSON, treat as non-blocking
		return HookOutput{Continue: true, Message: string(out)}, nil
	}
	return output, nil
}

// failedHookOutput is the result of a hook command that did not exit 0.
//
// Its output used to be discarded, so a hook that printed {"continue":
// false} and then exited non-zero (a script whose last command failed) had
// its veto ignored and the tool ran. Now:
//
//   - Killed at its Timeout or by cancellation: non-blocking, whatever it
//     printed — a half-finished run decides nothing — and the error says it
//     timed out so Fire's warning shows the hook never finished.
//   - Exit code 2: a block, the Claude Code PreToolUse convention (the
//     PreToolUse alias of BeforeTool is accepted, so the convention is too);
//     stderr, else the printed message, is the reason. Not an error.
//   - Any other exit code: a JSON veto on stdout is still honoured; the exit
//     is only logged, since Fire treats an error as "fail open". Without a
//     veto the hook fails open as before.
func failedHookOutput(ctx context.Context, out []byte, err error) (HookOutput, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return HookOutput{Continue: true}, fmt.Errorf("hook command timed out, not blocking: %w", err)
		}
		return HookOutput{Continue: true}, fmt.Errorf("hook command: %w", err)
	}
	printed := HookOutput{Continue: true}
	decoded := json.Unmarshal(out, &printed) == nil
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
		msg := strings.TrimSpace(string(exitErr.Stderr))
		if msg == "" && decoded {
			msg = printed.Message
		}
		if msg == "" {
			msg = "hook 以退出码 2 拒绝了此操作"
		}
		return HookOutput{Continue: false, Message: msg}, nil
	}
	if decoded && !printed.Continue {
		log.Warnf("hook command exited with %v after printing a veto; the veto is honoured", err)
		return printed, nil
	}
	return HookOutput{Continue: true}, fmt.Errorf("hook command: %w", err)
}

// copyHooks returns a safe copy of registered hooks for the given event.
func (m *Manager) copyHooks(event HookEvent) []HookConfig {
	list := m.hooks[event]
	if len(list) == 0 {
		return nil
	}
	cp := make([]HookConfig, len(list))
	copy(cp, list)
	return cp
}

// claudeCodeToolNames lists, for a cove tool, the Claude Code tool names a
// hooks.json matcher may use for it. Matchers used to be compared
// case-sensitively against cove's lowercase names, so hooks copied from a
// Claude Code config ("matcher": "Bash", "Edit|Write") loaded without error
// and then never fired. Matching is now case-insensitive (which covers
// Bash/Edit/Write/Read/Grep/Glob/WebFetch/WebSearch directly); this table adds
// the names that differ: Claude Code's MultiEdit is cove's edit, and its Bash
// also covers cove's powershell tool, the shell tool used on Windows.
var claudeCodeToolNames = map[string][]string{
	"edit":       {"MultiEdit"},
	"powershell": {"Bash"},
}

// matches checks whether the hook's Matcher regex matches the target,
// ignoring case, or matches one of the target's Claude Code tool names (see
// claudeCodeToolNames). An empty Matcher matches everything.
func (m *Manager) matches(h HookConfig, target string) bool {
	if h.Matcher == "" {
		return true
	}
	re, err := regexp.Compile("(?i)" + h.Matcher)
	if err != nil {
		log.Warnf("hook regex error: %v", err)
		return false
	}
	if re.MatchString(target) {
		return true
	}
	for _, alias := range claudeCodeToolNames[strings.ToLower(target)] {
		if re.MatchString(alias) {
			return true
		}
	}
	return false
}

// Legacy event aliases, kept because hook config files and docs spell the
// pre/post-tool events both ways.
const (
	PreToolUse  HookEvent = BeforeTool
	PostToolUse HookEvent = AfterTool
)
