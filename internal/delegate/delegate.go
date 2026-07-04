package delegate

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/log"
	"github.com/liuzhixin405/cove/internal/tool"
)

// SubAgent is an isolated child agent that executes a specific sub-task.
type SubAgent struct {
	provider api.Provider
	model    string
	registry *tool.Registry
	maxIter  int
}

// Config configures a sub-agent.
type Config struct {
	Provider api.Provider
	Model    string
	Tools    []tool.Tool // restricted tool set
	MaxIter  int         // max iterations (default 30)
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
		provider: cfg.Provider,
		model:    cfg.Model,
		registry: reg,
		maxIter:  cfg.MaxIter,
	}
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
				ToolUseID:      tc.ID,
				PermissionMode: "auto", // sub-agents auto-approve within their toolset
			}

			result, err := t.Call(ctx, tc.Input, tctx)
			content := ""
			if err != nil {
				content = "Error: " + err.Error()
			} else {
				content = result.Data
			}
			// Truncate large results
			if len(content) > 4000 {
				content = content[:4000] + "\n[...truncated]"
			}
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
		Provider: d.provider,
		Model:    d.model,
		Tools:    d.tools,
		MaxIter:  30,
	})

	return sa.Run(subCtx, task, systemPrompt)
}
