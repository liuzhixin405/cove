package skills

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Matching compared only filepath.Base(filePath), so any pattern with a
// directory part (`src/api/*.go`, `internal/**/*.ts`) could never match.
func TestMatchingDirectoryPatterns(t *testing.T) {
	// Directory patterns are anchored at the project root, and a relative
	// path is also resolved against the git root. Running from this package
	// directory (as `go test` does) would make the git-root form of
	// "other/internal/x.ts" be ".../internal/skills/other/internal/x.ts",
	// which does match `internal/**/*.ts`; run from an unrelated directory
	// so the table describes the documented behaviour.
	t.Chdir(t.TempDir())
	m := NewManager()
	m.Register(Skill{Name: "api", Conditional: true, Paths: []string{"src/api/*.go"}})
	m.Register(Skill{Name: "ts", Conditional: true, Paths: []string{"internal/**/*.ts"}})
	m.Register(Skill{Name: "base", Conditional: true, Paths: []string{"*.md"}})

	cases := map[string][]string{
		"src/api/user.go":            {"api"},
		"src/api/v2/user.go":         nil,
		"src/other/user.go":          nil,
		"internal/a.ts":              {"ts"},
		"internal/x/y/z.ts":          {"ts"},
		"other/internal/x.ts":        nil,
		"./src/api/user.go":          {"api"},
		`src\api\user.go`:            {"api"},
		"docs/deep/README.md":        {"base"},
		"internal/x/y/z.go":          nil,
		"internal/x/y/z.ts.bak":      nil,
		"src/api/user.go/../user.go": {"api"},
	}
	for p, want := range cases {
		var got []string
		for _, s := range m.Matching(context.Background(), p) {
			got = append(got, s.Name)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Matching(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestMatchingAbsolutePathUnderCwd(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	m := NewManager()
	m.Register(Skill{Name: "api", Conditional: true, Paths: []string{"src/api/*.go"}})
	abs := filepath.Join(dir, "src", "api", "user.go")
	if got := m.Matching(context.Background(), abs); len(got) != 1 {
		t.Fatalf("absolute path under cwd did not match: %v", got)
	}
	// Relative to the git root when cwd is a subdirectory of it.
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "cmd")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)
	if got := m.Matching(context.Background(), abs); len(got) != 1 {
		t.Fatalf("absolute path under the git root did not match: %v", got)
	}
}

func TestParseFrontmatterRobustness(t *testing.T) {
	want := func(t *testing.T, label, content string) Skill {
		t.Helper()
		s := parseSkill("x", content, "x/SKILL.md")
		if s.Name != "demo" || s.Description != "Demo skill" {
			t.Fatalf("%s: name=%q description=%q (frontmatter ignored)", label, s.Name, s.Description)
		}
		return s
	}
	lf := "---\nname: demo\ndescription: Demo skill\npaths: \"*.go\"\n---\nBody line\n"
	crlf := "---\r\nname: demo\r\ndescription: Demo skill\r\npaths: \"*.go\"\r\n---\r\nBody line\r\n"
	for label, c := range map[string]string{
		"lf":   lf,
		"crlf": crlf,
		"bom":  utf8BOM + crlf,
	} {
		s := want(t, label, c)
		if !reflect.DeepEqual(s.Paths, []string{"*.go"}) || !s.Conditional {
			t.Errorf("%s: Paths=%q Conditional=%v", label, s.Paths, s.Conditional)
		}
		if s.Prompt != "Body line\n" {
			t.Errorf("%s: Prompt = %q", label, s.Prompt)
		}
	}
	// Closing --- at EOF with no trailing newline.
	if s := want(t, "eof", "---\nname: demo\ndescription: Demo skill\n---"); s.Prompt != "" {
		t.Errorf("eof: Prompt = %q", s.Prompt)
	}
}

func TestParseFrontmatterYAMLLists(t *testing.T) {
	s := parseSkill("x", "---\nname: demo\ndescription: Demo skill\npaths:\n  - \"*.go\"\n  - 'src/**/*.ts'\n  -\nallowed_tools: [read, \"grep\", ]\nsteps:\n  - Read the file\n  - Run tests\n---\nbody\n", "x/SKILL.md")
	if !reflect.DeepEqual(s.Paths, []string{"*.go", "src/**/*.ts"}) {
		t.Errorf("Paths = %q", s.Paths)
	}
	if !reflect.DeepEqual(s.AllowedTools, []string{"read", "grep"}) {
		t.Errorf("AllowedTools = %q", s.AllowedTools)
	}
	if !reflect.DeepEqual(s.Steps, []string{"Read the file", "Run tests"}) {
		t.Errorf("Steps = %q", s.Steps)
	}
	if !s.Conditional {
		t.Error("expected Conditional with real patterns")
	}

	// An empty list must never make a skill conditional (it used to yield
	// Paths=[""] and Conditional=true).
	for _, c := range []string{
		"---\nname: demo\npaths:\n---\nbody\n",
		"---\nname: demo\npaths: []\n---\nbody\n",
		"---\nname: demo\npaths: \" , \"\n---\nbody\n",
	} {
		s := parseSkill("x", c, "x/SKILL.md")
		if s.Conditional || len(s.Paths) != 0 {
			t.Errorf("%q: Paths=%q Conditional=%v", c, s.Paths, s.Conditional)
		}
	}
}
