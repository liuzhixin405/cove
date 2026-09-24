package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	executor       Executor
	budgetExceeded func() bool
}

// Authorizer decides whether a sub-agent may run one tool call. It is the same
// gate the engine applies to top-level calls, handed down as a callback so the
// delegate package does not have to know about permission modes or prompts.
// A nil return means the call may proceed; a non-nil error is shown to the
// sub-agent as the tool's result.
type Authorizer func(ctx context.Context, tc api.ToolCall) error

// Executor runs one tool call and returns the text handed back to the model.
// The engine supplies its own tool pipeline here (safety scan, hooks,
// guardrails, validation, permission, checkpoints, output limits), so a
// sub-agent's calls get exactly the treatment top-level calls get.
type Executor func(ctx context.Context, tc api.ToolCall) string

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
	// are all read-only; see gate(). Unused when Executor is set.
	Authorize Authorizer
	// Executor, when set, runs each tool call instead of the sub-agent.
	Executor Executor
	// BudgetExceeded, when set, is checked before every model call; a true
	// result stops the sub-agent.
	BudgetExceeded func() bool
}

// Options adjusts a single delegated task.
type Options struct {
	// ReadOnly restricts the sub-agent to read-only tools.
	ReadOnly bool
}

// excludedTools are never offered to a sub-agent. The first group would let
// it spawn further sub-agents (execute_plan and team_create both run the plan
// executor); the second mutates the plan and task state that the parent's
// executor is driving; the last would have a background sub-agent prompt the
// user, flip the session's mode, or switch the session's active worktree.
var excludedTools = map[string]bool{
	"agent": true, "execute_plan": true, "team_create": true, "team_delete": true,
	"todowrite": true, "task": true, "task_update": true, "task_stop": true, "cron": true,
	"question": true, "plan_mode": true, "exit_plan_mode": true,
	"worktree": true, "exit_worktree": true,
}

// loopLimit is how many consecutive identical tool-call batches end a run.
const loopLimit = 3

// subAgentResultBytes backstops the size of one tool result in a sub-agent.
const subAgentResultBytes = 32 * 1024

