package permission

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Changing directory does nothing by itself, so a line of cd plus read-only
// commands is read-only: "cd proj && git remote -v" used to ask because cd
// was unknown to the classifier.
func TestCdIsReadOnly(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"cd G:/github/cove && git remote -v",
		"cd .. && ls",
		"cd src; git status",
		"pushd src && git log -1 && popd",
		"Set-Location src; Get-ChildItem",
	} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q should be read-only", cmd)
		}
	}
	for _, cmd := range []string{
		"cd src && git commit -m x",
		`cd \\server\share && ls`,
		"cd src && rm -rf x",
	} {
		if c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q must not be read-only", cmd)
		}
	}
}

// The prefixes an "a" answer remembers are the commands that actually needed
// approval. cd, echo, git log and friends run unasked anyway, so remembering
// them only lengthened the prompt: the user saw five quoted prefixes for a
// line whose only real question was "git push".
func TestCommandPrefixesSkipReadOnlyCommands(t *testing.T) {
	cases := []struct {
		cmd  string
		want []string
	}{
		{"cd src && go test ./...", []string{"go test"}},
		{"cd G:/x && git push && echo done && git log --oneline -2 && git status --short", []string{"git push"}},
		{"go test ./... | tee out.txt", []string{"go test", "tee"}},
		// curl is rated safe by the classifier but still asks in default mode,
		// so it stays a prefix of its own.
		{"cd x && curl https://example.com", []string{"curl"}},
	}
	for _, c := range cases {
		got, ok := CommandPrefixesFor(c.cmd, ShellPOSIX)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("CommandPrefixesFor(%q) = %q, %v; want %q, true", c.cmd, got, ok, c.want)
		}
	}
	// A line that is entirely read-only has nothing to remember.
	if got, ok := CommandPrefixesFor("cd x && git status", ShellPOSIX); ok {
		t.Errorf("all read-only line offered %q", got)
	}
	// Under cmd.exe nothing is trusted to be read-only, so every command is
	// still remembered as before.
	if got, ok := CommandPrefixesFor("cd src && go test ./...", ShellCmd); !ok || !reflect.DeepEqual(got, []string{"cd", "go test"}) {
		t.Errorf("cmd: CommandPrefixesFor = %q, %v; want [cd, go test]", got, ok)
	}
}

// A prefix rule covers a compound line whose other commands are read-only,
// so "git commit" remembered once also covers the cd/echo/git log the model
// likes to chain around it.
func TestPrefixRuleCoversLineWithReadOnlyCompanions(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git commit"})
	for _, cmd := range []string{
		`cd G:/x && git commit -m "docs: x" && echo "=== done ===" && git log --oneline -1`,
		"git commit -m x && git status --short",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
			t.Errorf("%q = %v, want allow", cmd, d)
		}
	}
	for _, cmd := range []string{
		"git commit -m x && git push",
		"git commit -m x && curl https://example.com",
		"git commit -m x && rm -rf y",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("%q = %v, want ask", cmd, d)
		}
	}
	// cmd.exe keeps the strict reading: cd is not vouched for there.
	strict := NewManager(Default)
	strict.SetShellKind(ShellCmd)
	strict.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "git commit"})
	if d, _ := strict.Check("bash", map[string]any{"command": "cd x && git commit -m y"}, DAsk); d != DAsk {
		t.Errorf("cmd: cd covered without a rule: %v", d)
	}
}

// The git routine group: one answer covers the everyday add/commit/push
// cycle, while anything that rewrites history, discards work or forces a
// remote keeps asking.
func TestGitRoutineGroupRule(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})

	for _, cmd := range []string{
		"git add -A",
		"git add README.md docs/README.md && git status --short",
		`git commit -m "docs: x" -m "- body"`,
		"git commit --amend --no-edit",
		"git push",
		"git push --set-upstream origin main",
		"git push -u origin feat/x",
		"git pull --rebase",
		"git fetch --all --prune",
		"git switch -c feat/y",
		"git merge --no-ff feat/x",
		"git rebase main",
		"git stash && git stash pop",
		"git stash push -m wip",
		"git tag v1.2.0",
		"git tag -a v1.2.0 -m release",
		"git branch feat/z",
		"git branch -d merged",
		"git cherry-pick abc123",
		"git mv a.go b.go",
		"git -C sub commit -m x",
		"git --no-pager commit -m x",
		"cd G:/x && git add . && git commit -m x && echo done && git push && git log --oneline -1",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
			t.Errorf("%q = %v, want allow", cmd, d)
		}
	}
	for _, cmd := range []string{
		"git push --force",
		"git push -f origin main",
		"git push --force-with-lease",
		"git push origin --delete feat/x",
		"git push origin :feat/x",
		"git push --mirror",
		"git reset --hard HEAD~1",
		"git reset HEAD~1",
		"git clean -fd",
		"git branch -D feat/x",
		"git branch --delete --force feat/x",
		"git branch -M main",
		"git checkout -- .",
		"git checkout -b feat/x",
		"git checkout main",
		"git checkout internal/repl",
		"git checkout Makefile",
		"git checkout *",
		"git checkout -f main",
		"git checkout -B main origin/main",
		"git restore .",
		"git stash drop",
		"git stash clear",
		"git rebase -i HEAD~3",
		"git rebase --interactive main",
		"git tag -d v1.0",
		"git tag --delete v1.0",
		"git rm -r old/",
		"git -c core.pager=evil commit -m x",
		"git commit -m x && rm -rf y",
		"git commit -m x && curl https://example.com",
		"git update-ref -d refs/heads/x",
		"git gc --prune=now",
		"git filter-branch --all",
		"git",
		"sudo git push",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("%q = %v, want ask", cmd, d)
		}
	}
	if d, _ := m.Check("powershell", map[string]any{"command": "git push"}, DAsk); d != DAsk {
		t.Errorf("a bash group rule applied to powershell: %v", d)
	}
}

