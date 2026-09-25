package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Item 1: instruction files load from the git root down to cwd, in the order
// CLAUDE.md, .claude/CLAUDE.md, AGENTS.md, .cove.md, deduplicated.
func TestInstructionFilesWalkUpToGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "pkg", "svc")
	writeT(t, filepath.Join(root, "AGENTS.md"), "root agents rules")
	writeT(t, filepath.Join(root, "CLAUDE.md"), "root claude rules")
	writeT(t, filepath.Join(root, ".cove.md"), "root cove rules")
	writeT(t, filepath.Join(sub, ".claude", "CLAUDE.md"), "svc claude rules")
	writeT(t, filepath.Join(sub, "AGENTS.md"), "root agents rules") // duplicate content
	// Above the git root: must not be loaded.
	writeT(t, filepath.Join(filepath.Dir(root), "AGENTS.md"), "outside rules")
	t.Cleanup(func() { _ = os.Remove(filepath.Join(filepath.Dir(root), "AGENTS.md")) })

	s := &Store{dirs: []string{t.TempDir()}, cwd: sub}
	var got []string
	for _, e := range s.All() {
		if e.Project {
			got = append(got, e.Content)
		}
	}
	want := []string{"root claude rules", "root agents rules", "root cove rules", "svc claude rules"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("instruction files = %q, want %q", got, want)
	}
	p := s.BuildPrompt()
	if strings.Contains(p, "outside rules") {
		t.Fatal("file above git root was loaded")
	}
	if s.InstructionFilesTruncated() {
		t.Fatal("small instruction files reported truncated")
	}
}

func TestInstructionFilesNoGitRootOnlyCwd(t *testing.T) {
	parent := t.TempDir()
	cwd := filepath.Join(parent, "work")
	writeT(t, filepath.Join(parent, "AGENTS.md"), "parent rules")
	writeT(t, filepath.Join(cwd, "AGENTS.md"), "cwd rules")
	s := &Store{dirs: []string{t.TempDir()}, cwd: cwd}
	p := s.BuildPrompt()
	if !strings.Contains(p, "cwd rules") || strings.Contains(p, "parent rules") {
		t.Fatalf("without a git root only cwd should load:\n%s", p)
	}
}

func TestInstructionFilesTruncatedFlag(t *testing.T) {
	cwd := t.TempDir()
	writeT(t, filepath.Join(cwd, "CLAUDE.md"), strings.Repeat("a", MaxInstructionBytes-10))
	writeT(t, filepath.Join(cwd, "AGENTS.md"), strings.Repeat("b", 1000))
	s := &Store{dirs: []string{t.TempDir()}, cwd: cwd}
	p := s.BuildPrompt()
	if !s.InstructionFilesTruncated() {
		t.Fatal("expected truncation flag")
	}
	if strings.Count(p, "b") > 100 {
		t.Fatal("over-budget instruction content was not clipped")
	}
}

// Item 2: past 24KB, PromptFor puts the BM25 top matches in full and indexes the rest.
func TestPromptForTopKFullTextPlusIndex(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		files[fmt.Sprintf("m%02d.md", i)] = fmt.Sprintf("Topic %d filler\n%s", i, strings.Repeat("generic filler words here\n", 80))
	}
	files["deploy.md"] = "Deploy runbook: kubernetes helm rollout canary\n" + strings.Repeat("kubernetes helm canary\n", 20)
	s := newTestStore(t, files)
	s.cwd = t.TempDir()

	p := s.PromptFor("how do I do a kubernetes canary rollout")
	if !strings.Contains(p, "kubernetes helm canary") {
		t.Fatal("relevant memory not included in full")
	}
	if !strings.Contains(p, "<memory_index>") || !strings.Contains(p, "m05.md") {
		t.Fatal("rest should be indexed")
	}
	if strings.Count(p, "generic filler words here") > 8*80 {
		t.Fatal("more than top-8 included in full")
	}
	// BuildPrompt still works and indexes everything (no query).
	bp := s.BuildPrompt()
	if strings.Contains(bp, "kubernetes helm canary\n") || !strings.Contains(bp, "deploy.md") {
		t.Fatal("BuildPrompt should index without a query")
	}
	if r := s.RelevantMemoriesFor("kubernetes canary"); !strings.Contains(r, "kubernetes helm canary") {
		t.Fatal("RelevantMemoriesFor should return the ranked full text")
	}
}

func TestPromptForInlinesUnder24KB(t *testing.T) {
	s := newTestStore(t, map[string]string{
		"a.md": strings.Repeat("x", 10*1024),
		"b.md": strings.Repeat("y", 10*1024),
	})
	s.cwd = t.TempDir()
	p := s.PromptFor("anything")
	if strings.Contains(p, "<memory_index>") {
		t.Fatal("20KB should be inlined under the 24KB threshold")
	}
	if r := s.RelevantMemoriesFor("anything"); r != "" {
		t.Fatalf("nothing extra when all inline, got %q", r)
	}
}

