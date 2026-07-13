package tui

import (
	"os"
	"strings"
)

// PreferASCIIText reports whether the terminal should avoid non-ASCII UI text.
// This protects legacy Windows code pages from mojibake in status/hint lines.
func PreferASCIIText() bool {
	if v, ok := envBool("COVE_TUI_ASCII"); ok {
		return v
	}
	return windowsConsoleNeedsASCIIFallback()
}

func envBool(name string) (bool, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	v := strings.TrimSpace(strings.ToLower(raw))
	switch v {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
}
