package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody of " + description + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// layout builds: <root>/home (HOME), <root>/outside/repo (git repo) and
// <root>/outside/repo/pkg (the working directory).
func layout(t *testing.T) (home, outside, repo, cwd string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	outside = filepath.Join(root, "outside")
	repo = filepath.Join(outside, "repo")
	cwd = filepath.Join(repo, "pkg")
	for _, d := range []string{home, filepath.Join(repo, ".git"), cwd} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return
}

func descOf(t *testing.T, m *Manager, name string) string {
	t.Helper()
	s, ok := m.Get(name)
	if !ok {
		return "<missing>"
	}
	return s.Description
}

// The last directory loaded wins, and parent directories were loaded last, so
// a skill in the home directory (an ancestor of most projects) overrode the
// project's own. The closer definition must win: project > user > built-in.
func TestProjectSkillBeatsUserSkillBeatsBuiltin(t *testing.T) {
	home, _, _, cwd := layout(t)
	writeSkill(t, filepath.Join(home, ".cove", "skills"), "plan", "user plan")
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "plan", "user claude plan")
	writeSkill(t, filepath.Join(home, ".cove", "skills"), "spike", "user spike")
	writeSkill(t, filepath.Join(cwd, ".cove", "skills"), "plan", "project plan")

	m := NewManager()
	LoadAll(m, cwd)
	if got := descOf(t, m, "plan"); got != "project plan" {
		t.Errorf("plan = %q, want the project's definition", got)
	}
	if got := descOf(t, m, "spike"); got != "user spike" {
		t.Errorf("spike = %q, want the user's definition over the built-in", got)
	}
}

// The usual layout: the project lives under the home directory, so walking up
// from the project reached home and loaded ~/.claude/skills last — overriding
// the project's own definition.
func TestProjectUnderHomeStillWinsOverUserSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repo := filepath.Join(home, "code", "proj")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(home, ".claude", "skills"), "plan", "user claude plan")
	writeSkill(t, filepath.Join(repo, ".cove", "skills"), "plan", "project plan")

	m := NewManager()
	LoadAll(m, repo)
	if got := descOf(t, m, "plan"); got != "project plan" {
		t.Fatalf("plan = %q, want the project's definition", got)
	}
}

// Within a repository, skills in directories between the repo root and the
// working directory are loaded, the closer one winning. Nothing above the repo
// root is loaded, and neither are subdirectories: a vendored or cloned
// dependency that ships a .claude/skills folder must not inject skills.
func TestSkillSearchStaysInsideTheRepository(t *testing.T) {
	_, outside, repo, cwd := layout(t)
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "shared", "repo root")
	writeSkill(t, filepath.Join(cwd, ".claude", "skills"), "shared", "package")
	writeSkill(t, filepath.Join(repo, ".claude", "skills"), "rootonly", "repo root only")
	writeSkill(t, filepath.Join(outside, ".claude", "skills"), "foreign", "above the repo")
	writeSkill(t, filepath.Join(cwd, "vendor", "lib", ".claude", "skills"), "vendored", "from a dependency")
	writeSkill(t, filepath.Join(cwd, "vendor", ".claude", "skills"), "vendored2", "from a dependency")

	m := NewManager()
	LoadAll(m, cwd)
	if got := descOf(t, m, "shared"); got != "package" {
		t.Errorf("shared = %q, want the package-level (closer) definition", got)
	}
	if got := descOf(t, m, "rootonly"); got != "repo root only" {
		t.Errorf("rootonly = %q, want the repo-root skill to load", got)
	}
	for _, name := range []string{"foreign", "vendored", "vendored2"} {
		if _, ok := m.Get(name); ok {
			t.Errorf("skill %q from outside the repo / a subdirectory was loaded", name)
		}
	}
}

