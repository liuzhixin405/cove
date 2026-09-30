package permission

import "testing"

// gitBranchCategory compared whole words against a list of writing options,
// so an attached value (-uorigin/main) or one of git's abbreviated long
// options (--set-u=, --unset-up) was CatSafe although it writes .git/config.
// Only known listing options keep git branch a read now.
func TestGitBranchWritesAreNotSafe(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"git branch -uorigin/main",
		"git branch -u origin/main",
		"git branch --set-u=origin/main",
		"git branch --set-upstream-to=origin/main",
		"git branch --unset-up",
		"git branch --unset-upstream",
		"git branch --edit-desc",
		"git branch --del x",
		"git branch -dx",
		"git branch -Dfx",
		"git branch -avD x",
		"git branch --cop a b",
		"git branch --list --delete x",
		"git branch -l -m a b",
		"git branch --track x origin/x",
		"git branch --no-t x",
		"git branch --create-reflog x",
		"git branch --recurse-submodules x",
		"git branch newname",
		"git branch --contains HEAD newname",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got == CatSafe {
			t.Errorf("ClassifyLineFor(%q) = CatSafe, want not safe", cmd)
		}
	}
	for _, cmd := range []string{
		"git branch",
		"git branch -a",
		"git branch -avv",
		"git branch -r",
		"git branch --show-current",
		"git branch --list 'feat/*'",
		"git branch -l feat/x",
		"git branch --contains HEAD",
		"git branch --merged main",
		"git branch --no-merged",
		"git branch --sort=-committerdate '--format=%(refname:short)'",
		"git branch --points-at HEAD",
		"git branch --color=always -a",
		"git branch -q --list",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatSafe {
			t.Errorf("ClassifyLineFor(%q) = %v, want CatSafe", cmd, got)
		}
	}
}
