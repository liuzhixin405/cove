package command

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/session"
)

// mismatchWarning is the phrase of session.ProjectMismatchWarning; the
// hidden-sessions hint also says "其他项目", so match the warning itself.
const mismatchWarning = "该会话属于其他项目目录"

// projectStore holds one session from project A, one from project B and one
// legacy session saved before sessions recorded their directory.
func projectStore(t *testing.T) (store *fakeSessionStore, dirA, dirB string) {
	t.Helper()
	root := t.TempDir()
	dirA = filepath.Join(root, "project-a")
	dirB = filepath.Join(root, "project-b")
	msgs := []api.Message{{Role: "user", Content: "hello"}}
	return &fakeSessionStore{records: map[string]session.Record{
		"sess-a":     {ID: "sess-a", Title: "A 项目会话", Cwd: dirA, Messages: msgs},
		"sess-b":     {ID: "sess-b", Title: "B 项目会话", Cwd: dirB, Messages: msgs},
		"sess-old":   {ID: "sess-old", Title: "旧版会话", Messages: msgs},
		"sess-a-dup": {ID: "sess-a-dup", Title: "A 项目另一会话", Cwd: strings.ToUpper(dirA[:1]) + dirA[1:], Messages: msgs},
	}}, dirA, dirB
}

func TestResumeCmdListsOnlyCurrentProjectSessions(t *testing.T) {
	store, dirA, _ := projectStore(t)

	out, err := NewResumeCmd().Execute(context.Background(), Input{Cwd: dirA, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	// sess-a-dup spells the drive letter in another case; on Windows that is
	// still project A.
	for _, id := range []string{"sess-a ", "sess-a-dup"} {
		if !strings.Contains(out.Message, id) {
			t.Errorf("current project's session %q missing from /resume:\n%s", id, out.Message)
		}
	}
	for _, other := range []string{"sess-b", "sess-old"} {
		if strings.Contains(out.Message, other) {
			t.Errorf("/resume listed %s, which is not from this project:\n%s", other, out.Message)
		}
	}
	if !strings.Contains(out.Message, "/resume all") {
		t.Errorf("/resume should point at /resume all for the hidden sessions:\n%s", out.Message)
	}
}

func TestResumeCmdAllListsEveryProject(t *testing.T) {
	store, dirA, _ := projectStore(t)

	out, err := NewResumeCmd().Execute(context.Background(), Input{Args: []string{"all"}, Cwd: dirA, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"sess-a", "sess-b", "sess-old"} {
		if !strings.Contains(out.Message, id) {
			t.Errorf("/resume all is missing %s:\n%s", id, out.Message)
		}
	}
}

func TestResumeCmdByIDFromOtherProjectWarns(t *testing.T) {
	store, dirA, dirB := projectStore(t)
	eng := &fakeEngine{}

	out, err := NewResumeCmd().Execute(context.Background(), Input{Args: []string{"sess-b"}, Cwd: dirA, SessionStore: store, Engine: eng})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.loaded) == 0 {
		t.Fatal("an explicit session ID from another project should still be resumed")
	}
	if !strings.Contains(out.Message, mismatchWarning) || !strings.Contains(out.Message, dirB) {
		t.Errorf("resuming another project's session should warn and name its directory:\n%s", out.Message)
	}
}

func TestResumeCmdByIDFromSameProjectDoesNotWarn(t *testing.T) {
	store, dirA, _ := projectStore(t)

	out, err := NewResumeCmd().Execute(context.Background(), Input{Args: []string{"sess-a"}, Cwd: dirA, SessionStore: store, Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Message, mismatchWarning) {
		t.Errorf("same-project resume warned:\n%s", out.Message)
	}
}

func TestHistoryCmdListsOnlyCurrentProjectSessions(t *testing.T) {
	store, dirA, _ := projectStore(t)

	out, err := NewHistoryCmd().Execute(context.Background(), Input{Cwd: dirA, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "A 项目会话") {
		t.Errorf("current project's session missing from /history:\n%s", out.Message)
	}
	for _, other := range []string{"B 项目会话", "旧版会话"} {
		if strings.Contains(out.Message, other) {
			t.Errorf("/history listed %q, which is not from this project:\n%s", other, out.Message)
		}
	}
}

func TestHistoryCmdAllListsEveryProject(t *testing.T) {
	store, dirA, _ := projectStore(t)

	out, err := NewHistoryCmd().Execute(context.Background(), Input{Args: []string{"all"}, Cwd: dirA, SessionStore: store})
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"A 项目会话", "B 项目会话", "旧版会话"} {
		if !strings.Contains(out.Message, title) {
			t.Errorf("/history all is missing %q:\n%s", title, out.Message)
		}
	}
}
