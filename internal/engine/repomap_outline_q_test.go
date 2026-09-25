package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	ctxt "github.com/liuzhixin405/cove/internal/context"
	"github.com/liuzhixin405/cove/internal/repomap"
)

func thisRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// The system prompt carries a small project outline instead of the full repo
// map (38.6KB of the 51.7KB prompt on this repository) and the file tree.
func TestSystemPromptCarriesOutlineNotRepoMap(t *testing.T) {
	isolatedHome(t)
	root := thisRepoRoot(t)
	eng := newTestEngine(&mockProvider{})
	eng.enhancedRepoMap = repomap.NewEnhancedGenerator(root)
	eng.SetProjectContext(&ctxt.ProjectContext{
		Cwd:      root,
		FileTree: strings.Repeat("some/dir/file.go\n", 2000),
		RepoMap:  "LEGACY_REPO_MAP_MARKER",
	})
	eng.systemPrompt = ""
	p := eng.SystemPrompt()

	for _, bad := range []string{"<repo_map>", "LEGACY_REPO_MAP_MARKER", "Project structure:", "some/dir/file.go"} {
		if strings.Contains(p, bad) {
			t.Errorf("system prompt still carries %q", bad)
		}
	}
	start, end := strings.Index(p, "<project_outline>"), strings.Index(p, "</project_outline>")
	if start < 0 || end < start {
		t.Fatalf("system prompt has no <project_outline>:\n%s", p)
	}
	if n := end + len("</project_outline>\n") - start; n > 4096 {
		t.Fatalf("project outline is %d bytes, want <= 4096", n)
	}
	// Chat turns send this prompt in full: it was 51,720 bytes on this repo.
	if len(p) >= 15*1024 {
		t.Fatalf("system prompt is %d bytes on this repo, want < 15KB", len(p))
	}
	t.Logf("system prompt on this repo: %d bytes", len(p))
}

// The outline sits in the cached prefix: rebuilding the prompt must not
// change it.
func TestProjectOutlineStableAcrossRebuilds(t *testing.T) {
	isolatedHome(t)
	root := thisRepoRoot(t)
	eng := newTestEngine(&mockProvider{})
	eng.enhancedRepoMap = repomap.NewEnhancedGenerator(root)
	eng.SetProjectContext(&ctxt.ProjectContext{Cwd: root})
	eng.systemPrompt = ""
	a := eng.SystemPrompt()
	eng.systemPrompt = ""
	if b := eng.SystemPrompt(); a != b {
		t.Fatal("system prompt changed between two builds with no file changed")
	}
}

func TestLooksLikeTask(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"你好", false},
		{"hi there", false},
		{"谢谢，辛苦了", false},
		{"修复 internal/engine/engine.go 里的 panic", true},
		{"为什么 `buildPrompt` 返回空", true},
		{"turnContextNote() 是做什么的", true},
		{"please fix the login bug", true},
		{"测试一下这个", true},
		{"实现一个新命令", true},
		{"what does engine.go do", true},
		{"The build fails with an error", true},
		{strings.Repeat("这个需求比较长，", 30), true},
	}
	for _, c := range cases {
		if got := looksLikeTask(c.msg); got != c.want {
			t.Errorf("looksLikeTask(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func excerptNotes(msgs []api.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, "<repo_map_excerpt>") {
			out = append(out, m.Content)
		}
	}
	return out
}

