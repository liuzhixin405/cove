package tool

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/shell"
)

// Nobody can type into a command the model runs. `git commit` without -m used
// to open an editor and sit there until the timeout killed it.
func TestBashGitDoesNotWaitForAnEditor(t *testing.T) {
	if shell.Default().Kind != shell.Bash {
		t.Skip("needs a bash shell")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// The user's shell environment decides; don't inherit a test runner's.
	for _, k := range []string{"GIT_EDITOR", "VISUAL", "EDITOR"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	dir := t.TempDir()
	setup := "git init -q && git config user.email t@example.test && git config user.name t && git config core.editor 'sleep 30 #' && echo x > f && git add f"
	if res, _ := NewBashTool().Call(context.Background(), Input{"command": setup}, Context{Cwd: dir}); strings.Contains(res.Data, "exit code") {
		t.Fatalf("setup failed: %s", res.Data)
	}

	start := time.Now()
	res, err := NewBashTool().Call(context.Background(), Input{"command": "git commit", "timeout": float64(15000)}, Context{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("git commit took %v (waiting for an editor): %s", elapsed, res.Data)
	}
	if !strings.Contains(res.Data, "exit code") {
		t.Fatalf("git commit without a message should fail, got: %s", res.Data)
	}
}
