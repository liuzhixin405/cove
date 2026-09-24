//go:build !windows

package main

// stdinIsMSYSPty is Windows-only: elsewhere a terminal is a character device,
// which stdinIsPiped already recognises.
func stdinIsMSYSPty() bool { return false }
