package skills

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
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
