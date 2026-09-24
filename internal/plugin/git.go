package plugin

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/liuzhixin405/cove/internal/shell"
)

// gitCommand builds a git invocation for plugin and marketplace work.
//
// git must never wait for input here. A private or mistyped HTTPS repository
// answers 401, and git then asked for a username on the terminal cove's UI
// owns, or the Windows credential manager opened a login window, and the
// install hung. shell.Env turns off git's terminal prompt; GCM_INTERACTIVE
// does the same for Git Credential Manager. Credentials that are already
// stored still work.
func gitCommand(args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Env = append(shell.Env(os.Environ()), "GCM_INTERACTIVE=never")
	return cmd
}

// cloneRepo shallow-clones url into dir.
//
// core.symlinks=false is written into the new repository before checkout (and
// so also applies to later pulls): a plugin is untrusted content whose skills
// and commands are read into the model's prompt, and a symlink named
// skills/x/SKILL.md pointing at ~/.ssh/id_rsa would have that file read and
// sent to the provider. Links are checked out as plain files holding the link
// text instead. "--" keeps a URL starting with "-" from being read as an option.
func cloneRepo(url, dir string) error {
	cmd := gitCommand("clone", "--depth=1", "--quiet", "--config", "core.symlinks=false", "--", url, dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