// Built-in skills are workflows (planning, TDD, debugging, spikes, PR flow),
// not file-type rules. They are listed for the model to load on demand and are
// never pushed into the conversation because a source file was touched.
func TestBuiltinSkillsAreNeverAutoInjectedByFileType(t *testing.T) {
	m := NewManager()
	m.LoadEmbedded()
	if m.Count() < 10 {
		t.Fatalf("only %d built-in skills loaded", m.Count())
	}
	for _, f := range []string{"main.go", "app.py", "index.ts", "lib.rs", "App.java", "x.rb"} {
		if got := m.Matching(context.Background(), f); len(got) != 0 {
			var names []string
			for _, s := range got {
				names = append(names, s.Name)
			}
			t.Errorf("%s triggers built-in skills %v", f, names)
		}
	}
}

// The skill list is part of the system prompt; iterating the map produced a
// different order on every start, so the prompt differed between sessions.
func TestSkillListIsSortedByName(t *testing.T) {
	m := NewManager()
	for _, n := range []string{"zeta", "alpha", "mike", "bravo", "yankee", "charlie"} {
		m.Register(Skill{Name: n, Description: n})
	}
	prompt := m.BuildPrompt()
	var order []string
	for _, n := range []string{"alpha", "bravo", "charlie", "mike", "yankee", "zeta"} {
		order = append(order, n)
		if !strings.Contains(prompt, "<name>"+n+"</name>") {
			t.Fatalf("%s missing", n)
		}
	}
	last := -1
	for _, n := range order {
		i := strings.Index(prompt, "<name>"+n+"</name>")
		if i < last {
			t.Fatalf("skills not in name order: %s appears too early\n%s", n, prompt)
		}
		last = i
	}
	var names []string
	for _, s := range m.All() {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, order) {
		t.Fatalf("All() = %v, want sorted %v", names, order)
	}
}

func TestDisableRemovesSkills(t *testing.T) {
	m := NewManager()
	m.LoadEmbedded()
	m.Disable("spike", "plan", "no-such-skill")
	for _, n := range []string{"spike", "plan"} {
		if _, ok := m.Get(n); ok {
			t.Errorf("%s still loaded after Disable", n)
		}
	}
	if _, ok := m.Get("systematic-debugging"); !ok {
		t.Error("Disable removed a skill it was not asked to")
	}
}

// Paths were split on commas without removing the quotes around the value, so
// `paths: "*.go,*.py"` produced the patterns `"*.go` and `*.py"`, which never
// match anything.
func TestQuotedPathsInFrontmatterMatch(t *testing.T) {
	s := parseSkill("x", "---\nname: x\npaths: \"*.go,*.py\"\n---\nbody\n", "x/SKILL.md")
	if !reflect.DeepEqual(s.Paths, []string{"*.go", "*.py"}) {
		t.Fatalf("Paths = %q", s.Paths)
	}
	m := NewManager()
	m.Register(s)
	if len(m.Matching(context.Background(), "a.go")) != 1 {
		t.Fatal("quoted first pattern did not match a.go")
	}
}

func TestSkillsRecordWhereTheyCameFrom(t *testing.T) {
	home, _, _, cwd := layout(t)
	writeSkill(t, filepath.Join(home, ".cove", "skills"), "mine", "user skill")
	writeSkill(t, filepath.Join(cwd, ".cove", "skills"), "ours", "project skill")
	m := NewManager()
	LoadAll(m, cwd)
	for name, want := range map[string]string{"mine": SourceUser, "ours": SourceProject, "plan": SourceBuiltin} {
		if s, _ := m.Get(name); s.Source != want {
			t.Errorf("%s: source %q, want %q", name, s.Source, want)
		}
	}
}

// Exporting copies a built-in skill to ~/.cove/skills so it can be edited; the
// copy then overrides the built-in. It never overwrites an existing file.
func TestExportBuiltinWritesAnEditableCopy(t *testing.T) {
	home, _, _, _ := layout(t)
	path, err := ExportBuiltin("plan")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".cove", "skills", "plan", "SKILL.md")
	if path != want {
		t.Fatalf("exported to %s, want %s", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "name: plan") {
		t.Fatalf("export content wrong (err=%v)", err)
	}
	if _, err := ExportBuiltin("plan"); err == nil {
		t.Fatal("second export overwrote the existing copy")
	}
	if _, err := ExportBuiltin("no-such-skill"); err == nil {
		t.Fatal("exporting an unknown skill succeeded")
	}
}
