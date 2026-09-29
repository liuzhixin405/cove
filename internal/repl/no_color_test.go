package repl

import (
	"os"
	"testing"
)

// TestMain pins the colour decision. The render tests here compare exact bytes
// including SGR sequences, so an ambient NO_COLOR would fail them — and
// NO_COLOR is set in plenty of environments (Cove's own shells set it for the
// tools they spawn). noColor() reads the setting at first use, so clearing it
// here decouples the suite from the caller.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("NO_COLOR")
	os.Exit(m.Run())
}

// TestNoColorFollowsTheEnvironment covers the decision the pin above keeps out
// of the render tests: the value is read lazily and then held.
func TestNoColorFollowsTheEnvironment(t *testing.T) {
	defer func() {
		_ = os.Unsetenv("NO_COLOR")
		resetNoColor()
	}()

	if noColor() {
		t.Errorf("noColor() = true with NO_COLOR cleared")
	}

	_ = os.Setenv("NO_COLOR", "1")
	resetNoColor()
	if !noColor() {
		t.Errorf("noColor() = false with NO_COLOR=1")
	}

	// Read once: flipping the variable mid-session does not repaint history.
	_ = os.Unsetenv("NO_COLOR")
	if !noColor() {
		t.Errorf("noColor() re-read the environment after the first decision")
	}
}