// The first task-like turn gets a relevant repo map excerpt after the user's
// message; a chat turn before it and task turns after it do not.
func TestRepoMapExcerptOnFirstTaskTurnOnly(t *testing.T) {
	isolatedHome(t)
	root := t.TempDir()
	files := map[string]string{
		"go.mod":                    "module x\n",
		"internal/engine/engine.go": "package engine\n\ntype Engine struct{}\n\nfunc (e *Engine) Run() {}\n",
		"internal/engine/turn.go":   "package engine\n\nfunc turnNote() {}\n",
		"internal/store/store.go":   "package store\n\nfunc Save() {}\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "hi"}, {content: "done"}, {content: "done again"}}})
	eng.enhancedRepoMap = repomap.NewEnhancedGenerator(root)
	eng.SetProjectContext(&ctxt.ProjectContext{Cwd: root})

	run := func(msg string) {
		t.Helper()
		if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: msg}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	run("你好")
	if n := len(excerptNotes(eng.messages)); n != 0 {
		t.Fatalf("chat turn got %d repo map excerpts", n)
	}

	run("修复 internal/engine/engine.go 里的 panic")
	notes := excerptNotes(eng.messages)
	if len(notes) != 1 {
		t.Fatalf("first task turn got %d excerpts, want 1", len(notes))
	}
	ex := notes[0][strings.Index(notes[0], "<repo_map_excerpt>"):]
	ex = ex[:strings.Index(ex, "</repo_map_excerpt>")+len("</repo_map_excerpt>")]
	if len(ex) > repoMapExcerptMaxBytes {
		t.Fatalf("excerpt is %d bytes, cap %d", len(ex), repoMapExcerptMaxBytes)
	}
	if !strings.Contains(ex, "internal/engine/engine.go") {
		t.Fatalf("excerpt lacks the named file:\n%s", ex)
	}
	if strings.Contains(eng.SystemPrompt(), "<repo_map_excerpt>") {
		t.Fatal("excerpt leaked into the system prompt")
	}

	run("fix the Save bug in internal/store/store.go")
	if n := len(excerptNotes(eng.messages)); n != 1 {
		t.Fatalf("second task turn added an excerpt (total %d), want only the first", n)
	}
}

func TestMemoryIndexCapped(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("\n\n<user_memories>\n<memory>\n<name>CLAUDE.md</name>\n<content>\nproject rules\n</content>\n</memory>\n")
	sb.WriteString("<memory_index>\nSaved memories (too many to include in full). Read the file of any entry relevant to the current task before relying on it:\n")
	for i := 0; i < 200; i++ {
		sb.WriteString("- memory-entry-" + strings.Repeat("x", 20) + ".md (C:\\Users\\u\\.cove\\memory\\memory-entry.md): # Some memory title here\n")
	}
	sb.WriteString("</memory_index>\n</user_memories>\n")
	in := sb.String()

	out := capMemoryIndex(in)
	start, end := strings.Index(out, "<memory_index>"), strings.Index(out, "</memory_index>")
	if start < 0 || end < 0 {
		t.Fatalf("capped prompt lost the index tags:\n%s", out)
	}
	if n := end + len("</memory_index>\n") - start; n > memoryIndexMaxBytes {
		t.Fatalf("memory index is %d bytes, cap %d", n, memoryIndexMaxBytes)
	}
	if !strings.Contains(out, "project rules") || !strings.HasSuffix(out, "</user_memories>\n") {
		t.Fatalf("capping touched the rest of the memory block:\n%s", out)
	}
	if !strings.Contains(out, "more saved memories") {
		t.Fatalf("capped index does not say entries were left out:\n%s", out[start:])
	}

	small := "\n\n<user_memories>\n<memory_index>\nhead\n- a (p): t\n</memory_index>\n</user_memories>\n"
	if got := capMemoryIndex(small); got != small {
		t.Fatalf("small index changed:\n%s", got)
	}
	if got := capMemoryIndex("no index"); got != "no index" {
		t.Fatalf("non-index prompt changed: %q", got)
	}
}

// A task-like message with nothing to search for (Chinese only, no path or
// identifier) gets no excerpt and does not use up the session's one excerpt.
func TestRepoMapExcerptNeedsSearchTerms(t *testing.T) {
	isolatedHome(t)
	root := t.TempDir()
	p := filepath.Join(root, "internal", "engine", "engine.go")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("package engine\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(&mockProvider{responses: []mockResponse{{content: "a"}, {content: "b"}, {content: "c"}}})
	eng.enhancedRepoMap = repomap.NewEnhancedGenerator(root)
	eng.SetProjectContext(&ctxt.ProjectContext{Cwd: root})
	run := func(msg string) {
		t.Helper()
		if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: msg}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	longChat := strings.Repeat("今天天气不错我们随便聊聊最近的生活和工作吧", 10)
	for _, msg := range []string{"我在测试你的能力", longChat} {
		if !looksLikeTask(msg) {
			t.Fatalf("precondition: %q should look like a task", msg)
		}
		run(msg)
		if n := len(excerptNotes(eng.messages)); n != 0 {
			t.Fatalf("%q got an excerpt", msg)
		}
		if eng.repoMapExcerpts != 0 {
			t.Fatalf("%q used up the excerpt (count %d)", msg, eng.repoMapExcerpts)
		}
	}

	run("请看一下 internal/engine/engine.go 里的问题")
	if n := len(excerptNotes(eng.messages)); n != 1 {
		t.Fatalf("task naming a file got %d excerpts, want 1", n)
	}
}

func TestLooksLikeTaskTestWordBoundary(t *testing.T) {
	for _, msg := range []string{"testimony of a witness", "a protest happened"} {
		if looksLikeTask(msg) {
			t.Errorf("looksLikeTask(%q) = true", msg)
		}
	}
	for _, msg := range []string{"run the tests", "testing it now", "two bugs and errors", "fixed yesterday"} {
		if !looksLikeTask(msg) {
			t.Errorf("looksLikeTask(%q) = false", msg)
		}
	}
}