// NewSubAgent creates a new isolated sub-agent.
func NewSubAgent(cfg Config) *SubAgent {
	if cfg.MaxIter == 0 {
		cfg.MaxIter = 30
	}
	reg := tool.NewRegistry()
	for _, t := range cfg.Tools {
		if excludedTools[t.Def().Name] {
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
		executor:       cfg.Executor,
		budgetExceeded: cfg.BudgetExceeded,
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

	// Token usage across every model call the sub-agent made, and the model
	// that served them, so callers can report what the task cost.
	InputTokens  int
	OutputTokens int
	Model        string
}

// toolDefsFor builds the API tool definitions with each tool's real schema.
func toolDefsFor(tools []tool.Tool) []api.ToolDef {
	var defs []api.ToolDef
	for _, t := range tools {
		d := t.Def()
		schema := map[string]any{"type": "object"}
		if len(d.InputSchema) > 0 {
			var parsed map[string]any
			if err := json.Unmarshal(d.InputSchema, &parsed); err == nil && parsed != nil {
				schema = parsed
			}
		}
		defs = append(defs, api.ToolDef{Name: d.Name, Description: d.Description, InputSchema: schema})
	}
	return defs
}

// fingerprint identifies a tool-call batch by names and arguments, ignoring
// the call IDs the provider assigns.
func fingerprint(calls []api.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, tc := range calls {
		args, _ := json.Marshal(tc.Input) // map keys marshal sorted
		parts = append(parts, tc.Name+string(args))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// Run executes the sub-agent's task and returns a summary.
func (sa *SubAgent) Run(ctx context.Context, task string, systemPrompt string) *Result {
	messages := []api.Message{{Role: "user", Content: task}}
	toolDefs := toolDefsFor(sa.registry.All())
	res := &Result{Model: sa.model}

	var lastPrint string
	repeats := 0
	for iter := 0; iter < sa.maxIter; iter++ {
		res.Steps = iter
		select {
		case <-ctx.Done():
			// A deadline is the sub-agent's own time limit running out, not
			// the user cancelling; "cancelled" for both made a timeout look
			// like a Ctrl+C in the plan summary.
			res.Error = "cancelled"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Error = "timed out"
			}
			return res
		default:
		}
		if sa.budgetExceeded != nil && sa.budgetExceeded() {
			res.Error = "budget exceeded"
			return res
		}

		resp, err := sa.provider.Chat(ctx, api.ChatRequest{
			Model:      sa.model,
			Messages:   messages,
			SystemBase: systemPrompt,
			Tools:      toolDefs,
			MaxTokens:  16000,
		})
		if err != nil {
			res.Error = err.Error()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.Error = "timed out: " + res.Error
			}
			return res
		}
		res.InputTokens += resp.InputTokens
		res.OutputTokens += resp.OutputTokens
		if resp.Model != "" {
			res.Model = resp.Model
		}

		if len(resp.ToolCalls) == 0 {
			res.Output, res.Success, res.Steps = resp.Content, true, iter+1
			return res
		}

		if fp := fingerprint(resp.ToolCalls); fp == lastPrint {
			repeats++
		} else {
			lastPrint, repeats = fp, 1
		}
		if repeats >= loopLimit {
			res.Error = fmt.Sprintf("loop detected: the same tool calls were requested %d times in a row", repeats)
			res.Steps = iter + 1
			return res
		}

		messages = append(messages, api.Message{
			Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls, ThinkingBlocks: resp.ThinkingBlocks,
		})
		for _, tc := range resp.ToolCalls {
			content := sa.runTool(ctx, tc)
			// Truncate large results on a rune boundary (a byte slice lands
			// inside a multi-byte rune for Chinese output, and invalid UTF-8
			// goes straight into the next request's JSON body). The engine's
			// executor already applies its own, window-aware limit; this is
			// only a backstop, so it must not be the binding one — at 4000
			// bytes a sub-agent saw little more than the first page of a file.
			content = textutil.ClipBytes(content, subAgentResultBytes, "\n[...truncated]")
			messages = append(messages, api.Message{
				Role: "tool", ToolCallID: tc.ID, Name: tc.Name, Content: content,
			})
		}
	}

	res.Error, res.Steps = "max iterations reached", sa.maxIter
	return res
}

// runTool executes one call and returns the text for its tool result.
func (sa *SubAgent) runTool(ctx context.Context, tc api.ToolCall) string {
	// The sub-agent's own registry decides what it may call. The executor
	// can reach every engine tool, including the excluded ones.
	t, ok := sa.registry.Find(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name)
	}
	if sa.executor != nil {
		return sa.executor(ctx, tc)
	}
	if err := sa.gate(ctx, tc, t); err != nil {
		return "Error: " + err.Error()
	}
	result, err := t.Call(ctx, tc.Input, tool.Context{
		Cwd:            sa.cwd,
		ToolUseID:      tc.ID,
		PermissionMode: sa.permissionMode,
	})
	if err != nil {
		return "Error: " + err.Error()
	}
	return result.Data
}

// Delegator manages sub-agent lifecycle.
type Delegator struct {
	mu     sync.Mutex
	tools  []tool.Tool
	active map[string]context.CancelFunc

	// providerSource is consulted when each task starts, so a provider or
	// model switch after construction reaches later sub-agents.
	providerSource func() (api.Provider, string)

	// Tool gate handed to every sub-agent this delegator spawns. Set before
	// the first Delegate call; not guarded by mu.
	cwd            string
	permissionMode string
	authorize      Authorizer
	executor       Executor
	budgetExceeded func() bool
}

// NewDelegator creates a sub-agent delegator.
func NewDelegator(provider api.Provider, model string, tools []tool.Tool) *Delegator {
	return &Delegator{
		tools:          tools,
		active:         make(map[string]context.CancelFunc),
		providerSource: func() (api.Provider, string) { return provider, model },
	}
}

// SetProviderSource makes the delegator resolve its provider and model when
// each task starts.
func (d *Delegator) SetProviderSource(src func() (api.Provider, string)) {
	d.providerSource = src
}

// SetGate installs the permission gate that sub-agents run their tool calls
// through, along with the project directory they operate in. Call it before the
// first Delegate. A delegator with no gate can only run read-only tools.
func (d *Delegator) SetGate(cwd, permissionMode string, authorize Authorizer) {
	d.cwd = cwd
	d.permissionMode = permissionMode
	d.authorize = authorize
}

// SetExecutor routes every sub-agent tool call through exec. It supersedes the
// gate installed by SetGate.
func (d *Delegator) SetExecutor(exec Executor) { d.executor = exec }

// SetBudgetCheck stops sub-agents before a model call once exceeded() is true.
func (d *Delegator) SetBudgetCheck(exceeded func() bool) { d.budgetExceeded = exceeded }

// Delegate spawns a sub-agent for the given task. Blocks until completion.
func (d *Delegator) Delegate(ctx context.Context, taskID, task, systemPrompt string) *Result {
	return d.DelegateWith(ctx, taskID, task, systemPrompt, Options{})
}

// DelegateWith is Delegate with per-task options.
func (d *Delegator) DelegateWith(ctx context.Context, taskID, task, systemPrompt string, opts Options) *Result {
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

	tools := d.tools
	if opts.ReadOnly {
		tools = nil
		for _, t := range d.tools {
			if t.Def().IsReadOnly {
				tools = append(tools, t)
			}
		}
	}
	provider, model := d.providerSource()
	sa := NewSubAgent(Config{
		Provider:       provider,
		Model:          model,
		Tools:          tools,
		MaxIter:        30,
		Cwd:            d.cwd,
		PermissionMode: d.permissionMode,
		Authorize:      d.authorize,
		Executor:       d.executor,
		BudgetExceeded: d.budgetExceeded,
	})

	return sa.Run(subCtx, task, systemPrompt)
}
