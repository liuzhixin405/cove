package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeProjSkill(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + body + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A project skill replaces a user skill of the same name (the closer
// definition wins), and the inner project directory wins over the outer.
func TestProjectSkillReplacesUserSkillAndInnerWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writeProjSkill(t, filepath.Join(home, ".cove", "skills"), "commit", "user commit")

	proj := t.TempDir()
	if err := os.Mkdir(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeProjSkill(t, filepath.Join(proj, ".cove", "skills"), "commit", "project commit")
	writeProjSkill(t, filepath.Join(proj, ".cove", "skills"), "deploy", "outer deploy")
	sub := filepath.Join(proj, "svc")
	writeProjSkill(t, filepath.Join(sub, ".cove", "skills"), "deploy", "inner deploy")

	m := NewManager()
	LoadAll(m, sub)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if got := m.skills["commit"]; got.Source != SourceProject {
		t.Fatalf("commit source = %s, want the project's", got.Source)
	}
	if got := m.skills["deploy"]; !strings.Contains(got.Prompt, "inner deploy") {
		t.Fatalf("deploy = %q, want the directory closer to cwd", got.Prompt)
	}
}

// A project skill directory that is a junction out of the project is not
// read: its SKILL.md would have gone into the system prompt.
func TestProjectSkillThroughJunctionIsRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	outside := t.TempDir()
	writeProjSkill(t, outside, "secret", "SECRET-OUTSIDE")

	proj := t.TempDir()
	skillsDir := filepath.Join(proj, ".cove", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(skillsDir, "secret")
	target := filepath.Join(outside, "secret")
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("cmd", "/c", "mklink", "/J", link, target).Run()
	} else {
		err = os.Symlink(target, link)
	}
	if err != nil {
		t.Skipf("cannot create a link here: %v", err)
	}
	m := NewManager()
	LoadAll(m, proj)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if sk, ok := m.skills["secret"]; ok {
		t.Fatalf("a skill linked in from outside the project was loaded: %q", sk.Prompt)
	}
}