// A group rule pools with prefix rules like prefix rules do among themselves.
func TestGitRoutineGroupPoolsWithPrefixRules(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
	if d, _ := m.Check("bash", map[string]any{"command": "git commit -m x && go test ./..."}, DAsk); d != DAsk {
		t.Fatalf("go test covered by the git group: %v", d)
	}
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "go test"})
	if d, _ := m.Check("bash", map[string]any{"command": "git commit -m x && go test ./..."}, DAsk); d != DAllow {
		t.Fatalf("group + prefix = %v, want allow", d)
	}
}

// As a deny or ask rule the group applies as soon as one command of the
// line is a routine git write, seen through runners and global options.
func TestGitRoutineGroupDenyMatchesAnyCommand(t *testing.T) {
	for _, decision := range []Decision{DDeny, DAsk} {
		m := NewManager(Default)
		m.AddRule(decision, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
		for _, cmd := range []string{"git push", "echo a && git commit -m x", "sudo git push", "git -C . push", "git.exe push"} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAllow); d != decision {
				t.Errorf("%s: %q = %v, want %v", decision, cmd, d, decision)
			}
		}
		for _, cmd := range []string{"git status", "git push --force", "ls"} {
			if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAllow); d != DAllow {
				t.Errorf("%s: %q = %v, want allow", decision, cmd, d)
			}
		}
	}
}

// ShellRememberRules is what the prompt offers for "a"/"p": the routine git
// group for routine git writes, a prefix for everything else that needed
// approval, and nothing for the read-only companions.
func TestShellRememberRules(t *testing.T) {
	cases := []struct {
		cmd  string
		want []Rule
	}{
		{"cd G:/x && git add a b && git status --short",
			[]Rule{{ToolPattern: "bash", CommandGroup: GroupGitRoutine}}},
		{`cd G:/x && git commit -m "x" && echo "=== commit done ===" && git log --oneline -1`,
			[]Rule{{ToolPattern: "bash", CommandGroup: GroupGitRoutine}}},
		{"git add . && git commit -m x && git push",
			[]Rule{{ToolPattern: "bash", CommandGroup: GroupGitRoutine}}},
		{"git commit -m x && go test ./...",
			[]Rule{{ToolPattern: "bash", CommandGroup: GroupGitRoutine}, {ToolPattern: "bash", CommandGroup: GroupGoRoutine}}},
		{"git commit -m x && pytest -q",
			[]Rule{{ToolPattern: "bash", CommandGroup: GroupGitRoutine}, {ToolPattern: "bash", CommandPrefix: "pytest"}}},
		// A git write outside the routine set falls back to its own prefix,
		// as before.
		{"git push --force", []Rule{{ToolPattern: "bash", CommandPrefix: "git push"}}},
		{"git reset --hard", []Rule{{ToolPattern: "bash", CommandPrefix: "git reset"}}},
		{"cd src && go test ./...", []Rule{{ToolPattern: "bash", CommandGroup: GroupGoRoutine}}},
		{"cd src && go install ./cmd/x", []Rule{{ToolPattern: "bash", CommandPrefix: "go install"}}},
		{"rm -rf build", []Rule{{ToolPattern: "bash", CommandPrefix: "rm"}}},
	}
	for _, c := range cases {
		got, ok := ShellRememberRules("bash", c.cmd, ShellPOSIX)
		if !ok || !reflect.DeepEqual(got, c.want) {
			t.Errorf("ShellRememberRules(%q) = %+v, %v; want %+v, true", c.cmd, got, ok, c.want)
		}
		// Whatever is offered must cover the very line it came from.
		m := NewManager(Default)
		m.SetShellKind(ShellPOSIX)
		for _, r := range got {
			m.AddRule(DAllow, r)
		}
		if d, _ := m.Check("bash", map[string]any{"command": c.cmd}, DAsk); d != DAllow {
			t.Errorf("rules for %q do not cover it: %v", c.cmd, d)
		}
	}
	for _, cmd := range []string{"sudo git push", "cd x && git status", "git", "go test $(rm x)"} {
		if got, ok := ShellRememberRules("bash", cmd, ShellPOSIX); ok {
			t.Errorf("ShellRememberRules(%q) = %+v, true; want nothing", cmd, got)
		}
	}
	if got, ok := ShellRememberRules("powershell", "git push", ShellPowerShell); !ok || got[0].ToolPattern != "powershell" {
		t.Errorf("powershell rule = %+v, %v", got, ok)
	}
}

