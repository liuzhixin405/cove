//go:build !windows

package textmode

// consoleNeedsASCIIFallback is always false off Windows: every terminal that
// matters there is UTF-8, and there is no code page to ask about.
func consoleNeedsASCIIFallback() bool { return false }
