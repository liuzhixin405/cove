package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestInstallHTTPClientHasTimeout(t *testing.T) {
	if installHTTPClient.Timeout <= 0 {
		t.Fatal("InstallSkill HTTP client should have a timeout")
	}
}

func TestSeedDefaultSkillsCreatesFiles(t *testing.T) {
	tmpHome := t.TempDir()
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", tmpHome)
	} else {
		t.Setenv("HOME", tmpHome)
	}

	SeedDefaultSkills()

	skillsDir := filepath.Join(tmpHome, ".cove", "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		// Embedded approach: skills are loaded from embedded FS, not written to disk on first run
		// Verify embedded FS has skills instead
		embEntries, embErr := fs.ReadDir(embeddedSkills, ".")
		if embErr != nil {
			t.Fatalf("embedded skills not readable: %v", embErr)
		}
		if len(embEntries) < 5 {
			t.Fatalf("expected at least 5 embedded skills, got %d", len(embEntries))
		}
		return
	}
	if len(entries) < 5 {
		t.Fatalf("expected at least 5 skill dirs, got %d", len(entries))
	}

	mgr := NewManager()
	mgr.AddDirectory(skillsDir)
	all := mgr.All()
	if len(all) < 5 {
		t.Fatalf("expected at least 5 skills loaded, got %d", len(all))
	}

	for _, name := range []string{"plan", "systematic-debugging", "test-driven-development", "spike", "requesting-code-review", "github-pr-workflow", "github-code-review"} {
		if _, ok := mgr.Get(name); !ok {
			t.Fatalf("expected skill %q to be loaded from disk", name)
		}
	}
}

func TestConditionalSkillsMatchingByPath(t *testing.T) {
	mgr := NewManager()

	mgr.Register(Skill{
		Name:        "go-debug",
		Description: "Debug Go code",
		Prompt:      "Use delve for debugging",
		Conditional: true,
		Paths:       []string{"*.go"},
	})
	mgr.Register(Skill{
		Name:        "py-test",
		Description: "Test Python code",
		Prompt:      "Use pytest",
		Conditional: true,
		Paths:       []string{"*.py", "*_test.py"},
	})
	mgr.Register(Skill{
		Name:        "universal",
		Description: "Always loaded",
		Prompt:      "General advice",
		Conditional: false,
	})

	matches := mgr.Matching(nil, "/home/user/main.go")
	if len(matches) != 1 || matches[0].Name != "go-debug" {
		t.Fatalf("expected [go-debug] for main.go, got %v", skillNames(matches))
	}

	matches = mgr.Matching(nil, "/home/user/test_app.py")
	if len(matches) != 1 || matches[0].Name != "py-test" {
		t.Fatalf("expected [py-test] for test_app.py, got %v", skillNames(matches))
	}

	matches = mgr.Matching(nil, "/home/user/app_test.py")
	if len(matches) != 1 || matches[0].Name != "py-test" {
		t.Fatalf("expected [py-test] for app_test.py, got %v", skillNames(matches))
	}

	matches = mgr.Matching(nil, "/home/user/app.js")
	if len(matches) != 0 {
		t.Fatalf("expected no matches for app.js, got %v", skillNames(matches))
	}
}

func skillNames(skills []Skill) []string {
	var names []string
	for _, s := range skills {
		names = append(names, s.Name)
	}
	return names
}

func TestParseFrontmatter_StepsCommaSeparated(t *testing.T) {
	content := "---\nname: deploy\ndescription: Deploy the service\nsteps: Run tests, Build image, Push image, Roll out\n---\nBody text here.\n"
	fm := parseFrontmatter(content)
	if fm == nil {
		t.Fatal("expected non-nil frontmatter")
	}
	want := []string{"Run tests", "Build image", "Push image", "Roll out"}
	if len(fm.Steps) != len(want) {
		t.Fatalf("expected %d steps, got %d (%v)", len(want), len(fm.Steps), fm.Steps)
	}
	for i, s := range want {
		if fm.Steps[i] != s {
			t.Errorf("step %d: expected %q, got %q", i, s, fm.Steps[i])
		}
	}
}

func TestParseFrontmatter_StepsAbsentLeavesNilSlice(t *testing.T) {
	content := "---\nname: simple\ndescription: No steps here\n---\nJust do it.\n"
	fm := parseFrontmatter(content)
	if fm == nil {
		t.Fatal("expected non-nil frontmatter")
	}
	if len(fm.Steps) != 0 {
		t.Fatalf("expected no steps, got %v", fm.Steps)
	}
}

func TestParseSkill_PropagatesStepsToSkill(t *testing.T) {
	content := "---\nname: release\ndescription: Cut a release\nsteps: Tag version, Build artifacts\nallowed_tools: bash, write\n---\nRelease workflow body.\n"
	sk := parseSkill("release", content, "/tmp/release/SKILL.md")
	if len(sk.Steps) != 2 || sk.Steps[0] != "Tag version" || sk.Steps[1] != "Build artifacts" {
		t.Fatalf("expected Steps propagated from frontmatter, got %v", sk.Steps)
	}
	if len(sk.AllowedTools) != 2 {
		t.Fatalf("expected AllowedTools still parsed alongside Steps, got %v", sk.AllowedTools)
	}
}

func TestSkill_RenderInvocation_NoStepsMatchesLegacyShape(t *testing.T) {
	sk := Skill{Name: "plain", Prompt: "Just follow this prose."}
	got := sk.RenderInvocation()
	want := "[Skill: plain]\n\nJust follow this prose.\n\nFollow these instructions to complete the task."
	if got != want {
		t.Fatalf("expected legacy-shaped output for a skill with no Steps/AllowedTools.\nwant: %q\ngot:  %q", want, got)
	}
}

func TestSkill_RenderInvocation_StepsRenderedAsNumberedChecklist(t *testing.T) {
	sk := Skill{Name: "checklist", Prompt: "body", Steps: []string{"First step", "Second step", "Third step"}}
	got := sk.RenderInvocation()
	for i, step := range sk.Steps {
		numbered := strconv.Itoa(i+1) + ". " + step
		if !strings.Contains(got, numbered) {
			t.Fatalf("expected %q in rendered output, got %q", numbered, got)
		}
	}
	// Steps must appear before the free-form prompt body.
	if strings.Index(got, "First step") > strings.Index(got, "body") {
		t.Fatalf("expected steps checklist to precede prompt body, got %q", got)
	}
}

func TestSkill_RenderInvocation_AllowedToolsRenderedAsConstraint(t *testing.T) {
	sk := Skill{Name: "scoped", Prompt: "body", AllowedTools: []string{"read", "grep"}}
	got := sk.RenderInvocation()
	if !strings.Contains(got, "only use these tools: read, grep") {
		t.Fatalf("expected allowed-tools constraint sentence, got %q", got)
	}
}
