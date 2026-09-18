// Package textmode answers one question: may the UI use non-ASCII characters?
//
// It lived inside the full-screen shell and was deleted with it, but the
// problem it solves outlives that layout. cove's UI is Chinese throughout and
// its gutter uses box-drawing and symbol glyphs (▸ ⚙ ✓ ✗ ⎿ ›). On a console
// that is not UTF-8 those come out as mojibake — which is how the old code
// ended up printing a literal "?" where a ✓ belonged, making success and
// failure indistinguishable.
//
// cli/cove forces the Windows console to code page 65001 at init and warns
// when it cannot. This package covers the case where that forcing failed: the
// glyphs are the part that breaks first and is cheapest to substitute.
package textmode

import (
	"os"
	"strings"
)

// PreferASCII reports whether the UI should avoid non-ASCII characters.
//
// COVE_TUI_ASCII overrides the detection in either direction, which is also
// how the behaviour is exercised on a UTF-8 terminal.
func PreferASCII() bool {
	if v, ok := envBool("COVE_TUI_ASCII"); ok {
		return v
	}
	return consoleNeedsASCIIFallback()
}

func envBool(name string) (bool, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}
