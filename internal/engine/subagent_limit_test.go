package engine

import (
	"context"
	"strings"
	"testing"
)

// config "subagent_max_iterations" reaches the sub-agents the agent tool
// spawns, and a capped sub-agent hands back what it did.
func TestSubagentMaxIterationsFromConfig(t *testing.T) {
	prov := alwaysToolProvider(0)
	eng := newPatternEngine(t, prov, func(c *Config) { c.SubagentMaxIterations = 2 },
		&mockTool{name: "read_tool", readOnly: true, safe: true, result: "ok"})
	eng.WirePlanExecutor()
	runner, ok := eng.runtime.AgentRunner.(*agentRunner)
	if !ok {
		t.Fatalf("AgentRunner = %T", eng.runtime.AgentRunner)
	}
	res, err := runner.Run(context.Background(), "explore", "看看")
	if err != nil {
		t.Fatal(err)
	}
	if res.Steps != 2 || len(prov.requests()) != 2 {
		t.Fatalf("steps = %d, model calls = %d, want 2", res.Steps, len(prov.requests()))
	}
	if res.Success || !strings.Contains(res.Output, "已达上限") || !strings.Contains(res.Output, "已完成 2 个步骤") {
		t.Fatalf("capped sub-agent output = %q", res.Output)
	}
}
