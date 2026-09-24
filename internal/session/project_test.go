package session

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

func sessionIDs(records []Record) []string {
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

func saveProjectSession(t *testing.T, s *Store, id, cwd string) {
	t.Helper()
	r := &Record{
		ID:        id,
		CreatedAt: time.Now(),
		Title:     id,
		Model:     "claude-opus-5",
		Cwd:       cwd,
		Messages:  []api.Message{{Role: "user", Content: "请看一下 " + id}},
	}
	if err := s.Save(r); err != nil {
		t.Fatalf("Save %s: %v", id, err)
	}
}

func TestListReturnsProjectDirOfEachSession(t *testing.T) {
	s := newTestStore(t)
	dirA := filepath.Join(t.TempDir(), "project-a")
	saveProjectSession(t, s, "sess-a", dirA)

	records, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 || records[0].Cwd != dirA {
		t.Fatalf("List = %+v, want one record with Cwd %q", records, dirA)
	}
}

func TestFilterByProjectHidesSessionsFromOtherDirectories(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	dirA := filepath.Join(root, "project-a")
	dirB := filepath.Join(root, "project-b")
	saveProjectSession(t, s, "sess-a1", dirA)
	saveProjectSession(t, s, "sess-a2", dirA)
	saveProjectSession(t, s, "sess-b", dirB)

	all, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := sessionIDs(all); strings.Join(got, ",") != "sess-a1,sess-a2,sess-b" {
		t.Fatalf("List (all view) = %v, want every project's sessions", got)
	}

	if got := sessionIDs(FilterByProject(all, dirA)); strings.Join(got, ",") != "sess-a1,sess-a2" {
		t.Errorf("FilterByProject(A) = %v, want [sess-a1 sess-a2]", got)
	}
	if got := sessionIDs(FilterByProject(all, dirB)); strings.Join(got, ",") != "sess-b" {
		t.Errorf("FilterByProject(B) = %v, want [sess-b]", got)
	}
}

// Sessions saved before the cwd field existed have no project. They must stay
// on disk and in the all view, but not leak into every project's list.
func TestFilterByProjectExcludesLegacySessionsWithoutDir(t *testing.T) {
	s := newTestStore(t)
	dirA := filepath.Join(t.TempDir(), "project-a")
	saveProjectSession(t, s, "sess-a", dirA)
	writeRawSession(t, s.dir, "legacy.json",
		`{"id":"legacy","title":"旧会话","model":"claude-opus-5","messages":[{"role":"user","content":"旧项目的问题"}]}`)

	all, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := sessionIDs(all); strings.Join(got, ",") != "legacy,sess-a" {
		t.Fatalf("List (all view) = %v, want the legacy session kept", got)
	}
	if got := sessionIDs(FilterByProject(all, dirA)); strings.Join(got, ",") != "sess-a" {
		t.Errorf("FilterByProject = %v, want the legacy session excluded", got)
	}
}

func TestSameProjectDirNormalizesPaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	if !SameProjectDir(dir, filepath.Join(root, "project", "sub", "..")) {
		t.Error("a path with ../ should match its cleaned form")
	}
	if !SameProjectDir(dir, dir+string(filepath.Separator)) {
		t.Error("a trailing separator should not change the project")
	}
	if SameProjectDir(dir, filepath.Join(root, "project-other")) {
		t.Error("a sibling directory must not match")
	}
	if SameProjectDir("", dir) || SameProjectDir(dir, "") {
		t.Error("an unknown (empty) directory must not match any project")
	}
}

func TestSameProjectDirIgnoresCaseOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("paths are case-sensitive on this platform")
	}
	dir := filepath.Join(t.TempDir(), "Project")
	if !SameProjectDir(dir, strings.ToUpper(dir)) {
		t.Errorf("%q and its upper-case form should be the same project on Windows", dir)
	}
}

func TestNormalizeProjectDirMakesPathAbsoluteAndClean(t *testing.T) {
	if got := NormalizeProjectDir(""); got != "" {
		t.Errorf("NormalizeProjectDir(\"\") = %q, want empty", got)
	}
	got := NormalizeProjectDir(filepath.Join("some", "rel", "..", "dir"))
	if !filepath.IsAbs(got) {
		t.Fatalf("NormalizeProjectDir(relative) = %q, want an absolute path", got)
	}
	if !strings.HasSuffix(got, filepath.Join("some", "dir")) {
		t.Errorf("NormalizeProjectDir = %q, want it to end in some%cdir", got, filepath.Separator)
	}
}

func TestProjectMismatchWarningOnlyForOtherKnownDirectory(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "project-a")
	dirB := filepath.Join(root, "project-b")

	if w := ProjectMismatchWarning(&Record{Cwd: dirA}, dirA); w != "" {
		t.Errorf("same project warned: %q", w)
	}
	w := ProjectMismatchWarning(&Record{Cwd: dirB}, dirA)
	if !strings.Contains(w, dirB) || !strings.Contains(w, dirA) {
		t.Errorf("warning %q should name both the session's and the current directory", w)
	}
	if w := ProjectMismatchWarning(&Record{}, dirA); w != "" {
		t.Errorf("legacy session without a directory warned: %q", w)
	}
}
