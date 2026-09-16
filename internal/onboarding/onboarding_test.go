package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateHome points the user-home lookups at a throwaway directory so nothing
// in this package can ever touch the developer's real ~/.cove. onboarding only
// writes inside State.ProjectDir today; this keeps that true by construction if
// someone later adds a home-relative path.
func isolateHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestCheckDetectsGoProjectWithGit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/x\n")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := Check(dir)
	if s.ProjectDir != dir {
		t.Errorf("ProjectDir = %q, want %q", s.ProjectDir, dir)
	}
	if !s.HasGoMod {
		t.Error("HasGoMod = false, want true")
	}
	if !s.HasGit {
		t.Error("HasGit = false, want true")
	}
	if s.HasPackageJSON {
		t.Error("HasPackageJSON = true, want false")
	}
	if s.HasClaudeMD {
		t.Error("HasClaudeMD = true, want false")
	}
	if s.Language != "Go" {
		t.Errorf("Language = %q, want %q", s.Language, "Go")
	}
	if !s.NeedsOnboarding() {
		t.Error("NeedsOnboarding() = false, want true for a project with no CLAUDE.md")
	}
}

func TestCheckFindsClaudeMDInDotClaudeSubdir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".claude", "CLAUDE.md"), "# guide\n")

	s := Check(dir)
	if !s.HasClaudeMD {
		t.Error("HasClaudeMD = false, want true for .claude/CLAUDE.md")
	}
	if s.NeedsOnboarding() {
		t.Error("NeedsOnboarding() = true, want false when .claude/CLAUDE.md exists")
	}
}

func TestCheckIgnoresClaudeMDThatIsADirectory(t *testing.T) {
	// fileExists deliberately rejects directories: a directory named CLAUDE.md
	// is not a usable project guide, and treating it as one would make
	// NeedsOnboarding say "already done" forever.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	s := Check(dir)
	if s.HasClaudeMD {
		t.Error("HasClaudeMD = true, want false when CLAUDE.md is a directory")
	}
}

func TestCheckDetectsGitWorktreeWhereDotGitIsAFile(t *testing.T) {
	// Linked worktrees and submodules store a ".git" *file* holding
	// "gitdir: <path>" instead of a ".git" directory.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git"), "gitdir: /somewhere/.git/worktrees/wt\n")

	s := Check(dir)
	if !s.HasGit {
		t.Error("HasGit = false, want true when .git is a gitdir file (worktree/submodule)")
	}
}

func TestCheckNoGitWhenDotGitAbsent(t *testing.T) {
	s := Check(t.TempDir())
	if s.HasGit {
		t.Error("HasGit = true, want false for a directory with no .git")
	}
}

func TestDetectLanguagePrecedence(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"go wins over node", []string{"go.mod", "package.json"}, "Go"},
		{"rust wins over node", []string{"Cargo.toml", "package.json"}, "Rust"},
		{"node alone", []string{"package.json"}, "JavaScript/TypeScript"},
		{"requirements before pyproject", []string{"pyproject.toml", "requirements.txt"}, "Python"},
		{"pyproject alone", []string{"pyproject.toml"}, "Python"},
		{"maven", []string{"pom.xml"}, "Java"},
		{"gradle", []string{"build.gradle"}, "Java/Kotlin"},
		{"ruby", []string{"Gemfile"}, "Ruby"},
		{"csproj via glob", []string{"App.csproj"}, "C#"},
		{"elixir", []string{"mix.exs"}, "Elixir"},
		{"empty dir", nil, "unknown"},
		{"unrecognized files only", []string{"README.md", "Makefile"}, "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tc.files {
				writeFile(t, filepath.Join(dir, f), "x")
			}
			if got := detectLanguage(dir); got != tc.want {
				t.Errorf("detectLanguage(%v) = %q, want %q", tc.files, got, tc.want)
			}
		})
	}
}

func TestDetectLanguageCsprojGlobDoesNotMatchLiteralStar(t *testing.T) {
	// Guards the glob branch: a file literally named "*.csproj" is not what the
	// pattern is for, but a real project file must still match.
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "MyApp.Web.csproj"), "<Project/>")
	if got := detectLanguage(dir); got != "C#" {
		t.Errorf("detectLanguage = %q, want %q", got, "C#")
	}
}

