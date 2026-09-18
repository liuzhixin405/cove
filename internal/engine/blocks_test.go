package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/render"
	"github.com/liuzhixin405/cove/internal/uiout"
)

func TestToolCallReachesTheSinkAsAnExpandableBlock(t *testing.T) {
	// The whole point of emitting a struct rather than a formatted line: the
	// full output travels with the summary, so the front end can collapse it
	// and still show it on demand. A line cannot carry that.
	big := strings.Repeat("output line\n", 200)
	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "1", Name: "bash", Input: map[string]any{"command": "go test ./..."}}}},
			{content: "done"},
		},
	}
	tl := &mockTool{name: "bash", readOnly: true, safe: true, result: big}
	eng := newTestEngine(prov, tl)

	cap := uiout.NewCapture()
	eng.SetOutput(cap)

	if _, err := eng.RunMessageWithStream(context.Background(),
		api.Message{Role: "user", Content: "run the tests"}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	var got *render.Block
	for _, b := range cap.Blocks() {
		if b.Kind == render.KindTool && b.Tool == "bash" {
			cp := b
			got = &cp
		}
	}
	if got == nil {
		t.Fatalf("no tool block reached the sink; blocks=%+v lines=%q", cap.Blocks(), cap.Lines())
	}
	if !strings.Contains(got.Header, "go test ./...") {
		t.Errorf("header does not name the command that ran: %q", got.Header)
	}
	if got.Summary == "" {
		t.Error("block has no summary, so the collapsed form would show nothing")
	}
	if !got.Expandable() {
		t.Errorf("a 200-line result is not expandable, so the output is unreachable: %+v", *got)
	}
	if !strings.Contains(got.Full, "output line") {
		t.Error("the full output was not carried on the block")
	}
	if got.IsError {
		t.Error("a successful call was marked as an error")
	}
}

func TestFailedToolCallIsAlwaysExpandable(t *testing.T) {
	// Why a call failed is exactly what the user needs next, whatever the
	// tool's collapse policy says.
	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "1", Name: "read", Input: map[string]any{"filePath": "missing.go"}}}},
			{content: "done"},
		},
	}
	// read is policySummaryOnly, so only the error rule can make this
	// expandable.
	tl := &mockTool{name: "read", readOnly: true, safe: true, result: "Error: file not found: missing.go\n  at some/path"}
	eng := newTestEngine(prov, tl)

	cap := uiout.NewCapture()
	eng.SetOutput(cap)
	if _, err := eng.RunMessageWithStream(context.Background(),
		api.Message{Role: "user", Content: "read it"}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	for _, b := range cap.Blocks() {
		if b.Kind != render.KindTool || b.Tool != "read" {
			continue
		}
		if !b.IsError {
			t.Fatalf("a failing call was not marked as an error: %+v", b)
		}
		if !b.Expandable() {
			t.Fatalf("a failing call is not expandable, so the reason is unreachable: %+v", b)
		}
		// Success and failure must be distinguishable without colour: the old
		// formatToolLine printed a literal "?" for both.
		collapsed := render.Collapsed(b, 80, render.Styles{})
		if !strings.Contains(collapsed, "✗") {
			t.Fatalf("the collapsed failure carries no failure glyph:\n%s", collapsed)
		}
		return
	}
	t.Fatal("no read block reached the sink")
}

