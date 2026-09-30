package permission

import "testing"

// Under bash a backslash-newline continues the line and a backslash inside a
// word only quotes the next character. The classifier read the line
// literally, so "find . -f\<NL>ls out.txt" was a find plus an ls (bash runs
// find -fls out.txt, which writes a file) and "git log --output\<NL> echo" a
// git log plus an echo (bash writes the log to ./echo).
func TestBashBackslashesAreNotReadOnly(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		"find . -f\\\nls out.txt",
		"git log --output\\\n echo",
		`find . -f\ls out.txt`,
		`git log --out\put=x`,
		`r\m -rf build`,
	} {
		for _, kind := range []ShellKind{ShellPOSIX, ""} {
			if c.IsReadOnlyLineFor(cmd, kind) {
				t.Errorf("IsReadOnlyLineFor(%q, %q) = true, want false", cmd, kind)
			}
			if c.AutoApproveLineFor(cmd, kind) {
				t.Errorf("AutoApproveLineFor(%q, %q) = true, want false", cmd, kind)
			}
		}
	}
	// A line continuation between two harmless words stays read-only.
	for _, cmd := range []string{"git log \\\n  --oneline -5", `cat \x`, "ls -la \\\n src"} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q, posix) = false, want true", cmd)
		}
	}
	// Under PowerShell a backslash is a path character.
	if !c.IsReadOnlyLineFor(`Get-Content C:\proj\a.txt`, ShellPowerShell) {
		t.Error(`Get-Content C:\proj\a.txt under PowerShell should stay read-only`)
	}
}

// The hard block is the only guard in bypass mode; it must read the line as
// bash does.
func TestBashBackslashesHardBlocked(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{"rm \\\n-rf ~", `r\m -rf /`, `rm -rf /\u\s\r`} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatDangerous {
			t.Errorf("ClassifyLineFor(%q) = %v, want CatDangerous", cmd, got)
		}
	}
}

// A remembered prefix rule and the read-only companions it tolerates are
// judged on bash's reading of the line too.
func TestPrefixRuleSeesBashBackslashes(t *testing.T) {
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "go test"})
	for _, cmd := range []string{
		"go test ./... && find . -f\\\nls out.txt",
		"go test ./... && git log --output\\\n echo",
	} {
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("%q = %v, want ask", cmd, d)
		}
	}
	if d, _ := m.Check("bash", map[string]any{"command": "go test \\\n  ./..."}, DAsk); d != DAllow {
		t.Errorf("continued go test = %v, want allow", d)
	}
	// A deny rule sees the program bash runs, whatever the shell kind.
	md := NewManager(Default)
	md.AddRule(DDeny, Rule{ToolPattern: "bash", CommandPrefix: "rm"})
	if d, _ := md.Check("bash", map[string]any{"command": `r\m -r build`}, DAllow); d != DDeny {
		t.Errorf(`deny rm: r\m -r build = %v, want deny`, d)
	}
}
