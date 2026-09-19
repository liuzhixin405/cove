package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
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
	RuntimeFn  func(HookInput) (HookOutput, error) // Go callback (for HookRuntime)
	Timeout    time.Duration                       // max execution time (0 = no limit)
	Sequential bool                                // true = must complete before continuing; false = fire-and-forget
}

// defaultAsyncHookTimeout bounds an async hook that declares no Timeout of its
// own, so a hung hook process cannot outlive the session.
const defaultAsyncHookTimeout = 60 * time.Second

// HookInput is the data passed to a hook when it fires.
type HookInput struct {
	Event     HookEvent      `json:"event"`
	ToolName  string         `json:"tool_name,omitempty"`
	ToolInput map[string]any `json:"tool_input,omitempty"`
	Model     string         `json:"model,omitempty"`
	Messages  []api.Message  `json:"-"`
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

// executeHook runs a single hook and returns its output.
func (m *Manager) executeHook(ctx context.Context, h HookConfig, input HookInput) (HookOutput, error) {
	switch h.Type {
	case HookRuntime:
		if h.RuntimeFn == nil {
			return HookOutput{Continue: true}, nil
		}
		return h.RuntimeFn(input)
	case HookCommand:
		return m.runCommand(ctx, h.Command, input)
	default:
		return HookOutput{Continue: true}, fmt.Errorf("unknown hook type: %v", h.Type)
	}
}

// runCommand executes an external script, passing HookInput as JSON on stdin
// and reading HookOutput as JSON from stdout.
func (m *Manager) runCommand(ctx context.Context, cmdPath string, input HookInput) (HookOutput, error) {
	inJSON, err := json.Marshal(input)
	if err != nil {
		return HookOutput{Continue: true}, fmt.Errorf("hook marshal: %w", err)
	}

	cmd := exec.CommandContext(ctx, cmdPath)
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
	if err != nil {
		return HookOutput{Continue: true}, fmt.Errorf("hook command: %w", err)
	}

	var output HookOutput
	if err := json.Unmarshal(out, &output); err != nil {
		// If the command didn't return valid JSON, treat as non-blocking
		return HookOutput{Continue: true, Message: string(out)}, nil
	}
	return output, nil
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

// matches checks whether the hook's Matcher regex matches the target.
// An empty Matcher matches everything.
func (m *Manager) matches(h HookConfig, target string) bool {
	if h.Matcher == "" {
		return true
	}
	matched, err := regexp.MatchString(h.Matcher, target)
	if err != nil {
		log.Warnf("hook regex error: %v", err)
		return false
	}
	return matched
}

// Legacy event aliases, kept because hook config files and docs spell the
// pre/post-tool events both ways.
const (
	PreToolUse  HookEvent = BeforeTool
	PostToolUse HookEvent = AfterTool
)
