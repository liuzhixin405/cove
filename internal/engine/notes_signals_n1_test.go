package engine

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/notes"
)

func notesEngine(t *testing.T) *Engine {
	t.Helper()
	isolateHome(t)
	e := &Engine{sessionNotes: notes.New(t.TempDir())}
	return e
}

// Chinese decisions and findings are recorded (the patterns were English only).
func TestRecordSignalsChinese(t *testing.T) {
	e := notesEngine(t)
	e.recordSignals("这个项目改用 pnpm 管理依赖。", "我发现 go.sum 里缺少 x/net 的条目。")
	e.recordSignals("决定采用 SQLite 作为本地缓存", "原因是 CGO 在 Windows 上没有启用")
	c := e.sessionNotes.Content()
	for _, want := range []string{"pnpm 管理依赖", "go.sum 里缺少 x/net 的条目", "SQLite 作为本地缓存", "CGO 在 Windows 上没有启用"} {
		if !strings.Contains(c, want) {
			t.Errorf("notes lack %q:\n%s", want, c)
		}
	}
	// A plain sentence that merely contains 用 is not a decision.
	e2 := notesEngine(t)
	e2.recordSignals("这个函数没有用，删掉吧", "")
	if c := e2.sessionNotes.Content(); c != "" {
		t.Errorf("non-decision recorded:\n%s", c)
	}
}

// Writes and tool errors are not notes any more: "File: x" and error lines
// filled the notes and crowded out the decisions.
func TestNoFileOrErrorNotes(t *testing.T) {
	isolateHome(t)
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "1", Name: "bad", Input: map[string]any{}}}},
		{content: "ok"},
	}}
	eng := newTestEngine(prov, &mockTool{name: "bad", readOnly: true, err: errString("boom")})
	eng.sessionNotes = notes.New(t.TempDir())
	eng.trackFileChanges(api.ToolCall{Name: "write", Input: map[string]any{"filePath": "a.go"}})
	if _, err := run(t, eng, "go"); err != nil {
		t.Fatal(err)
	}
	c := eng.sessionNotes.Content()
	if strings.Contains(c, "File:") || strings.Contains(c, "boom") {
		t.Fatalf("notes carry file/error entries:\n%s", c)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Sentences that only look like decisions or findings are not recorded.
func TestRecordSignalsRejectsLookalikes(t *testing.T) {
	cases := []struct{ user, assistant string }{
		{"用户登录很慢怎么办", ""},
		{"用例都跑过了吗", ""},
		{"由你决定怎么做", ""},
		{"我不决定这个", ""},
		{"", "没有发现问题"},
		{"", "未发现异常；继续"},
		{"use it.", ""},
		{"", "我发现 ok"},
	}
	for _, c := range cases {
		e := notesEngine(t)
		e.recordSignals(c.user, c.assistant)
		if got := e.sessionNotes.Content(); got != "" {
			t.Errorf("%q / %q recorded:\n%s", c.user, c.assistant, got)
		}
	}
	e := notesEngine(t)
	e.recordSignals("我们决定采用 JSONL 存储", "")
	if !strings.Contains(e.sessionNotes.Content(), "JSONL 存储") {
		t.Fatal("a real decision was not recorded")
	}
	e2 := notesEngine(t)
	e2.recordSignals("", "原因是 缓存键没有带版本；所以重建")
	if c := e2.sessionNotes.Content(); !strings.Contains(c, "缓存键没有带版本") || strings.Contains(c, "所以重建") {
		t.Fatalf("finding not cut at ；:\n%s", c)
	}
}
