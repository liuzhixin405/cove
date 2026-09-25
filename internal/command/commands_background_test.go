package command

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/memory"
)

// backgroundHome redirects the home directory, so dream's lock and session
// scan and the memory store all work on temp dirs.
func backgroundHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", "")
	t.Chdir(t.TempDir()) // no CLAUDE.md from the repository in /memory list
	return home
}

func writeSessions(t *testing.T, home string, ids ...string) {
	t.Helper()
	dir := filepath.Join(home, ".cove", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// doneProvider answers every dream turn with no tool calls, which ends the run.
type doneProvider struct{}

func (doneProvider) Name() string        { return "fake" }
func (doneProvider) DisplayName() string { return "Fake" }
func (doneProvider) Validate() error     { return nil }
func (doneProvider) Chat(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
	return &api.ChatResponse{Content: "done"}, nil
}
func (doneProvider) ChatStream(context.Context, api.ChatRequest, api.StreamHandler) (*api.ChatResponse, error) {
	return nil, fmt.Errorf("not used")
}

func TestDreamStatusShowsGates(t *testing.T) {
	home := backgroundHome(t)
	// The gates are the threshold trigger's (session_end is the default).
	writeSessions(t, home, "a", "cur")
	if err := os.WriteFile(filepath.Join(home, ".cove", "dream.json"), []byte(`{"trigger": "threshold"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dream.NewRunner(doneProvider{}, "m", "cur")

	out, err := NewDreamCmd().Execute(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"距上次整理", "从未整理", "还差 2 个会话", "12 小时"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("/dream output lacks %q:\n%s", want, out.Message)
		}
	}
}

func TestDreamRunStartsImmediatelyThenReportsLock(t *testing.T) {
	home := backgroundHome(t)
	writeSessions(t, home, "a")
	dream.NewRunner(doneProvider{}, "m", "")

	out, err := NewDreamCmd().Execute(context.Background(), Input{Args: []string{"run"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "已开始") {
		t.Fatalf("/dream run output = %q", out.Message)
	}
	// The run (or its fresh lock stamp) now holds the lock.
	out, err = NewDreamCmd().Execute(context.Background(), Input{Args: []string{"run"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "锁") {
		t.Fatalf("second /dream run output = %q, want a lock notice", out.Message)
	}
	// Let the background run finish before the temp home is removed.
	deadline := time.Now().Add(10 * time.Second)
	for dream.ActiveTask() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDreamRejectsUnknownSubcommand(t *testing.T) {
	backgroundHome(t)
	out, _ := NewDreamCmd().Execute(context.Background(), Input{Args: []string{"bogus"}})
	if !strings.Contains(out.Message, "用法") {
		t.Fatalf("output = %q, want usage", out.Message)
	}
}

func TestMemoryListCountMatchesStats(t *testing.T) {
	home := backgroundHome(t)
	dir := filepath.Join(home, ".cove", "memory")
	store := memory.NewStoreForDirs(dir)
	for name, body := range map[string]string{
		"a.md": "# Build\nuse go build ./...",
		"b.md": "first line only",
		"c.md": "\n\n  leading blank lines\nsecond",
	} {
		if err := store.Save(name, body); err != nil {
			t.Fatal(err)
		}
	}
	store.RecordExtraction(2) // hidden record: not an entry

	out, err := NewMemoryCmd().Execute(context.Background(), Input{Args: []string{"list"}, MemoryStore: store})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("共 %d 条", store.Stats().FileCount)
	if !strings.Contains(out.Message, want) {
		t.Fatalf("/memory list lacks %q:\n%s", want, out.Message)
	}
	if store.Stats().FileCount != 3 {
		t.Fatalf("FileCount = %d, want 3", store.Stats().FileCount)
	}
	for _, want := range []string{"# Build", "first line only", "leading blank lines"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("/memory list lacks the summary %q:\n%s", want, out.Message)
		}
	}
}

func TestMemoryStatsShowsLastExtraction(t *testing.T) {
	home := backgroundHome(t)
	store := memory.NewStoreForDirs(filepath.Join(home, ".cove", "memory"))

	out, _ := NewMemoryCmd().Execute(context.Background(), Input{Args: []string{"stats"}, MemoryStore: store})
	if !strings.Contains(out.Message, "上次提取") || !strings.Contains(out.Message, "尚无记录") {
		t.Fatalf("/memory stats without a record:\n%s", out.Message)
	}
	store.RecordExtraction(2)
	out, _ = NewMemoryCmd().Execute(context.Background(), Input{Args: []string{"stats"}, MemoryStore: store})
	if !strings.Contains(out.Message, "保存 2 条") {
		t.Fatalf("/memory stats after an extraction:\n%s", out.Message)
	}
}
