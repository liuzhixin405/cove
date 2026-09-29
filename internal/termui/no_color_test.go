package termui

import (
	"os"
	"testing"
)

// TestMain pins the colour decision the render tests assert on. They compare
// exact bytes, including SGR sequences, so an ambient NO_COLOR would fail
// them — and NO_COLOR is set in plenty of environments (Cove's own shells set
// it for the tools they spawn). noColor() reads the setting at first use, so
// clearing it here is enough to decouple the suite from the caller.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("NO_COLOR")
	os.Exit(m.Run())
}

// TestUncolorFollowsNoColor covers both sides of the decision, which the pin
// above deliberately keeps out of the render tests.
func TestUncolorFollowsNoColor(t *testing.T) {
	// This test borrows NO_COLOR; it hands back the pinned state TestMain
	// established, not whatever the caller's environment had.
	defer func() {
		_ = os.Unsetenv("NO_COLOR")
		resetNoColor()
	}()

	if got, want := uncolor(Dim+"boom"+Reset), Dim+"boom"+Reset; got != want {
		t.Errorf("uncolor with colour allowed = %q, want %q", got, want)
	}

	_ = os.Setenv("NO_COLOR", "1")
	resetNoColor()
	if got, want := uncolor(Dim+"boom"+Reset), "boom"; got != want {
		t.Errorf("uncolor under NO_COLOR = %q, want %q", got, want)
	}
	// Cursor control is not colour: the caller still clears the line.
	if got, want := uncolor("\x1b[2K"+Dim+"boom"+Reset), "\x1b[2Kboom"; got != want {
		t.Errorf("uncolor under NO_COLOR dropped cursor control: %q, want %q", got, want)
	}
}
