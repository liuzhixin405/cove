package tool

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

type stubAgentRunner struct{ res *api.AgentRunResult }

func (s *stubAgentRunner) Run(context.Context, string, string) (*api.AgentRunResult, error) {
	return s.res, nil
}
func (s *stubAgentRunner) Register(string, string, string) {}

func agentFirstLine(t *testing.T, res *api.AgentRunResult) string {
	t.Helper()
	out, err := NewAgentTool().Call(context.Background(), Input{"type": "general", "prompt": "x"},
		Context{Runtime: &Runtime{AgentRunner: &stubAgentRunner{res: res}}})
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitN(out.Data, "\n", 2)[0]
}

func TestAgentToolFirstLineReportsExitReason(t *testing.T) {
	cases := []struct {
		res  *api.AgentRunResult
		want string
	}{
		{&api.AgentRunResult{Output: "ok", Steps: 3, Success: true, ExitReason: "completed"},
			"[exit: completed, steps: 3, truncated: no]"},
		{&api.AgentRunResult{Output: "partial", Steps: 60, ExitReason: "max_iterations", Truncated: true},
			"[exit: max_iterations, steps: 60, truncated: yes]"},
		{&api.AgentRunResult{Steps: 2, ExitReason: "loop"},
			"[exit: loop, steps: 2, truncated: no]"},
		// A runner that does not fill ExitReason still gets a first line.
		{&api.AgentRunResult{Output: "ok", Steps: 1, Success: true}, "[exit: completed, steps: 1, truncated: no]"},
		{&api.AgentRunResult{Steps: 1, Error: "boom"}, "[exit: error, steps: 1, truncated: no]"},
	}
	for _, c := range cases {
		if got := agentFirstLine(t, c.res); got != c.want {
			t.Errorf("first line = %q, want %q", got, c.want)
		}
	}
}