// Item 4: project + global dirs merge, project wins, sources reported, Save goes to project.
func TestProjectAndGlobalMemoryMerge(t *testing.T) {
	proj, glob := t.TempDir(), t.TempDir()
	writeT(t, filepath.Join(proj, "style.md"), "project style")
	writeT(t, filepath.Join(glob, "style.md"), "global style")
	writeT(t, filepath.Join(glob, "me.md"), "global me")
	s := NewStoreForProject(proj, glob)
	s.cwd = t.TempDir()
	got := map[string]Entry{}
	for _, e := range s.All() {
		got[e.Name] = e
	}
	if got["style.md"].Content != "project style" || got["style.md"].Source != SourceProject {
		t.Fatalf("project entry should win: %+v", got["style.md"])
	}
	if got["me.md"].Source != SourceGlobal {
		t.Fatalf("global entry source: %+v", got["me.md"])
	}
	if err := s.Save("new.md", "fresh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, "new.md")); err != nil {
		t.Fatal("Save should write into the project dir")
	}
	if s.PrimaryDir() != proj {
		t.Fatal("PrimaryDir should be the project dir")
	}
}

func TestSaveTotalLimit300KB(t *testing.T) {
	if MaxTotalBytes != 300*1024 {
		t.Fatalf("MaxTotalBytes = %d", MaxTotalBytes)
	}
	s := newTestStore(t, nil)
	line := strings.Repeat("q", 100) + "\n"
	content := strings.Repeat(line, 190) // ~19KB, 190 lines
	for i := 0; i < 16; i++ {
		if err := s.Save(fmt.Sprintf("f%02d.md", i), content); err != nil {
			t.Fatalf("save %d: %v (should fit in 300KB)", i, err)
		}
	}
	if err := s.Save("over.md", content); err == nil {
		t.Fatal("expected total-limit error past 300KB")
	}
}

func TestProjectRootFindsGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if got := ProjectRoot(sub); got != filepath.Clean(root) {
		t.Fatalf("ProjectRoot = %q, want %q", got, root)
	}
	other := t.TempDir()
	if got := ProjectRoot(other); got != filepath.Clean(other) {
		t.Fatalf("ProjectRoot without git = %q", got)
	}
}

func TestNewProjectStoreLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	data := filepath.Join(home, ".cove", "projects", "abcd1234")
	s := NewProjectStore(data)
	if s.PrimaryDir() != filepath.Join(data, "memory") {
		t.Fatalf("primary = %q", s.PrimaryDir())
	}
	if s.dirs[1] != filepath.Join(home, ".cove", "memory") {
		t.Fatalf("global = %q", s.dirs[1])
	}
}

// Appending to a name that only exists globally writes into the project
// directory on top of the global content, so the project copy does not hide
// what the global one said.
func TestAppendOverGlobalKeepsGlobalContent(t *testing.T) {
	proj, glob := t.TempDir(), t.TempDir()
	writeT(t, filepath.Join(glob, "user-preferences.md"), "Prefers tabs.")
	s := NewStoreForProject(proj, glob)
	s.cwd = t.TempDir()
	name, err := s.Append("user-preferences.md", "Replies in Chinese.")
	if err != nil || name != "user-preferences.md" {
		t.Fatalf("Append = %q, %v", name, err)
	}
	p := s.PromptFor("preferences")
	if !strings.Contains(p, "Prefers tabs.") || !strings.Contains(p, "Replies in Chinese.") {
		t.Fatalf("prompt lost old or new content:\n%s", p)
	}
	if _, err := os.Stat(filepath.Join(proj, "user-preferences.md")); err != nil {
		t.Fatal("append should land in the project directory")
	}
	if data, _ := os.ReadFile(filepath.Join(glob, "user-preferences.md")); string(data) != "Prefers tabs." {
		t.Fatal("global file must not change")
	}
}

func TestAppendRollsOverPastFileCap(t *testing.T) {
	s := newTestStore(t, map[string]string{"grow.md": strings.Repeat("x", AppendFileBytes)})
	s.cwd = t.TempDir()
	name, err := s.Append("grow.md", "new fact")
	if err != nil || name != "grow-2.md" {
		t.Fatalf("Append = %q, %v; want grow-2.md", name, err)
	}
}

// Appending exactly what the file already ends with (line by line) is a no-op.
func TestAppendSkipsDuplicateTail(t *testing.T) {
	s := newTestStore(t, map[string]string{"notes.md": "Old fact.\nUses pnpm.\nRuns go test.\n"})
	s.cwd = t.TempDir()
	if _, err := s.Append("notes.md", "Uses pnpm.\nRuns go test."); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(s.dirs[0], "notes.md"))
	if strings.Count(string(data), "Uses pnpm.") != 1 {
		t.Fatalf("duplicate tail appended again: %q", data)
	}
	if _, err := s.Append("notes.md", "New fact."); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(s.dirs[0], "notes.md"))
	if !strings.Contains(string(data), "New fact.") {
		t.Fatalf("new content lost: %q", data)
	}
}
