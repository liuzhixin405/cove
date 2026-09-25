package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPathInside(t *testing.T) {
	root := t.TempDir()
	for target, want := range map[string]bool{
		"a.go":                                 true,
		"sub/b.go":                             true,
		"./c.go":                               true,
		"sub/../d.go":                          true,
		"../escape.go":                         false,
		"sub/../../escape.go":                  false,
		filepath.Join(root, "x", "y.go"):       true,
		filepath.Join(t.TempDir(), "other.go"): false,
		"":                                     false,
	} {
		if got := PathInside(root, target); got != want {
			t.Errorf("PathInside(%q, %q) = %v, want %v", root, target, got, want)
		}
	}
	if PathInside("", "a.go") {
		t.Error("an empty root must contain nothing")
	}
}

func TestProjectRootFindsGitDirWithoutRunningGit(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(sub); !SameProject(got, repo) {
		t.Errorf("ProjectRoot(%q) = %q, want the git root %q", sub, got, repo)
	}
	plain := t.TempDir()
	if got := ProjectRoot(plain); !SameProject(got, plain) {
		t.Errorf("ProjectRoot(%q) = %q, want the directory itself", plain, got)
	}
}

func TestPersistedRuleID(t *testing.T) {
	for want, r := range map[string]Rule{
		"allow-bash-git commit":         {ToolPattern: "bash", CommandPrefix: "git commit"},
		"allow-write":                   {ToolPattern: "write"},
		"allow-mcp-github-create_issue": {ToolPattern: "mcp", InputEquals: map[string]string{"toolName": "create_issue", "serverName": "github"}},
	} {
		if got := PersistedRuleID(r); got != want {
			t.Errorf("PersistedRuleID(%+v) = %q, want %q", r, got, want)
		}
	}
}

func TestAppendAllowRuleLeavesAnUnreadableFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, _ := NewFilePolicyStorage(path)
	if err := AppendAllowRule(store, Rule{ToolPattern: "bash", CommandPrefix: "go test"}, ""); err == nil {
		t.Fatal("AppendAllowRule overwrote a file it could not parse")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{not json" {
		t.Errorf("file changed to %q", data)
	}
}
