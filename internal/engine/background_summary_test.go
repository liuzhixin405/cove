package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/extract"
	"github.com/liuzhixin405/cove/internal/session"
)

// slowExtractProvider answers the extraction request after a delay with one
// memory, and records when it finished.
type slowExtractProvider struct {
	delay    time.Duration
	finished atomic.Bool
}

func (p *slowExtractProvider) Name() string        { return "x" }
func (p *slowExtractProvider) DisplayName() string { return "x" }
func (p *slowExtractProvider) Validate() error     { return nil }
func (p *slowExtractProvider) Chat(ctx context.Context, _ api.ChatRequest) (*api.ChatResponse, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.finished.Store(true)
	return &api.ChatResponse{Content: "---MEMORY---\nFILE: fact.md\nMODE: write\nCONTENT:\n项目使用 Go 1.25\n---END---"}, nil
}
func (p *slowExtractProvider) ChatStream(ctx context.Context, req api.ChatRequest, _ api.StreamHandler) (*api.ChatResponse, error) {
	return p.Chat(ctx, req)
}

type summaryRecorder struct {
	mu  sync.Mutex
	got []BackgroundSummary
}

func (r *summaryRecorder) record(s BackgroundSummary) {
	r.mu.Lock()
	r.got = append(r.got, s)
	r.mu.Unlock()
}

func (r *summaryRecorder) all() []BackgroundSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]BackgroundSummary(nil), r.got...)
}

func extractingEngine(t *testing.T, delay time.Duration) (*Engine, *slowExtractProvider) {
	t.Helper()
	isolateHome(t)
	eng := newPatternEngine(t, &seqProvider{}, nil)
	xp := &slowExtractProvider{delay: delay}
	eng.setExtractRunner(extract.NewRunner(xp, "m"))
	// Extraction needs a few messages of history.
	eng.LoadMessages([]api.Message{
		{Role: "user", Content: "我们用 Go"}, {Role: "assistant", Content: "好的"},
	})
	return eng, xp
}

// cove -p waits for the memory extraction started at the end of its turn
// (the process used to exit first and kill it).
func TestWaitBackgroundWaitsForExtraction(t *testing.T) {
	eng, xp := extractingEngine(t, 150*time.Millisecond)
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	eng.WaitBackground(ctx)
	if !xp.finished.Load() {
		t.Fatal("WaitBackground returned before the extraction finished")
	}
}

func TestWaitBackgroundHonoursContext(t *testing.T) {
	eng, _ := extractingEngine(t, 3*time.Second)
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	eng.WaitBackground(ctx)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("WaitBackground ignored its context: returned after %v", el)
	}
}

func TestBackgroundSummaryReportsExtractedMemories(t *testing.T) {
	eng, _ := extractingEngine(t, 10*time.Millisecond)
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	if _, err := run(t, eng, "记住这个"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	got := rec.all()
	if len(got) != 1 || got[0].MemoriesExtracted != 1 || !got[0].SessionSaved {
		t.Fatalf("summaries = %+v, want one with MemoriesExtracted=1, SessionSaved", got)
	}
}

// A turn where nothing happened in the background says nothing.
func TestBackgroundSummarySilentWhenNothingHappened(t *testing.T) {
	isolateHome(t)
	eng := newPatternEngine(t, &seqProvider{}, nil)
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	if _, err := run(t, eng, "你好"); err != nil {
		t.Fatal(err)
	}
	eng.WaitBackground(context.Background())
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("summaries = %+v, want none", got)
	}
}

func TestBackgroundSummaryReportThreshold(t *testing.T) {
	cases := []struct {
		s    BackgroundSummary
		want bool
	}{
		{BackgroundSummary{SessionSaved: true}, false},
		{BackgroundSummary{SessionSaved: true, MemoriesExtracted: 2}, true},
		{BackgroundSummary{SessionSaved: true, DreamChanged: true}, true},
		{BackgroundSummary{SessionSaved: false}, true},
	}
	for _, c := range cases {
		if got := c.s.Notable(); got != c.want {
			t.Fatalf("%+v.Notable() = %v, want %v", c.s, got, c.want)
		}
	}
}

// The dream gate's "sessions since the last consolidation" must not count the
// session in use; after a resume that is the resumed one.
func TestResumeSessionMovesDreamCurrentSession(t *testing.T) {
	home := isolateHome(t)
	dir := filepath.Join(home, ".cove", "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-old.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.store = mustSessionStore(t)
	eng.dreamRunner = dream.NewRunner(nil, "m", "session-new")
	if n := eng.DreamStatus().SessionsSinceLast; n != 1 {
		t.Fatalf("before resume: SessionsSinceLast = %d, want 1", n)
	}
	eng.ResumeSession(&session.Record{ID: "session-old", Cwd: home})
	if n := eng.DreamStatus().SessionsSinceLast; n != 0 {
		t.Fatalf("after resume: SessionsSinceLast = %d, want 0 (the resumed session is in use)", n)
	}
}

// The first turn only records the dream gate; it is reported when it moves
// afterwards, or when a consolidation fires.
func TestDreamGateMovedNeedsAChange(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, nil)
	st := func(since int) dream.Status {
		return dream.Status{Enabled: true, MinSessions: 3, SessionsSinceLast: since, HoursSinceLast: -1}
	}
	if eng.dreamGateMoved(st(1), false) {
		t.Fatal("the first observation is a baseline, not a change")
	}
	if eng.dreamGateMoved(st(1), false) {
		t.Fatal("unchanged gate reported")
	}
	if !eng.dreamGateMoved(st(2), false) {
		t.Fatal("a session closer to the gate must be reported")
	}
	if !eng.dreamGateMoved(st(2), true) {
		t.Fatal("a consolidation that fired must be reported")
	}
}
