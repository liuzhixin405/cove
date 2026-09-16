package delegate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/textutil"
	"github.com/liuzhixin405/cove/internal/tool"
)

// SubAgent is an isolated child agent that executes a specific sub-task.
type SubAgent struct {
	provider       api.Provider
	model          string
	registry       *tool.Registry
	maxIter        int
	cwd            string
	permissionMode string
	authorize      Authorizer
}

// Authorizer decides whether a sub-agent may run one tool call. It is the same
// gate the engine applies to top-level calls, handed down as a callback so the
// delegate package does not have to know about permission modes or prompts.
// A nil return means the call may proceed; a non-nil error is shown to the
// sub-agent as the tool's result.
type Authorizer func(ctx context.Context, tc api.ToolCall) error

// Config configures a sub-agent.
type Config struct {
	Provider api.Provider
	Model    string
	Tools    []tool.Tool // restricted tool set
	MaxIter  int         // max iterations (default 30)
	// Cwd is the project directory tool calls resolve relative paths against.
	Cwd string
	// PermissionMode is the session's real mode. It is passed through to
	// tool.Context so tools that branch on it see the truth rather than a
	// hardcoded "auto".
	PermissionMode string
	// Authorize gates every tool call. Leave nil only if the sub-agent's tools
	// are all read-only; see gate().
	Authorize Authorizer
}

// NewSubAgent creates a new isolated sub-agent.
func NewSubAgent(cfg Config) *SubAgent {
	if cfg.MaxIter == 0 {
		cfg.MaxIter = 30
	}
	reg := tool.NewRegistry()
	for _, t := range cfg.Tools {
		// Never allow delegate in sub-agents (prevent recursion)
		if t.Def().Name == "delegate" {
			continue
		}
		reg.Register(t)
	}
	return &SubAgent{
		provider:       cfg.Provider,
		model:          cfg.Model,
		registry:       reg,
		maxIter:        cfg.MaxIter,
		cwd:            cfg.Cwd,
		permissionMode: cfg.PermissionMode,
		authorize:      cfg.Authorize,
	}
}

// gate reports whether the sub-agent may run this tool call.
func (sa *SubAgent) gate(ctx context.Context, tc api.ToolCall, t tool.Tool) error {
	if sa.authorize != nil {
		return sa.authorize(ctx, tc)
	}
	// No gate was wired. Fail closed: a sub-agent that can mutate the
	// workspace with nobody checking is the exact hole this gate closes, so
	// only read-only tools go through. Read-only tools stay usable so a
	// sub-agent is still worth spawning.
	if t.Def().IsReadOnly {
		return nil
	}
	return fmt.Errorf("no permission gate configured for sub-agent: refusing non-read-only tool %q", tc.Name)
}

// Result is the outcome of a sub-agent task.
type Result struct {
	Output  string
	Steps   int
	Success bool
	Error   string
}

// Run executes the sub-agent's task and returns a summary.
func (sa *SubAgent) Run(ctx context.Context, task string, systemPrompt string) *Result {
	messages := []api.Message{{Role: "user", Content: task}}

	var toolDefs []api.ToolDef
	for _, t := range sa.registry.All() {
		d := t.Def()
		schema := make(map[string]any)
		if len(d.InputSchema) > 0 {
			schema["type"] = "object"
		}
		toolDefs = append(toolDefs, api.ToolDef{
			Name:        d.Name,
			Description: d.Description,
			InputSchema: schema,
		})
	}

	for iter := 0; iter < sa.maxIter; iter++ {
		select {
		case <-ctx.Done():
			return &Result{Error: "cancelled", Success: false, Steps: iter}
		default:
		}

		resp, err := sa.provider.Chat(ctx, api.ChatRequest{
			Model:      sa.model,
			Messages:   messages,
			SystemBase: systemPrompt,
			Tools:      toolDefs,
			MaxTokens:  16000,
		})
		if err != nil {
			return &Result{Error: err.Error(), Success: false, Steps: iter}
		}

		if len(resp.ToolCalls) == 0 {
			return &Result{Output: resp.Content, Success: true, Steps: iter + 1}
		}

		// Execute tools
		messages = append(messages, api.Message{
			Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls,
		})

		for _, tc := range resp.ToolCalls {
			t, ok := sa.registry.Find(tc.Name)
			if !ok {
				messages = append(messages, api.Message{
					Role: "tool", ToolCallID: tc.ID, Name: tc.Name,
					Content: fmt.Sprintf("Error: unknown tool %q", tc.Name),
				})
				continue
			}

			tctx := tool.Context{
				Cwd:            sa.cwd,
				ToolUseID:      tc.ID,
				PermissionMode: sa.permissionMode,
			}

			if err := sa.gate(ctx, tc, t); err != nil {
				messages = append(messages, api.Message{
					Role: "tool", ToolCallID: tc.ID, Name: tc.Name,
					Content: "Error: " + err.Error(),
				})
				continue
			}

			result, err := t.Call(ctx, tc.Input, tctx)
			content := ""
			if err != nil {
				content = "Error: " + err.Error()
			} else {
				content = result.Data
			}
			// Truncate large results on a rune boundary. A byte slice at 4000
			// lands inside a multi-byte rune for the Chinese tool output this
			// project produces constantly, and the resulting invalid UTF-8 goes
			// straight into the next request's JSON body.
			content = textutil.ClipBytes(content, 4000, "\n[...truncated]")
			messages = append(messages, api.Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: content,
			})
		}
	}

	return &Result{Error: "max iterations reached", Success: false, Steps: sa.maxIter}
}

// Delegator manages sub-agent lifecycle.
type Delegator struct {
	mu       sync.Mutex
	provider api.Provider
	model    string
	tools    []tool.Tool
	active   map[string]context.CancelFunc

	// Permission gate handed to every sub-agent this delegator spawns. Set
	// before the first Delegate call; not guarded by mu.
	cwd            string
	permissionMode string
	authorize      Authorizer
}

// NewDelegator creates a sub-agent delegator.
func NewDelegator(provider api.Provider, model string, tools []tool.Tool) *Delegator {
	return &Delegator{
		provider: provider,
		model:    model,
		tools:    tools,
		active:   make(map[string]context.CancelFunc),
	}
}

// SetGate installs the permission gate that sub-agents run their tool calls
// through, along with the project directory they operate in. Call it before the
// first Delegate. A delegator with no gate can only run read-only tools.
func (d *Delegator) SetGate(cwd, permissionMode string, authorize Authorizer) {
	d.cwd = cwd
	d.permissionMode = permissionMode
	d.authorize = authorize
}

// Delegate spawns a sub-agent for the given task. Blocks until completion.
func (d *Delegator) Delegate(ctx context.Context, taskID, task, systemPrompt string) *Result {
	subCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)

	d.mu.Lock()
	d.active[taskID] = cancel
	d.mu.Unlock()

	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.active, taskID)
		d.mu.Unlock()
	}()

	log.Debugf("delegate: starting sub-agent for task %s", taskID)

	sa := NewSubAgent(Config{
		Provider:       d.provider,
		Model:          d.model,
		Tools:          d.tools,
		MaxIter:        30,
		Cwd:            d.cwd,
		PermissionMode: d.permissionMode,
		Authorize:      d.authorize,
	})

	return sa.Run(subCtx, task, systemPrompt)
}
