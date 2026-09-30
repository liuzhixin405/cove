package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Colour escapes cost tokens and confuse the model; every tool that honours
// one of the common switches is told to write plain text.
func TestEnvDisablesColor(t *testing.T) {
	env := Env([]string{"NO_COLOR=", "TERM=xterm-256color", "FORCE_COLOR=1"})
	last := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			last[k] = v // a later entry wins, as it does for the child process
		}
	}
	want := map[string]string{"NO_COLOR": "1", "TERM": "dumb", "CLICOLOR": "0", "FORCE_COLOR": "0"}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("%s = %q, want %q", k, last[k], v)
		}
	}
}

func TestEnvKeepsBase(t *testing.T) {
	env := Env([]string{"PATH=/bin"})
	if env[0] != "PATH=/bin" {
		t.Fatalf("Env dropped the base environment: %q", env)
	}
}

func lastEnv(env []string) map[string]string {
	last := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			last[strings.ToUpper(k)] = v
		}
	}
	return last
}

// A repository's .git/config can name a core.fsmonitor program that git
// status runs; git commands run unasked as read-only, so the child
// environment turns fsmonitor off and keeps git from paging. A bare
// repository shipped inside a project is only used when named explicitly.
func TestEnvHardensGit(t *testing.T) {
	last := lastEnv(Env([]string{"PATH=/bin"}))
	want := map[string]string{"GIT_PAGER": "cat", "GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "core.fsmonitor", "GIT_CONFIG_VALUE_0": "false",
		"GIT_CONFIG_KEY_1": "safe.bareRepository", "GIT_CONFIG_VALUE_1": "explicit"}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("%s = %q, want %q", k, last[k], v)
		}
	}
}

// Config the user already passes through GIT_CONFIG_COUNT is kept: ours goes
// after it.
func TestEnvAppendsAfterExistingGitConfigCount(t *testing.T) {
	base := []string{"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=me",
		"GIT_CONFIG_KEY_1=core.autocrlf", "GIT_CONFIG_VALUE_1=false"}
	last := lastEnv(Env(base))
	want := map[string]string{
		"GIT_CONFIG_COUNT": "4", "GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "me",
		"GIT_CONFIG_KEY_1": "core.autocrlf", "GIT_CONFIG_KEY_2": "core.fsmonitor", "GIT_CONFIG_VALUE_2": "false",
		"GIT_CONFIG_KEY_3": "safe.bareRepository", "GIT_CONFIG_VALUE_3": "explicit",
	}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("%s = %q, want %q", k, last[k], v)
		}
	}
	// Windows environment names are case-insensitive.
	if runtime.GOOS == "windows" {
		last = lastEnv(Env([]string{"git_config_count=1", "GIT_CONFIG_KEY_0=a.b", "GIT_CONFIG_VALUE_0=c"}))
		if last["GIT_CONFIG_COUNT"] != "3" || last["GIT_CONFIG_KEY_1"] != "core.fsmonitor" || last["GIT_CONFIG_KEY_0"] != "a.b" {
			t.Errorf("lower-case count not honoured: %v", last)
		}
	}
	// A count git would reject anyway is replaced rather than extended.
	last = lastEnv(Env([]string{"GIT_CONFIG_COUNT=x"}))
	if last["GIT_CONFIG_COUNT"] != "2" || last["GIT_CONFIG_KEY_0"] != "core.fsmonitor" {
		t.Errorf("bogus count: %v", last)
	}
}

// A bare repository shipped in a project directory is not picked up by a
// plain "git status" run there: its config (a pager, diff.external...) would
// have applied to a command that runs unasked.
func TestEnvRefusesImplicitBareRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	bare := filepath.Join(dir, "shipped")
	if out, err := exec.Command("git", "init", "-q", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = bare
	cmd.Env = Env(os.Environ())
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("git used the shipped bare repository: %s", out)
	}
}
