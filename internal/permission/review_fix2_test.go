package permission

import "testing"

// Review fix round 2: regression tests built from the reviewer's probes.

// PowerShell treats typographic quotes (U+2018–U+201F) as quote characters,
// so a line the tokenizer reads one way PowerShell reads another.
func TestPowerShellSmartQuotesAreNeverTrusted(t *testing.T) {
	c := NewClassifier()
	cmds := []string{
		"gci \"x” | where { rm $_ } #\"",
		"Get-Content \"a” | ForEach-Object { Remove-Item b } #\"",
		"gci \"x”; Remove-Item b; #\"",
		"gci 'x’ | rm b #'",
		"gci “x”",
	}
	for _, kind := range []ShellKind{ShellPowerShell, ""} {
		for _, cmd := range cmds {
			if got := c.ClassifyLineFor(cmd, kind); got == CatSafe || got == CatBuild {
				t.Errorf("[%q] ClassifyLineFor(%q) = %v, want CatUnknown", kind, cmd, got)
			}
			if c.IsReadOnlyLineFor(cmd, kind) || c.AutoApproveLineFor(cmd, kind) {
				t.Errorf("[%q] %q auto-allowed", kind, cmd)
			}
			if _, ok := CommandPrefixesFor(cmd, kind); ok {
				t.Errorf("[%q] CommandPrefixesFor(%q) offered a prefix", kind, cmd)
			}
		}
	}
	m := NewManager(Default)
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "gci"})
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "Get-Content"})
	m.AddRule(DAllow, Rule{ToolPattern: "powershell", CommandPrefix: "where"})
	for _, cmd := range cmds {
		if d, _ := m.Check("powershell", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("prefix rules covered %q: %v", cmd, d)
		}
	}
	// bash treats them as ordinary characters.
	if !c.IsReadOnlyLineFor("echo “hi”", ShellPOSIX) {
		t.Error("POSIX: echo with typographic quotes should stay read-only")
	}
}

// >&WORD is a descriptor copy/close only when WORD is all digits, exactly
// "-", or digits followed by "-"; anything else is a file bash writes.
func TestRedirectAmpersandWordRules(t *testing.T) {
	c := NewClassifier()
	writes := []string{
		"cat a >&1b", "echo x >&0evil.sh", "cat a >& b", `cat a >&"b"`,
		"cat a &>b", "cat a >|b", "cat a >&-x", "cat a 2>&1x",
	}
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "cat"})
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "echo"})
	for _, cmd := range writes {
		if c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q) = true, want false (writes a file)", cmd)
		}
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("prefix rule covered %q: %v", cmd, d)
		}
	}
	for _, cmd := range []string{"cat a 2>&1", "cat a >&2", "cat a 2>&-", "cat a >&1-", "cat a 2>&1 | wc -l", "cat a 2>&1;ls"} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q) = false, want true (descriptor copy)", cmd)
		}
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d == DDeny {
			t.Errorf("%q denied", cmd)
		}
	}
}

func TestTreeCombinedShortOptionsThatWrite(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{"tree -aR -L 2", "tree -ao out.txt", "tree -fH .", "tree -Ra"} {
		if c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q) = true, want false", cmd)
		}
	}
	for _, cmd := range []string{"tree -a -L 2", "tree -af", "tree --dirsfirst"} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q) = false, want true", cmd)
		}
	}
}

// Round 3: bash removes quotes before judging the word after >&, so a quote
// glued to it (>&1'b', 2>&1"b", >&1-'x') makes it a file name.
func TestRedirectAmpersandWordWithQuotesIsAFile(t *testing.T) {
	c := NewClassifier()
	m := NewManager(Default)
	m.SetShellKind(ShellPOSIX)
	m.AddRule(DAllow, Rule{ToolPattern: "bash", CommandPrefix: "cat"})
	for _, cmd := range []string{"cat a >&1'b'", `cat a 2>&1"b"`, "cat a >&1-'x'", "cat a >&'1'b"} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got == CatSafe {
			t.Errorf("ClassifyLineFor(%q) = CatSafe, want not safe (writes a file)", cmd)
		}
		if d, _ := m.Check("bash", map[string]any{"command": cmd}, DAsk); d != DAsk {
			t.Errorf("cat prefix covered %q: %v", cmd, d)
		}
	}
	for _, cmd := range []string{"cat a 2>&1", "cat a >&2", "cat a >&1-", "cat a 1>&2"} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("IsReadOnlyLineFor(%q) = false, want true", cmd)
		}
	}
}
