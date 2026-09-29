package permission

import "testing"

// Checking that a tool is installed asks nothing: "which gh", "git
// --version", "command -v gh". Running through "command" still does.
func TestToolLookupIsReadOnly(t *testing.T) {
	c := NewClassifier()
	for _, cmd := range []string{"which gh", "git --version", "which gh\ngit --version", "command -v gh", "command -V gh"} {
		if !c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q not read-only", cmd)
		}
	}
	for _, cmd := range []string{"command rm -rf x", "git --version --exec-path=/x", "command -v"} {
		if c.IsReadOnlyLineFor(cmd, ShellPOSIX) {
			t.Errorf("%q taken as read-only", cmd)
		}
	}
}
