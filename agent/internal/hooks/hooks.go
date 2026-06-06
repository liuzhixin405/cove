package hooks

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/agentgo/internal/tool"
)

type Event string

const (
	PreToolUse   Event = "pre_tool_use"
	PostToolUse  Event = "post_tool_use"
	SessionStart Event = "session_start"
	SessionEnd   Event = "session_end"
	PreCompact   Event = "pre_compact"
	PostCompact  Event = "post_compact"
)

type ToolUseInfo struct {
	ToolName string
	Input    map[string]any
	Result   string
	IsError  bool
	ToolID   string
}

type Callback func(ctx context.Context, event Event, data any) error

type Manager struct {
	callbacks []Callback
	mu        sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{callbacks: make([]Callback, 0)}
}

func (m *Manager) Register(cb Callback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks = append(m.callbacks, cb)
}

func (m *Manager) Fire(ctx context.Context, event Event, data any) {
	m.mu.RLock()
	cbs := make([]Callback, len(m.callbacks))
	copy(cbs, m.callbacks)
	m.mu.RUnlock()

	for _, cb := range cbs {
		if err := cb(ctx, event, data); err != nil {
		}
	}
}

type ToolResultOverride struct {
	Override bool
	Result   tool.Result
	Reason   string
}

func (m *Manager) PreToolUseHook(ctx context.Context, info ToolUseInfo) (*ToolResultOverride, error) {
	var override *ToolResultOverride
	m.Fire(ctx, PreToolUse, info)
	for _, cb := range m.callbacks {
		if err := cb(ctx, PreToolUse, info); err != nil {
			if strings.HasPrefix(err.Error(), "override:") {
				parts := strings.SplitN(err.Error(), ":", 3)
				if len(parts) >= 2 {
					override = &ToolResultOverride{
						Override: true,
						Result:   tool.Result{Data: parts[1], IsError: parts[1] != ""},
						Reason:   parts[len(parts)-1],
					}
				}
			}
		}
	}
	return override, nil
}

type HookError struct {
	Hook string
	Err  error
}

func (e *HookError) Error() string {
	return fmt.Sprintf("hook %s: %v", e.Hook, e.Err)
}

func Override(result string) error {
	return fmt.Errorf("override:%s:blocked", result)
}
