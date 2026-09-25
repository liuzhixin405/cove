package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/checkpoint"
)

// A turn that wrote a file under a fresh checkpoint says so in the summary
// line: the checkpoint was only visible in debug output.
func TestSummaryMentionsCheckpointAfterWrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	isolateHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{{ID: "w1", Name: "write", Input: map[string]any{"file_path": "a.txt"}}}},
		{content: "done"},
		{content: "plain answer"},
	}}
	eng := newTestEngine(prov, &fileWriteTool{dir: dir})
	cp, err := checkpoint.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	eng.cpMgr = cp
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record

	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "edit"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	got := rec.all()
	if len(got) != 1 || !strings.Contains(strings.Join(got[0].Extra, "\n"), "/undo") {
		t.Fatalf("summaries = %+v, want the checkpoint hint", got)
	}

	// A turn without writes has no hint (and nothing else to say).
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "question"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("second turn produced a summary: %+v", got[1:])
	}
}
