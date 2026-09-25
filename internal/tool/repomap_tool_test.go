package tool

import (
	"context"
	"strings"
	"testing"
)

func repoMapTree(t *testing.T) string {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"go.mod":                    "module x\n",
		"internal/engine/engine.go": "package engine\n\ntype Engine struct{}\n\nfunc (e *Engine) Run() {}\n",
		"internal/engine/turn.go":   "package engine\n\nfunc turnContextNote() {}\n",
		"internal/store/store.go":   "package store\n\nfunc Save() {}\n",
	})
	return dir
}

func callRepoMap(t *testing.T, dir string, in Input) Result {
	t.Helper()
	res, err := NewRepoMapTool().Call(context.Background(), in, Context{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRepoMapToolDef(t *testing.T) {
	d := NewRepoMapTool().Def()
	if d.Name != "repo_map" || !d.IsReadOnly || !d.IsConcurrencySafe {
		t.Fatalf("def = %+v, want read-only concurrency-safe repo_map", d)
	}
	if !strings.Contains(string(d.InputSchema), `"query"`) || !strings.Contains(string(d.InputSchema), `"path"`) {
		t.Fatalf("schema lacks query/path: %s", d.InputSchema)
	}
	if strings.Contains(string(d.InputSchema), `"required"`) {
		t.Fatalf("query and path are optional: %s", d.InputSchema)
	}
	if got := NewRepoMapTool().CheckPermissions(Input{}, Context{}); got.Decision != Allow {
		t.Fatalf("permission = %+v, want allow", got)
	}
}

func TestRepoMapToolQueryReturnsRelevantFiles(t *testing.T) {
	dir := repoMapTree(t)
	res := callRepoMap(t, dir, Input{"query": "turnContextNote"})
	if res.IsError || !strings.Contains(res.Data, "internal/engine/turn.go") || strings.Contains(res.Data, "store.go") {
		t.Fatalf("query result = %q", res.Data)
	}

	res = callRepoMap(t, dir, Input{"path": "internal/store"})
	if res.IsError || !strings.Contains(res.Data, "store.go") || strings.Contains(res.Data, "engine.go") {
		t.Fatalf("path result = %q", res.Data)
	}

	res = callRepoMap(t, dir, Input{})
	if res.IsError || !strings.Contains(res.Data, ".go") {
		t.Fatalf("empty query result = %q", res.Data)
	}
	if len(res.Data) > repoMapToolMaxBytes {
		t.Fatalf("result is %d bytes, cap %d", len(res.Data), repoMapToolMaxBytes)
	}
}

func TestRepoMapToolNoMatch(t *testing.T) {
	dir := repoMapTree(t)
	res := callRepoMap(t, dir, Input{"query": "zzzNothing"})
	if res.IsError || !strings.Contains(res.Data, "No files") {
		t.Fatalf("no-match result = %+v", res)
	}
}

func TestRepoMapToolStaysInWorkingDirectory(t *testing.T) {
	dir, outside := repoMapTree(t), t.TempDir()
	if res := callRepoMap(t, dir, Input{"path": outside}); !res.IsError {
		t.Fatalf("repo_map outside cwd = %q, want an error", res.Data)
	}
}

func TestRepoMapToolRejectsParentPath(t *testing.T) {
	dir := repoMapTree(t)
	sub := dir + "/internal"
	if res := callRepoMap(t, sub, Input{"path": "../"}); !res.IsError {
		t.Fatalf("repo_map path ../ = %q, want an error", res.Data)
	}
}

func TestRepoMapToolTruncatesWithMarker(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	var body strings.Builder
	body.WriteString("package big\n\n")
	for i := 0; i < 40; i++ {
		body.WriteString("func HandlerWithAVeryLongDescriptiveName" + strings.Repeat("X", i) + "(requestContext string, responseWriter int) {}\n")
	}
	for i := 0; i < 30; i++ {
		files["big/handler_"+strings.Repeat("a", i+1)+".go"] = body.String()
	}
	writeTree(t, dir, files)
	res := callRepoMap(t, dir, Input{})
	if len(res.Data) > repoMapToolMaxBytes {
		t.Fatalf("result is %d bytes, cap %d", len(res.Data), repoMapToolMaxBytes)
	}
	if !strings.Contains(res.Data, "more matching files") {
		t.Fatalf("truncated result has no marker:\n%s", res.Data[len(res.Data)-300:])
	}
}

func TestRepoMapToolDescribesLanguages(t *testing.T) {
	desc := NewRepoMapTool().Def().Description
	for _, lang := range []string{"Go", "Python", "TypeScript", "JavaScript"} {
		if !strings.Contains(desc, lang) {
			t.Errorf("description lacks %s: %s", lang, desc)
		}
	}
}