func TestGenerateClaudeMDGoProject(t *testing.T) {
	s := &State{Language: "Go", HasGit: true, HasGoMod: true}
	want := "# Project Guide\n\n" +
		"## Overview\n\n" +
		"Language: Go\n" +
		"Version Control: Git\n" +
		"\n## Build & Run\n\n" +
		"```bash\ngo build ./...\ngo test ./...\n```\n" +
		"\n## Conventions\n\n" +
		"<!-- Add project conventions, coding style, and important notes here -->\n"
	if got := s.GenerateClaudeMD(); got != want {
		t.Errorf("GenerateClaudeMD()\ngot:  %q\nwant: %q", got, want)
	}
}

func TestGenerateClaudeMDNodeProjectWithoutGit(t *testing.T) {
	s := &State{Language: "JavaScript/TypeScript", HasPackageJSON: true}
	want := "# Project Guide\n\n" +
		"## Overview\n\n" +
		"Language: JavaScript/TypeScript\n" +
		"\n## Build & Run\n\n" +
		"```bash\nnpm install\nnpm run build\nnpm test\n```\n" +
		"\n## Conventions\n\n" +
		"<!-- Add project conventions, coding style, and important notes here -->\n"
	if got := s.GenerateClaudeMD(); got != want {
		t.Errorf("GenerateClaudeMD()\ngot:  %q\nwant: %q", got, want)
	}
}

func TestGenerateClaudeMDGoModWinsOverPackageJSON(t *testing.T) {
	// Both manifests present: the switch must pick the Go commands, matching
	// detectLanguage's own precedence, or the guide would contradict Language.
	s := &State{Language: "Go", HasGoMod: true, HasPackageJSON: true}
	got := s.GenerateClaudeMD()
	if !containsStr(got, "go build ./...") {
		t.Errorf("GenerateClaudeMD() missing go commands:\n%s", got)
	}
	if containsStr(got, "npm install") {
		t.Errorf("GenerateClaudeMD() should not emit npm commands for a Go module:\n%s", got)
	}
}

func TestGenerateClaudeMDUnknownProject(t *testing.T) {
	s := &State{Language: "unknown"}
	want := "# Project Guide\n\n" +
		"## Overview\n\n" +
		"Language: unknown\n" +
		"\n## Build & Run\n\n" +
		"<!-- Add build commands here -->\n" +
		"\n## Conventions\n\n" +
		"<!-- Add project conventions, coding style, and important notes here -->\n"
	if got := s.GenerateClaudeMD(); got != want {
		t.Errorf("GenerateClaudeMD()\ngot:  %q\nwant: %q", got, want)
	}
}