func TestRunningToolIsActivityNotHistory(t *testing.T) {
	// A "running X…" notice must not accumulate one dead row per tool call.
	prov := &mockProvider{
		responses: []mockResponse{
			{toolCalls: []api.ToolCall{{ID: "1", Name: "bash", Input: map[string]any{"command": "ls"}}}},
			{content: "done"},
		},
	}
	eng := newTestEngine(prov, &mockTool{name: "bash", readOnly: true, safe: true, result: "a\nb"})

	cap := uiout.NewCapture()
	eng.SetOutput(cap)
	if _, err := eng.RunMessageWithStream(context.Background(),
		api.Message{Role: "user", Content: "ls"}, nil, nil); err != nil {
		t.Fatalf("turn failed: %v", err)
	}

	for _, l := range cap.Lines() {
		if strings.Contains(l, "执行 bash") {
			t.Fatalf("the transient notice was emitted as a history line: %q", l)
		}
	}
	// And it must be cleared afterwards, or the spinner text outlives the tool.
	acts := cap.Activities()
	if len(acts) == 0 {
		t.Fatal("no activity was reported at all")
	}
	if acts[len(acts)-1] != "" {
		t.Fatalf("activity was left set to %q after the turn", acts[len(acts)-1])
	}
}

func TestUnwiredEngineIsSilentButCallbackStillWorks(t *testing.T) {
	// out must default to nil, not to uiout.Discard: defaulting to Discard
	// makes the sink branch always win and silently swallows every diagnostic
	// on a front end that uses the callback.
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "hi"}}})
	if eng.out != nil {
		t.Fatalf("a fresh engine has a sink installed (%T); the callback path is dead", eng.out)
	}

	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }
	eng.emitBlock(render.ToolBlock("1", "bash", "ls", "", "a\nb", false, 0))
	if len(lines) != 1 {
		t.Fatalf("the callback received %d lines, want 1", len(lines))
	}
	if !strings.Contains(lines[0], "bash") || !strings.Contains(lines[0], "ls") {
		t.Fatalf("the rendered line lost the tool or its target: %q", lines[0])
	}
	// The callback cannot expand anything, so advertising "/x 1" would point
	// at a command that front end does not have.
	if strings.Contains(lines[0], "/x") {
		t.Fatalf("the legacy line advertises an expand hint it cannot honour: %q", lines[0])
	}
}

func TestBlockIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := nextBlockID()
		if seen[id] {
			t.Fatalf("duplicate block id %q; /x would show the wrong output", id)
		}
		seen[id] = true
	}
}

func TestToolHeaderPicksTheRightArgument(t *testing.T) {
	cases := []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"bash", map[string]any{"command": "go test ./...", "timeout": 30}, "go test ./..."},
		{"powershell", map[string]any{"command": "Get-ChildItem"}, "Get-ChildItem"},
		{"read", map[string]any{"filePath": `D:\a\b.go`}, `D:\a\b.go`},
		{"write", map[string]any{"file_path": "/tmp/x"}, "/tmp/x"},
		{"grep", map[string]any{"pattern": "TODO", "path": "internal"}, "TODO in internal"},
		{"glob", map[string]any{"pattern": "**/*.go"}, "**/*.go"},
		{"webfetch", map[string]any{"url": "https://example.com"}, "https://example.com"},
		{"websearch", map[string]any{"query": "bubbletea v2"}, "bubbletea v2"},
		{"agent", map[string]any{"description": "audit the parser", "prompt": "long…"}, "audit the parser"},
		// Unknown tool: fall back to the conventional names rather than
		// guessing from an unordered map.
		{"some_mcp_tool", map[string]any{"path": "a/b", "extra": "zzz"}, "a/b"},
		{"todowrite", map[string]any{"todos": "many"}, ""},
		{"read", nil, ""},
	}
	for _, c := range cases {
		if got := toolHeaderFor(c.tool, c.input); got != c.want {
			t.Errorf("%s: header is %q, want %q", c.tool, got, c.want)
		}
	}
}

func TestToolHeaderIsBounded(t *testing.T) {
	// A base64 blob or a 4KB heredoc must not travel through the pipeline just
	// to be thrown away at the last step.
	long := strings.Repeat("很长的命令", 500)
	got := toolHeaderFor("bash", map[string]any{"command": long})
	if n := len([]rune(got)); n > maxToolHeaderRunes {
		t.Fatalf("header kept %d runes, cap is %d", n, maxToolHeaderRunes)
	}
}
