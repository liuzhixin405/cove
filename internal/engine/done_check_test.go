package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// namedProvider is a seqProvider reporting another provider name.
type namedProvider struct {
	*seqProvider
	name string
}

func (p namedProvider) Name() string { return p.name }

// doneCheckRun runs one turn: a write (or read) tool call, then long final
// answers that trigger no other nudge. It returns the model calls made and
// how many done-check prompts the history holds.
func doneCheckRun(t *testing.T, providerName, model, mode string, write bool) (calls, checks int) {
	t.Helper()
	toolName := "read_tool"
	if write {
		toolName = "write"
	}
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("c0", toolName, map[string]any{"filePath": "a.go", "content": "x"}), nil
		}
		return &api.ChatResponse{Content: "已修改 a.go 并检查了调用方，全部工作已经完成，没有遗留事项需要处理。"}, nil
	}}
	var p api.Provider = prov
	if providerName != "" {
		p = namedProvider{prov, providerName}
	}
	eng := newPatternEngine(t, p, func(c *Config) {
		c.Model = model
		c.DoneCheck = mode
		c.LoopDetectionDisabled = true
	},
		&mockTool{name: "write", result: "written"},
		&mockTool{name: "read_tool", readOnly: true, safe: true, result: "ok"})
	if _, err := run(t, eng, "改 a.go"); err != nil {
		t.Fatal(err)
	}
	return len(prov.requests()), countMsgs(eng.messages, nudgeDoneCheckText)
}

func TestDoneCheckAutoFastModelAsksOnce(t *testing.T) {
	calls, checks := doneCheckRun(t, "anthropic", "claude-haiku-4-5", "auto", true)
	if checks != 1 || calls != 3 {
		t.Fatalf("calls=%d checks=%d, want one done check (3 calls)", calls, checks)
	}
}

func TestDoneCheckAutoNonAnthropicProviderAsks(t *testing.T) {
	calls, checks := doneCheckRun(t, "", "deepseek-v4-pro", "", true)
	if checks != 1 || calls != 3 {
		t.Fatalf("calls=%d checks=%d, want one done check (3 calls)", calls, checks)
	}
}

func TestDoneCheckAutoAnthropicTopModelSkips(t *testing.T) {
	calls, checks := doneCheckRun(t, "anthropic", "claude-opus-4-1", "auto", true)
	if checks != 0 || calls != 2 {
		t.Fatalf("calls=%d checks=%d, want no done check", calls, checks)
	}
}

func TestDoneCheckOnAnthropicTopModelAsks(t *testing.T) {
	calls, checks := doneCheckRun(t, "anthropic", "claude-opus-4-1", "on", true)
	if checks != 1 || calls != 3 {
		t.Fatalf("calls=%d checks=%d, want one done check", calls, checks)
	}
}

func TestDoneCheckOffSkips(t *testing.T) {
	calls, checks := doneCheckRun(t, "", "deepseek-v4-flash", "off", true)
	if checks != 0 || calls != 2 {
		t.Fatalf("calls=%d checks=%d, want no done check", calls, checks)
	}
}

func TestDoneCheckSkipsWhenNoFileChanged(t *testing.T) {
	calls, checks := doneCheckRun(t, "", "deepseek-v4-flash", "on", false)
	if checks != 0 || calls != 2 {
		t.Fatalf("calls=%d checks=%d, want no done check without a file change", calls, checks)
	}
}
