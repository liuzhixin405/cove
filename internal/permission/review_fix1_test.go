package permission

import "testing"

// Review fix round 1: commands that must not be auto-allowed as read-only.

func TestGitGrepPagerOptionCannotBeSmuggled(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"git grep -nO foo",
		"git grep -iO foo",
		"git grep --open foo",
		"git grep --open-files foo",
		"git grep -O'cmd' foo",
		"git grep -Ocmd foo",
		"git grep --open-files-in-pager=vim foo",
		"git grep --no-index foo",
		"git grep --weird foo",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got == CatSafe {
			t.Errorf("ClassifyLineFor(%q) = CatSafe, want not safe", cmd)
		}
	}
	for _, cmd := range []string{
		"git grep -n foo",
		"git grep -n -i foo -- src",
		"git grep -nw foo",
		"git grep -e foo -e bar",
		"git grep --count --ignore-case foo",
		"git grep -l foo HEAD~1",
	} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatSafe {
			t.Errorf("ClassifyLineFor(%q) = %v, want CatSafe", cmd, got)
		}
	}
}

func TestPowerShellScriptblocksAreNotReadOnly(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"gci | where { rm $_ }",
		"ls | % { del $_ }",
		"ls | ? { $_.Delete() }",
		"ls | foreach { del $_ }",
		"gci | where Name -eq x",
		"Get-ChildItem | Where-Object { Remove-Item $_ }",
		"gci {x}",
	} {
		if c.IsReadOnlyLineFor(cmd, ShellPowerShell) {
			t.Errorf("IsReadOnlyLineFor(%q, powershell) = true, want false", cmd)
		}
	}
	if !c.IsReadOnlyLineFor("which go", ShellPOSIX) {
		t.Error("which must stay read-only under POSIX")
	}
	if !c.IsReadOnlyLineFor(`Get-Content 'a{b}.txt'`, ShellPowerShell) {
		t.Error("a quoted brace is an argument, not a scriptblock")
	}

	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "gci"})
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "where"})
	if d, _ := m.Check("powershell", map[string]any{"command": "gci | where { rm $_ }"}, DAsk); d != DAsk {
		t.Errorf("prefix rules covered a PowerShell scriptblock: %v", d)
	}
}

func TestRedirectToFileWithAmpersandIsAWrite(t *testing.T) {
	c := NewClassifier()
	if c.IsReadOnlyLineFor("cat a >&b", ShellPOSIX) {
		t.Error("cat a >&b writes b and must not be read-only")
	}
	for _, cmd := range []string{"cat a 2>&1", "cat a >&2", "cat a 2>&-"} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q duplicates a descriptor and should stay read-only", cmd)
		}
	}
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "cat"})
	if d, _ := m.Check("bash", map[string]any{"command": "cat a >&b"}, DAsk); d != DAsk {
		t.Errorf("prefix rule covered cat a >&b: %v", d)
	}
	if d, _ := m.Check("bash", map[string]any{"command": "cat a 2>&1"}, DAsk); d != DAllow {
		t.Errorf("prefix rule refused cat a 2>&1: %v", d)
	}
}

func TestTreeWritingOptionsAreNotReadOnly(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{"tree -R", "tree -o out.txt", "tree -H . ", "tree -a -R -L 2"} {
		if c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q) = true, want false", cmd)
		}
	}
	if !c.IsReadOnlyLineFor("tree -L 2", ShellPOSIX) {
		t.Error("tree -L 2 should be read-only")
	}
}

// ${ is refused by prefix rules just as by the classifier.
func TestBraceExpansionRefusedByPrefixRules(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "echo"})
	if d, _ := m.Check("bash", map[string]any{"command": "echo ${HOME}"}, DAsk); d != DAsk {
		t.Errorf("prefix rule covered ${...}: %v", d)
	}
	if _, ok := CommandPrefixesFor("echo ${HOME}", ShellPOSIX); ok {
		t.Error("CommandPrefixesFor offered a prefix for ${...}")
	}
}
