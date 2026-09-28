package engine

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/permission"
)

// A warning-level finding (a forced push) is shown when the call runs; it
// used to be computed and dropped, so bypass ran it without a word.
func TestSafetyWarningIsShownWhenTheCallRuns(t *testing.T) {
	isolatedHome(t)
	t.Chdir(t.TempDir())
	eng := newTestEngine(&mockProvider{}, &mockTool{name: "bash", result: "pushed"})
	eng.SetPermissionMode(permission.Bypass)
	var lines []string
	eng.SetOutput(LineSink(func(s string) { lines = append(lines, s) }))
	if out, failed := eng.executeTool(t.Context(), api.ToolCall{ID: "p", Name: "bash", Input: map[string]any{"command": "git push --force origin main"}}); failed {
		t.Fatalf("a warning blocked the call: %s", out)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "potentially dangerous command") {
		t.Fatalf("no warning shown for a forced push: %q", lines)
	}
}