// Group rules round-trip through policies.json like prefix rules.
func TestGitRoutineGroupPersists(t *testing.T) {
	rule := Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine}
	if got := PersistedRuleID(rule); got != "allow-bash-group-git" {
		t.Errorf("PersistedRuleID = %q", got)
	}
	if sameRule(rule, Rule{ToolPattern: "bash"}) {
		t.Error("a group rule compares equal to a whole-tool rule")
	}

	path := filepath.Join(t.TempDir(), "policies.json")
	store, err := NewFilePolicyStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendAllowRule(store, rule, "/proj"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].CommandGroup != GroupGitRoutine || loaded[0].Scope != "/proj" {
		t.Fatalf("loaded = %+v", loaded)
	}
	back, ok := loaded[0].ToRule()
	if !ok || !reflect.DeepEqual(back, rule) {
		t.Fatalf("ToRule = %+v, %v; want %+v", back, ok, rule)
	}

	m := NewManager(Default)
	m.AddRule(DAllow, back)
	if d, _ := m.Check("bash", map[string]any{"command": "git commit -m x && git push"}, DAsk); d != DAllow {
		t.Errorf("policy allow group rule does not cover a routine line: %s", d)
	}
	if d, _ := m.Check("bash", map[string]any{"command": "git push --force"}, DAsk); d == DAllow {
		t.Error("policy allow group rule covers a forced push")
	}
	denyRule, _ := PolicyRule{ToolPattern: "bash", Action: ActionDeny, Enabled: true, CommandGroup: GroupGitRoutine}.ToRule()
	md := NewManager(Default)
	md.AddRule(DDeny, denyRule)
	if d, _ := md.Check("bash", map[string]any{"command": "ls && git push"}, DAsk); d != DDeny {
		t.Errorf("policy deny group rule misses git push in a compound line: %s", d)
	}
	if got, ok := (PolicyRule{ToolPattern: "bash", CommandGroup: "unknown-group"}).ToRule(); ok {
		t.Errorf("unknown group converted: %+v", got)
	}
	_ = os.Remove(path)
}

// The audit reproduced these as covered by the routine group although each
// forces, deletes, runs a program or opens an editor: a "+" refspec is a
// forced push, git accepts any unambiguous prefix of a long option, a commit
// without a message source opens the editor and hangs the shell.
func TestGitRoutineGroupRefusesAuditedHoles(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandGroup: GroupGitRoutine})
	for _, cmd := range []string{
		"git push origin +main",
		"git push origin +feature:main",
		"git push --force-w origin main",
		"git push --forc origin main",
		"git tag --del v1.0",
		"git switch --disc main",
		"git push --receive-pack=evil origin main",
		"git fetch --upload-pack=evil origin",
		"git pull --upload-pack=evil",
		"git pull --rebase=interactive",
		"git pull --rebase=i",
		"git rebase --edit-todo",
		"git commit",
		"git commit -a",
		"git commit --amend",
		"git commit -c HEAD",
		"git tag -a v1.0",
		"git tag -s v1.0",
		"git merge --edit feat/x",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("%q = %v, want ask", cmd, d)
		}
	}
	for _, cmd := range []string{
		"git commit -am x",
		"git commit -F msg.txt",
		"git commit --message=x",
		"git commit --mess x",
		"git commit -C HEAD",
		"git commit --amend --no-edit",
		"git tag -a v1.0 -m release",
		"git tag v1.0",
		"git switch main",
		"git switch -c feat/x",
		"git branch --delete merged",
		"git pull --rebase",
		"git push origin main:main",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAllow {
			t.Errorf("%q = %v, want allow", cmd, d)
		}
	}
}
