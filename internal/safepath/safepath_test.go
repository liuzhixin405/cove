package safepath

import (
	"path/filepath"
	"testing"
)

func TestValidateNameAcceptsOrdinaryNames(t *testing.T) {
	for _, name := range []string{"my-plugin", "skill_2", "v1.2.3", "a", "Console", "nullable", "com10", "lpt0"} {
		if err := ValidateName("plugin", name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
}

func TestValidateNameRejectsTraversalAndSeparators(t *testing.T) {
	for _, name := range []string{
		"", ".", "..", "../x", `..\x`, "a/b", `a\b`, `C:x`, `\\server\share`, `\\?\C:\x`,
		"file.txt:stream", "PROGRA~1", ".hidden", "x.disabled", "a b", "a\x00b",
	} {
		if err := ValidateName("plugin", name); err == nil {
			t.Errorf("ValidateName(%q) accepted", name)
		}
	}
}

// TestValidateNameRejectsWindowsAliases: Windows strips a trailing dot, so
// "foo." is the directory "foo" — a name from a remote manifest could target
// (and, through the install's cleanup, delete) another plugin's directory, and
// "x.disabled." slipped past the .disabled check. Device names (CON, NUL, COM1,
// with or without an extension) are not directories at all.
func TestValidateNameRejectsWindowsAliases(t *testing.T) {
	for _, name := range []string{
		"foo.", "foo..", "x.disabled.",
		"CON", "con", "PRN", "AUX", "NUL", "nul.txt", "COM1", "com9.log", "LPT1", "lpt3.x", "CONIN$", "conout$",
	} {
		if err := ValidateName("plugin", name); err == nil {
			t.Errorf("ValidateName(%q) accepted", name)
		}
	}
}

func TestJoinStaysDirectlyUnderRoot(t *testing.T) {
	root := t.TempDir()
	got, err := Join("skill", root, "tool-x")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != root {
		t.Fatalf("Join = %q, want a direct child of %q", got, root)
	}
	if _, err := Join("skill", root, "../escape"); err == nil {
		t.Fatal("Join accepted a traversal")
	}
}
