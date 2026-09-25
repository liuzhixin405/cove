package engine

import (
	"context"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	ctxt "github.com/liuzhixin405/cove/internal/context"
)

// A turn only needs fresh git state for its <environment> note; the file tree
// and repo map sit in the system prompt, which is not rebuilt per turn. The
// full rescan (file tree walk + repo map) used to run before every message.
func TestPerTurnRefreshesGitWithoutRescanningTheProject(t *testing.T) {
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "one"}, {content: "two"}}})
	collects, refreshes := 0, 0
	eng.collectContext = func() *ctxt.ProjectContext {
		collects++
		return &ctxt.ProjectContext{Cwd: "/w", IsGitRepo: true, GitRoot: "/w", GitBranch: "main", GitStatus: "clean"}
	}
	eng.refreshGit = func(pc *ctxt.ProjectContext) {
		refreshes++
		pc.GitStatus = "changed"
	}
	eng.SetProjectContext(eng.collectContext())

	for i := 0; i < 2; i++ {
		if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "turn"}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if collects != 1 {
		t.Fatalf("collectContext ran %d times, want 1 (startup only)", collects)
	}
	if refreshes != 2 {
		t.Fatalf("refreshGit ran %d times, want 2 (once per turn)", refreshes)
	}
	if _, status := eng.projCtx.GetGitInfo(); status != "changed" {
		t.Fatalf("git status = %q, the refresh did not reach the project context", status)
	}
}

// RefreshGitAll on a context outside a repository is a no-op, not a panic.
func TestRefreshGitAllOutsideRepoIsNoop(t *testing.T) {
	pc := &ctxt.ProjectContext{Cwd: t.TempDir()}
	pc.RefreshGitAll()
	var nilPC *ctxt.ProjectContext
	nilPC.RefreshGitAll()
	if b, s := pc.GetGitInfo(); b != "" || s != "" {
		t.Fatalf("got branch=%q status=%q outside a repo", b, s)
	}
}
