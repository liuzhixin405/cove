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
	BeforeTool          HookEvent = "BeforeTool"
	AfterTool           HookEvent = "AfterTool"
	BeforeAgent         HookEvent = "BeforeAgent"
	AfterAgent          HookEvent = "AfterAgent"
	BeforeModel         HookEvent = "BeforeModel"
	AfterModel          HookEvent = "AfterModel"
	SessionStart        HookEvent = "SessionStart"
	SessionEnd          HookEvent = "SessionEnd"
	PreCompress         HookEvent = "PreCompress"
	PostCompress        HookEvent = "PostCompress"
	BeforeToolSelection HookEvent = "BeforeToolSelection"
	Notification        HookEvent = "Notification"
)

// HookType distinguishes between in-process Go callbacks and external commands.
type HookType int

const (
	HookRuntime HookType = iota // Go function callback
	HookCommand                  // external command via stdin/stdout
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

// Register adds a hook configuration for a specific event.
func (m *Manager) Register(cfg HookConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hooks[cfg.Event] = append(m.hooks[cfg.Event], cfg)
}

// RegisterFunc is a convenience wrapper for registering a runtime hook.
func (m *Manager) RegisterFunc(event HookEvent, matcher string, sequential bool, fn func(HookInput) (HookOutput, error)) {
	m.Register(HookConfig{
		Event:      event,
		Matcher:    matcher,
		Type:       HookRuntime,
		RuntimeFn:  fn,
		Timeout:    30 * time.Second,
		Sequential: sequential,
	})
}

// RegisterCommand registers an external command hook.
func (m *Manager) RegisterCommand(event HookEvent, matcher string, sequential bool, command string, timeout time.Duration) {
	m.Register(HookConfig{
		Event:      event,
		Matcher:    matcher,
		Type:       HookCommand,
		Command:    command,
		Timeout:    timeout,
		Sequential: sequential,
	})
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

		// Non-sequential hooks run async (fire-and-forget)
		if !h.Sequential {
			go m.executeHook(context.Background(), h, input)
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

// FireAsync runs non-sequential hooks asynchronously. Use for events where
// blocking isn't required (e.g., Notification, SessionEnd).
func (m *Manager) FireAsync(event HookEvent, target string, input HookInput) {
	m.mu.RLock()
	hooks := m.copyHooks(event)
	m.mu.RUnlock()

	for _, h := range hooks {
		if !m.matches(h, target) {
			continue
		}
		go m.executeHook(context.Background(), h, input)
	}
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
		defer stdin.Close()
		stdin.Write(inJSON)
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

// Deprecated: kept for backward compatibility with old hook callers.
type ToolUseInfo struct {
	ToolName string
	Input    map[string]any
	Result   string
	IsError  bool
	ToolID   string
}

type Callback func(ctx context.Context, event Event, data any) error

// RegisterLegacy adds a legacy-style callback.
// Deprecated: use Register() or RegisterFunc() instead.
func (m *Manager) RegisterLegacy(cb Callback) {
	m.RegisterFunc(Notification, "", false, func(input HookInput) (HookOutput, error) {
		event := Event(input.Event)
		if err := cb(context.Background(), event, nil); err != nil {
			return HookOutput{Continue: true}, err
		}
		return HookOutput{Continue: true}, nil
	})
}

// Event is the legacy event type for backward compatibility.
type Event = HookEvent

// Legacy event constants for backward compatibility.
const (
	PreToolUse   HookEvent = BeforeTool
	PostToolUse  HookEvent = AfterTool
	SessionStartEv HookEvent = SessionStart
	SessionEndEv   HookEvent = SessionEnd
	PreCompactEv   HookEvent = PreCompress
	PostCompactEv  HookEvent = PostCompress
)

func Override(result string) error {
	return fmt.Errorf("override:%s:blocked", result)
}

// ============================================================================
// Backward compatibility: legacy API used by engine.go
// ============================================================================

// FireLegacy is the old 3-argument Fire(ctx, event, data) for backward compat.
func (m *Manager) FireLegacy(ctx context.Context, event HookEvent, data any) {
	switch event {
	case BeforeTool, AfterTool:
	default:
		// Treat unknown events as Notification
	}

	var target string
	input := HookInput{Event: event}

	if info, ok := data.(ToolUseInfo); ok {
		target = info.ToolName
		input.ToolName = info.ToolName
		input.ToolInput = info.Input
	}

	m.Fire(ctx, event, target, input)
}

// ToolResultOverride is the legacy result override type.
type ToolResultOverride struct {
	Override bool
	Result   string
	Reason   string
}

// HookError is the legacy hook error type.
type HookError struct {
	Hook string
	Err  error
}

func (e *HookError) Error() string { return fmt.Sprintf("hook %s: %v", e.Hook, e.Err) }

// PreToolUseHook is the legacy pre-tool hook that supports result overrides.
// Kept for backward compatibility.
func (m *Manager) PreToolUseHook(ctx context.Context, info ToolUseInfo) (*ToolResultOverride, error) {
	input := HookInput{
		Event:     BeforeTool,
		ToolName:  info.ToolName,
		ToolInput: info.Input,
	}
	output := m.Fire(ctx, BeforeTool, info.ToolName, input)
	if !output.Continue {
		return &ToolResultOverride{
			Override: true,
			Result:   output.Message,
			Reason:   output.Message,
		}, nil
	}
	return nil, nil
}
