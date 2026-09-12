package engine

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
	"github.com/liuzhixin405/cove/internal/tool"
)

// authorizeToolCall is the single gate for both top-level tool calls and the
// sub-agents that plan execution spawns. These tests pin the branches that the
// end-to-end permission tests do not reach.

func TestAuthorizeToolCallApprovesWhenTheUserAccepts(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}
	eng := newTestEngine(&mockProvider{}, writeTool)
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)

	var sawReason string
	eng.PermissionPrompt = func(toolName string, input map[string]any, reason string) bool {
		sawReason = reason
		return true
	}

	tc := api.ToolCall{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}}
	if err := eng.authorizeToolCall(tc, tool.Context{PermissionMode: "default"}, nil); err != nil {
		t.Fatalf("a call the user approved was denied: %v", err)
	}
	if sawReason != "write operation" {
		t.Errorf("prompt reason = %q, want the tool's own reason %q", sawReason, "write operation")
	}
}

func TestAuthorizeToolCallDeniesWhenTheUserRejects(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}
	eng := newTestEngine(&mockProvider{}, writeTool)
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return false }

	// The sub-agent path calls this with a bare tctx: no Runtime, no OnProgress,
	// no OnToolStart. It must still work.
	tc := api.ToolCall{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}}
	err := eng.authorizeToolCall(tc, tool.Context{}, nil)
	if err == nil {
		t.Fatal("a write was authorized without the user approving it")
	}
	if !strings.Contains(err.Error(), "user rejected") {
		t.Errorf("error = %v, want it to report the rejection", err)
	}
}

func TestAuthorizeToolCallRejectsUnknownTool(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	err := eng.authorizeToolCall(api.ToolCall{ID: "x", Name: "no_such_tool"}, tool.Context{}, nil)
	if err == nil {
		t.Fatal("an unknown tool was authorized")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("error = %v, want it to name the unknown tool", err)
	}
}

// setWaiting drives the stall monitor. It must be told the engine is blocked
// while the prompt is open, and told again when it closes, or a tool waiting on
// the user is reported as hung.
func TestAuthorizeToolCallReportsWaitingAroundThePrompt(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}
	eng := newTestEngine(&mockProvider{}, writeTool)
	eng.config.PermissionMode = "default"
	eng.perm.SetMode(permission.Default)

	var waiting []bool
	eng.PermissionPrompt = func(string, map[string]any, string) bool {
		// The prompt is open: the last state reported must be "waiting".
		if len(waiting) == 0 || !waiting[len(waiting)-1] {
			t.Errorf("prompt opened while not marked as waiting; states so far: %v", waiting)
		}
		return true
	}

	tc := api.ToolCall{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}}
	if err := eng.authorizeToolCall(tc, tool.Context{}, func(w bool) { waiting = append(waiting, w) }); err != nil {
		t.Fatalf("unexpected denial: %v", err)
	}
	if len(waiting) != 2 || !waiting[0] || waiting[1] {
		t.Errorf("waiting states = %v, want [true false]", waiting)
	}
}

// Auto mode is the path plan execution normally runs under: no prompt, no
// waiting, straight through.
func TestAuthorizeToolCallAutoModeDoesNotPrompt(t *testing.T) {
	writeTool := &mockTool{name: "write", readOnly: false, result: "written"}
	eng := newTestEngine(&mockProvider{}, writeTool)
	eng.config.PermissionMode = "auto"
	eng.perm.SetMode(permission.Auto)

	eng.PermissionPrompt = func(string, map[string]any, string) bool {
		t.Error("auto mode opened an interactive prompt")
		return false
	}

	tc := api.ToolCall{ID: "tc1", Name: "write", Input: map[string]any{"filePath": "a.go"}}
	if err := eng.authorizeToolCall(tc, tool.Context{PermissionMode: "auto"}, nil); err != nil {
		t.Fatalf("auto mode denied a write: %v", err)
	}
}
