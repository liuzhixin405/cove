package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove/internal/tool"
)

// GenerateOnce is one tool-less call that stays out of the session history.
func TestGenerateOnce(t *testing.T) {
	prov := &mockProvider{responses: []mockResponse{{content: "# CLAUDE.md draft", inputTokens: 1000}}}
	eng := newTestEngine(prov)
	before := len(eng.messages)
	out, err := eng.GenerateOnce(context.Background(), "sys", "draft it")
	if err != nil || out != "# CLAUDE.md draft" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if len(eng.messages) != before {
		t.Fatal("GenerateOnce changed the session history")
	}
	req := prov.lastReq
	if len(req.Tools) != 0 || req.MaxTokens != generateOnceMaxTokens || req.SystemBase != "sys" {
		t.Fatalf("request = tools %d, max %d, system %q", len(req.Tools), req.MaxTokens, req.SystemBase)
	}
	if eng.costTracker.Totals().Input == 0 {
		t.Fatal("the call was not billed")
	}

	eng.SetMaxBudget(0.0000001)
	eng.costTracker.AddDetailed("test-model", 1000000, 0, 0, 0)
	if _, err := eng.GenerateOnce(context.Background(), "sys", "again"); err == nil {
		t.Fatal("GenerateOnce ran over budget")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng.SetMaxBudget(0)
	if _, err := eng.GenerateOnce(ctx, "sys", "again"); err == nil {
		t.Fatal("GenerateOnce ran with a cancelled context")
	}
}

// OnTurnModel reports the routed model only when routing chooses between a
// fast and a main model.
func TestOnTurnModel(t *testing.T) {
	var got []string
	eng := newPatternEngine(t, &seqProvider{}, func(c *Config) { c.ModelFast = "test-fast" })
	eng.OnTurnModel = func(m string) { got = append(got, m) }
	if _, err := run(t, eng, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] == "" {
		t.Fatalf("OnTurnModel calls = %q", got)
	}

	var none []string
	eng2 := newPatternEngine(t, &seqProvider{}, nil)
	eng2.OnTurnModel = func(m string) { none = append(none, m) }
	if _, err := run(t, eng2, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("single-model engine reported %q", none)
	}
}

// A tool whose description follows an outside version (the MCP proxy lists
// the pool's tools) is re-read when that version moves, although the
// registry itself did not change.
type versionedTool struct {
	mockTool
	desc *string
}

func (t *versionedTool) Def() tool.Def {
	d := t.mockTool.Def()
	d.Description = *t.desc
	return d
}

func TestToolDefsCacheFollowsExtraVersion(t *testing.T) {
	desc := "mcp: no servers"
	eng := newTestEngine(&mockProvider{}, &versionedTool{mockTool: mockTool{name: "mcp", readOnly: true}, desc: &desc})
	version := 1
	eng.SetToolDefsVersion(func() int { return version })
	find := func() string {
		for _, d := range eng.buildAPIToolDefs() {
			if d.Name == "mcp" {
				return d.Description
			}
		}
		return ""
	}
	if got := find(); got != desc {
		t.Fatalf("first build: %q", got)
	}
	desc = "mcp: github/create_issue"
	if got := find(); got != "mcp: no servers" {
		t.Fatalf("cache dropped without a version change: %q", got)
	}
	version++
	if got := find(); got != "mcp: github/create_issue" {
		t.Fatalf("after the version moved: %q", got)
	}
}
