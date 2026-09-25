package dream

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/config"
)

// recordingProvider answers from a script and records every request.
type recordingProvider struct {
	mu     sync.Mutex
	script []*api.ChatResponse
	reqs   []api.ChatRequest
}

func (p *recordingProvider) Name() string        { return "rec" }
func (p *recordingProvider) DisplayName() string { return "rec" }
func (p *recordingProvider) Validate() error     { return nil }
func (p *recordingProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}
func (p *recordingProvider) Chat(_ context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.reqs)
	p.reqs = append(p.reqs, req)
	if n < len(p.script) {
		return p.script[n], nil
	}
	return &api.ChatResponse{Content: "done"}, nil
}

// The worker records the run's token usage and cost in dream-last.json, and
// Status carries it for /dream.
func TestWorkerRecordsUsageAndCost(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	writeDreamJSON(t, cfgDir, `{"enabled": true}`)
	touchSession(t, sessions, "s1")
	p := &recordingProvider{script: []*api.ChatResponse{
		{Content: "look", InputTokens: 1000, OutputTokens: 200, ToolCalls: []api.ToolCall{{ID: "1", Name: "glob", Input: map[string]any{"pattern": "*.none"}}}},
		{Content: "done", InputTokens: 500, OutputTokens: 100},
	}}
	if err := RunWorker(context.Background(), p, "deepseek-chat", sessions, ""); err != nil {
		t.Fatal(err)
	}
	lr, err := ReadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.InputTokens != 1500 || lr.OutputTokens != 300 {
		t.Fatalf("usage = %d/%d, want 1500/300", lr.InputTokens, lr.OutputTokens)
	}
	if lr.CostUSD <= 0 {
		t.Fatalf("cost = %v, want > 0", lr.CostUSD)
	}
	st := StatusFromDisk()
	if st.LastRunInputTokens != 1500 || st.LastRunOutputTokens != 300 || st.LastRunCostUSD != lr.CostUSD {
		t.Fatalf("status usage = %d/%d/%v", st.LastRunInputTokens, st.LastRunOutputTokens, st.LastRunCostUSD)
	}
}

// The worker consolidates the project's memory directory as well as the
// global one: both are named in the prompt and both are writable.
func TestWorkerConsolidatesProjectMemoryToo(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	writeDreamJSON(t, cfgDir, `{"enabled": true}`)
	touchSession(t, sessions, "s1")
	root := t.TempDir()
	projData, err := config.ProjectDataDir(root)
	if err != nil {
		t.Fatal(err)
	}
	projMem := filepath.Join(projData, "memory")
	outside := filepath.Join(t.TempDir(), "evil.md")
	p := &recordingProvider{script: []*api.ChatResponse{
		{Content: "write", ToolCalls: []api.ToolCall{
			{ID: "1", Name: "write", Input: map[string]any{"filePath": filepath.Join(projMem, "proj.md"), "content": "project fact"}},
			{ID: "2", Name: "write", Input: map[string]any{"filePath": filepath.Join(memoryDir(), "glob.md"), "content": "global fact"}},
			{ID: "3", Name: "write", Input: map[string]any{"filePath": outside, "content": "nope"}},
		}},
	}}
	if err := RunWorker(context.Background(), p, "m", sessions, root); err != nil {
		t.Fatal(err)
	}
	prompt := p.reqs[0].Messages[0].Content
	if !strings.Contains(prompt, memoryDir()) || !strings.Contains(prompt, projMem) {
		t.Fatalf("prompt should list both memory directories:\n%s", prompt)
	}
	for _, f := range []string{filepath.Join(projMem, "proj.md"), filepath.Join(memoryDir(), "glob.md")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Error("write outside both memory directories was allowed")
	}
}

func TestRunnerProjectRootSetsProjectMemory(t *testing.T) {
	workerTestEnv(t)
	root := t.TempDir()
	r := &Runner{memoryRoot: memoryDir()}
	r.SetProjectRoot(root)
	dirs := r.memoryRoots()
	if len(dirs) != 2 || !strings.HasSuffix(dirs[1], filepath.Join(config.ProjectKey(root), "memory")) {
		t.Fatalf("memoryRoots = %v", dirs)
	}
	r.SetProjectRoot("")
	if len(r.memoryRoots()) != 1 {
		t.Fatal("empty root should leave only the global directory")
	}
}

// panicAfterProvider reports usage on its first call and panics on the next.
type panicAfterProvider struct{ recordingProvider }

func (p *panicAfterProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	p.mu.Lock()
	n := len(p.reqs)
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	if n == 0 {
		return &api.ChatResponse{Content: "look", InputTokens: 700, OutputTokens: 70,
			ToolCalls: []api.ToolCall{{ID: "1", Name: "glob", Input: map[string]any{"pattern": "*.none"}}}}, nil
	}
	panic("provider exploded")
}

// A run that panics still records the usage it had accumulated.
func TestWorkerPanicRecordsUsage(t *testing.T) {
	cfgDir, sessions := workerTestEnv(t)
	writeDreamJSON(t, cfgDir, `{"enabled": true}`)
	touchSession(t, sessions, "s1")
	_ = RunWorker(context.Background(), &panicAfterProvider{}, "m", sessions, "")
	lr, err := ReadLastRun()
	if err != nil {
		t.Fatal(err)
	}
	if lr.Result != ResultFailed || lr.InputTokens != 700 || lr.OutputTokens != 70 {
		t.Fatalf("last run = %+v", lr)
	}
}
