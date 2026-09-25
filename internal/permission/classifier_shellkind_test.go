package permission

import "testing"

// The read-only shortcut runs a line without asking, so it must not trust a
// quote the real shell does not honour: cmd.exe treats ' as an ordinary
// character, bash reads \" outside quotes as a literal quote, and PowerShell
// evaluates $var.Method(...) in an argument.
func TestReadOnlyLineRespectsShellQuoting(t *testing.T) {
	c := NewClassifier()
	cases := []struct {
		cmd  string
		kind ShellKind
		want bool
	}{
		{`echo 'a & del x'`, ShellPOSIX, true},
		{`echo 'a & del x'`, ShellPowerShell, true},
		{`echo 'a & del x'`, ShellCmd, false},
		{`echo 'a & del x'`, "", false},
		{`grep -rn "foo(" .`, ShellPOSIX, true},
		{`grep -rn "foo(" .`, ShellCmd, false},
		{`echo \"; rm x; \"`, ShellPOSIX, false},
		{`echo "a\"; rm x; \"b"`, ShellPOSIX, false},
		{`echo $ExecutionContext.InvokeProvider.Item.Remove('ls')`, ShellPowerShell, false},
		{`Get-ChildItem $HOME`, ShellPowerShell, false},
		{`ls $HOME`, ShellPOSIX, true},
		{`git status`, ShellCmd, false},
		{`git log --format='%an'`, ShellCmd, false},
		{`git status`, "", true},
	}
	for _, tc := range cases {
		if got := c.IsReadOnlyLineFor(tc.cmd, tc.kind); got != tc.want {
			t.Errorf("IsReadOnlyLineFor(%q, %q) = %v, want %v", tc.cmd, tc.kind, got, tc.want)
		}
	}
	if c.AutoApproveLineFor(`go test -run 'a|b' ./...`, ShellCmd) {
		t.Error("cmd: quoted operator in a build line auto-approved")
	}
	if !c.AutoApproveLineFor(`go test -run 'a|b' ./...`, ShellPOSIX) {
		t.Error("posix: quoted operator in a build line should auto-approve")
	}
}

// PowerShell evaluates $var.Method(args) in argument position, so an unquoted
// $ is refused by prefix rules under PowerShell too.
func TestPrefixRuleRefusesPowerShellVariablesInArguments(t *testing.T) {
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "git commit"})
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "ls"})
	if d, _ := m.Check("powershell", map[string]any{"command": `git commit -m $ExecutionContext.InvokeProvider.Item.Remove('ls')`}, DAsk); d != DAsk {
		t.Errorf("powershell $var.Method() argument = %v, want ask", d)
	}
	if d, _ := m.Check("powershell", map[string]any{"command": `git commit -m "costs $5"`}, DAsk); d != DAllow {
		t.Errorf("powershell quoted $ = %v, want allow", d)
	}
}

// When the bash tool falls back to cmd.exe (no Git Bash, no PowerShell), the
// tokenizer cannot model cmd's ^ escapes and %VAR% expansion, so nothing runs
// unasked as "read-only" there: dir, type and git status all ask.
func TestCmdFallbackNeverReadOnly(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{"dir", "type README.md", "git status", "echo hi"} {
		if c.IsReadOnlyLineFor(cmd, ShellCmd) {
			t.Errorf("IsReadOnlyLineFor(%q, ShellCmd) = true, want false", cmd)
		}
	}
	if !c.IsReadOnlyLineFor("git status", ShellPOSIX) || !c.IsReadOnlyLineFor("dir", ShellPowerShell) {
		t.Error("read-only lines under bash / PowerShell must still be read-only")
	}
}

// Auto mode's build/read-only auto-approval does not apply under cmd either.
func TestCmdFallbackNeverAutoApproves(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{"dir", "type README.md", "git status", "go test ./...", "go build ./..."} {
		if c.AutoApproveLineFor(cmd, ShellCmd) {
			t.Errorf("AutoApproveLineFor(%q, ShellCmd) = true, want false", cmd)
		}
		if !c.AutoApproveLineFor(cmd, ShellPOSIX) && cmd != "dir" && cmd != "type README.md" {
			t.Errorf("AutoApproveLineFor(%q, ShellPOSIX) = false, want true", cmd)
		}
	}
}