func TestSummary(t *testing.T) {
	tests := []struct {
		name string
		s    *State
		want string
	}{
		{"git go no guide", &State{HasGit: true, Language: "Go"}, "git | Go | no CLAUDE.md"},
		{"git go with guide", &State{HasGit: true, Language: "Go", HasClaudeMD: true}, "git | Go | CLAUDE.md ✓"},
		{"no git", &State{Language: "Rust"}, "Rust | no CLAUDE.md"},
		{"blank language omitted", &State{HasGit: true}, "git | no CLAUDE.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.Summary(); got != tc.want {
				t.Errorf("Summary() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSummaryOfCheckedEmptyDir(t *testing.T) {
	// End-to-end: detectLanguage never returns "" so the language segment is
	// always present in a Summary built from a real Check.
	if got := Check(t.TempDir()).Summary(); got != "unknown | no CLAUDE.md" {
		t.Errorf("Summary() = %q, want %q", got, "unknown | no CLAUDE.md")
	}
}

func TestInitProjectCreatesClaudeMD(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/x\n")

	s := Check(dir)
	path, err := s.InitProject()
	if err != nil {
		t.Fatalf("InitProject() error = %v", err)
	}
	want := filepath.Join(dir, "CLAUDE.md")
	if path != want {
		t.Errorf("InitProject() path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("reading created file: %v", err)
	}
	if string(got) != s.GenerateClaudeMD() {
		t.Errorf("written content does not match GenerateClaudeMD()\ngot: %q", got)
	}
	if !containsStr(string(got), "go build ./...") {
		t.Errorf("created CLAUDE.md missing detected Go build commands:\n%s", got)
	}
}

func TestInitProjectIsIdempotentAndNeverClobbersUserEdits(t *testing.T) {
	// Regression test for the real failure mode: InitProject did not record
	// that it had created the file, so a second call on the same State
	// re-wrote CLAUDE.md and destroyed whatever the user had put there.
	isolateHome(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/x\n")

	s := Check(dir)
	path, err := s.InitProject()
	if err != nil {
		t.Fatalf("first InitProject() error = %v", err)
	}

	const userEdits = "# Project Guide\n\n手写的项目约定，不能被覆盖。\n"
	writeFile(t, path, userEdits)

	second, err := s.InitProject()
	if err != nil {
		t.Fatalf("second InitProject() error = %v", err)
	}
	if second != "" {
		t.Errorf("second InitProject() path = %q, want \"\" (documented: empty when it already exists)", second)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userEdits {
		t.Errorf("second InitProject() overwrote the user's CLAUDE.md\ngot:  %q\nwant: %q", got, userEdits)
	}
}

func TestInitProjectRefusesWhenFileAppearedAfterCheck(t *testing.T) {
	// Check() and InitProject() are separate steps, so the file can show up in
	// between (another cove window, a git pull). InitProject must not clobber
	// it just because its cached HasClaudeMD says it was absent.
	isolateHome(t)
	dir := t.TempDir()
	s := Check(dir)
	if s.HasClaudeMD {
		t.Fatal("precondition: expected HasClaudeMD false")
	}

	const existing = "# written by someone else\n"
	path := filepath.Join(dir, "CLAUDE.md")
	writeFile(t, path, existing)

	got, err := s.InitProject()
	if err != nil {
		t.Fatalf("InitProject() error = %v", err)
	}
	if got != "" {
		t.Errorf("InitProject() path = %q, want \"\"", got)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != existing {
		t.Errorf("InitProject() overwrote a file that appeared after Check()\ngot: %q", content)
	}
}

func TestInitProjectNoOpWhenCheckFoundExistingGuide(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	const existing = "# already here\n"
	writeFile(t, filepath.Join(dir, "CLAUDE.md"), existing)

	s := Check(dir)
	path, err := s.InitProject()
	if err != nil {
		t.Fatalf("InitProject() error = %v", err)
	}
	if path != "" {
		t.Errorf("InitProject() path = %q, want \"\"", path)
	}
	content, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != existing {
		t.Errorf("existing CLAUDE.md modified: got %q, want %q", content, existing)
	}
}

func TestInitProjectErrorsOnMissingDirectory(t *testing.T) {
	isolateHome(t)
	missing := filepath.Join(t.TempDir(), "no-such-dir", "nested")
	s := &State{ProjectDir: missing, Language: "unknown"}

	path, err := s.InitProject()
	if err == nil {
		t.Fatalf("InitProject() in a missing directory returned nil error (path=%q)", path)
	}
	if path != "" {
		t.Errorf("InitProject() path = %q on error, want \"\"", path)
	}
	if !containsStr(err.Error(), "CLAUDE.md") {
		t.Errorf("error message %q should mention CLAUDE.md", err)
	}
	if s.HasClaudeMD {
		t.Error("HasClaudeMD = true after a failed InitProject, want false")
	}
}

func TestInitProjectErrorsWhenTargetPathIsADirectory(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "CLAUDE.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := Check(dir)
	if s.HasClaudeMD {
		t.Fatal("precondition: a CLAUDE.md directory must not count as a guide")
	}

	path, err := s.InitProject()
	if err == nil {
		t.Fatalf("InitProject() returned nil error when CLAUDE.md is a directory (path=%q)", path)
	}
	if path != "" {
		t.Errorf("InitProject() path = %q on error, want \"\"", path)
	}
}

func containsStr(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
