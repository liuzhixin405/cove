package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/permission"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// sideEffectTool is not read-only and answers Allowed itself, like the tools
// that used to run in plan mode because they never refused.
type sideEffectTool struct {
	name     string
	planSafe bool
	calls    int
}

func (s *sideEffectTool) Def() tool.Def {
	return tool.Def{Name: s.name, InputSchema: []byte(`{"type":"object"}`), PlanSafe: s.planSafe}
}
func (s *sideEffectTool) Validate(tool.Input) string { return "" }
func (s *sideEffectTool) CheckPermissions(tool.Input, tool.Context) tool.PermissionDecision {
	return tool.Allowed("says it is fine")
}
func (s *sideEffectTool) Call(context.Context, tool.Input, tool.Context) (tool.Result, error) {
	s.calls++
	return tool.Result{Data: "changed something"}, nil
}

func planEngine(t *testing.T, tools ...tool.Tool) *Engine {
	t.Helper()
	isolatedHome(t)
	t.Chdir(t.TempDir())
	return newTestEngine(&mockProvider{}, tools...)
}

func run1(t *testing.T, eng *Engine, name string, input map[string]any) (string, bool) {
	t.Helper()
	return eng.executeTool(context.Background(), api.ToolCall{ID: "c", Name: name, Input: input})
}

// A tool that is not read-only runs in plan mode no longer just because its
// CheckPermissions answers Allowed; a PlanSafe one still does.
func TestPlanModeAllowsReadOnlyAndPlanSafeToolsOnly(t *testing.T) {
	writer := &sideEffectTool{name: "sidefx"}
	safe := &sideEffectTool{name: "notes", planSafe: true}
	eng := planEngine(t, writer, safe)
	eng.SetPermissionMode(permission.Plan)

	if _, failed := run1(t, eng, "sidefx", nil); !failed || writer.calls != 0 {
		t.Fatalf("a non-read-only tool ran in plan mode (calls=%d)", writer.calls)
	}
	if out, failed := run1(t, eng, "notes", nil); failed || safe.calls != 1 {
		t.Fatalf("a PlanSafe tool was refused in plan mode: %s", out)
	}
}

// plan_mode told the model "Read-only operations only" and restricted
// nothing: the flag it set was read by nobody.
func TestModelEnteredPlanModeIsEnforced(t *testing.T) {
	eng := planEngine(t, tool.NewPlanModeTool(), tool.NewWriteTool(), tool.NewReadTool())
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return true }
	if _, failed := run1(t, eng, "plan_mode", map[string]any{"reason": "think first"}); failed {
		t.Fatal("entering plan mode failed")
	}
	target, _ := filepath.Abs("a.txt")
	if out, failed := run1(t, eng, "write", map[string]any{"filePath": target, "content": "x"}); !failed {
		t.Fatalf("write ran in the model's plan mode: %s", out)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the file was written in plan mode")
	}
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, failed := run1(t, eng, "read", map[string]any{"filePath": target}); failed {
		t.Fatalf("read was refused in plan mode: %s", out)
	}
}

// The model may leave the plan mode it entered (the user is asked); plan
// mode the user set is the user's to leave.
func TestExitPlanModeLeavesOnlyTheModelsOwnPlanMode(t *testing.T) {
	eng := planEngine(t, tool.NewPlanModeTool(), tool.NewExitPlanModeTool())
	asked := 0
	eng.PermissionPrompt = func(string, map[string]any, string) bool { asked++; return true }

	run1(t, eng, "plan_mode", nil)
	if out, failed := run1(t, eng, "exit_plan_mode", map[string]any{"summary": "plan ready"}); failed || asked != 1 {
		t.Fatalf("exit_plan_mode from the model's plan mode: failed=%v asked=%d %s", failed, asked, out)
	}
	if eng.effectiveMode() == permission.Plan {
		t.Fatal("still in plan mode after exit_plan_mode")
	}

	eng.SetPermissionMode(permission.Plan)
	out, failed := run1(t, eng, "exit_plan_mode", nil)
	if !failed || !strings.Contains(out, "/mode plan") {
		t.Fatalf("exit_plan_mode left the user's plan mode: failed=%v %s", failed, out)
	}
	if eng.PermissionMode() != permission.Plan {
		t.Fatal("the user's plan mode changed")
	}
}

// In auto mode build lines are pre-approved; in the model's plan mode they
// must not be, since a build writes.
func TestPlanModePreApprovesNoBuildLines(t *testing.T) {
	eng := planEngine(t, tool.NewPlanModeTool(), &mockTool{name: "bash", result: "built"})
	eng.classifier = permission.NewClassifier()
	eng.SetPermissionMode(permission.Auto)
	run1(t, eng, "plan_mode", nil)
	if out, failed := run1(t, eng, "bash", map[string]any{"command": "go build ./..."}); !failed {
		t.Fatalf("a build line ran in plan mode: %s", out)
	}
}

// Plan mode runs a read-only shell line, as the manual says and as
// planModeGate's shell branch intends. The engine pre-approved read-only
// lines in default mode only, so the shell tool saw "plan" in tctx and
// refused every line before the gate was reached: `git status` in plan mode
// was refused. Writes and builds stay refused.
func TestPlanModeRunsReadOnlyShellLines(t *testing.T) {
	eng := planEngine(t, tool.NewBashTool())
	eng.classifier = permission.NewClassifier()
	eng.SetPermissionMode(permission.Plan)
	out, failed := run1(t, eng, "bash", map[string]any{"command": "echo plan-ok"})
	if failed || !strings.Contains(out, "plan-ok") {
		t.Fatalf("read-only line refused in plan mode: failed=%v %s", failed, out)
	}
	for _, cmd := range []string{"go build ./...", "rm -rf build", "echo x > f.txt"} {
		if out, failed := run1(t, eng, "bash", map[string]any{"command": cmd}); !failed {
			t.Fatalf("%q ran in plan mode: %s", cmd, out)
		}
	}
}
