//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestWindowsConsoleUTF8WarningIncludesDetectedCodePages(t *testing.T) {
	msg := windowsConsoleEncodingWarning(936, 936)
	checks := []string{
		"Console code page warning / 控制台代码页提醒",
		"input CP=936",
		"output CP=936",
		"chcp 65001",
		"Windows Terminal",
	}
	for _, want := range checks {
		if !strings.Contains(msg, want) {
			t.Fatalf("warning missing %q\nfull warning:\n%s", want, msg)
		}
	}
}

func TestWindowsConsoleUTF8WarningSkipsUTF8(t *testing.T) {
	if got := windowsConsoleEncodingWarning(65001, 65001); got != "" {
		t.Fatalf("expected empty warning for UTF-8 console, got %q", got)
	}
}

func TestWindowsConsoleEncodingActiveDetectsMismatch(t *testing.T) {
	if !windowsConsoleEncodingActive(936, 65001) {
		t.Fatalf("expected mismatched code pages to be considered active warning")
	}
	if !windowsConsoleEncodingActive(65001, 936) {
		t.Fatalf("expected non-UTF-8 output code page to be considered active warning")
	}
	if windowsConsoleEncodingActive(65001, 65001) {
		t.Fatalf("expected UTF-8/UTF-8 console to skip warning")
	}
}
