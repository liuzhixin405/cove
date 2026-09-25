package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/session"
)

// seedSessions saves n sessions in eng's store (the isolated home's).
func seedSessions(t *testing.T, st *session.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("old-%d", i)
		if err := st.Save(&session.Record{ID: id, Model: "m", Messages: []api.Message{{Role: "user", Content: id}}}); err != nil {
			t.Fatal(err)
		}
	}
}

// Final fix (Important 2a): the turn-end summary says how many old sessions
// max_sessions pruning deleted, so the deletion is not silent.
func TestBackgroundSummaryReportsPrunedSessions(t *testing.T) {
	isolateHome(t)
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.store = mustSessionStore(t)
	seedSessions(t, eng.store, 5)
	rec := &summaryRecorder{}
	eng.OnBackgroundSummary = rec.record
	resetPruneWarning(t, func(string, ...any) {})

	eng.bg.Add(1)
	eng.runBackgroundWork(backgroundJob{saved: true, sessionID: "current", keep: 2})

	got := rec.all()
	if len(got) != 1 || got[0].SessionsPruned != 3 || got[0].MaxSessions != 2 {
		t.Fatalf("summaries = %+v, want one with SessionsPruned=3, MaxSessions=2", got)
	}
	if !got[0].Notable() {
		t.Fatal("a summary with pruned sessions is not notable")
	}
}

// Final fix (Important 2b): the first deletion in a process is logged as a
// warning naming the count and the config key; later ones are not repeated.
func TestFirstPruneWarnsOnce(t *testing.T) {
	isolateHome(t)
	eng := newPatternEngine(t, &seqProvider{}, nil)
	eng.store = mustSessionStore(t)
	seedSessions(t, eng.store, 4)
	var warns []string
	resetPruneWarning(t, func(format string, args ...any) { warns = append(warns, fmt.Sprintf(format, args...)) })

	eng.bg.Add(1)
	eng.runBackgroundWork(backgroundJob{saved: true, sessionID: "current", keep: 1})
	if len(warns) != 1 || !strings.Contains(warns[0], "3") || !strings.Contains(warns[0], "max_sessions") {
		t.Fatalf("warnings = %q, want one naming 3 sessions and max_sessions", warns)
	}

	// A second store (throttle not yet armed) prunes again: no second warning.
	eng.store = mustSessionStore(t)
	seedSessions(t, eng.store, 4)
	eng.bg.Add(1)
	eng.runBackgroundWork(backgroundJob{saved: true, sessionID: "current", keep: 1})
	if len(warns) != 1 {
		t.Fatalf("warned %d times, want once per process: %q", len(warns), warns)
	}
}

func resetPruneWarning(t *testing.T, fn func(string, ...any)) {
	t.Helper()
	old := pruneWarnf
	pruneWarnf = fn
	pruneWarned.Store(false)
	t.Cleanup(func() { pruneWarnf = old; pruneWarned.Store(false) })
}
