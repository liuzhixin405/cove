package main

import (
	"strings"
	"testing"
)

func TestTUIActivityText_ASCIIOverride(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "1")

	if got, want := tuiCommandActivityText(), "running command..."; got != want {
		t.Fatalf("command activity = %q, want %q", got, want)
	}
	if got, want := tuiThinkingActivityText(), "thinking..."; got != want {
		t.Fatalf("thinking activity = %q, want %q", got, want)
	}
	if got, want := tuiToolActivityText("powershell"), "running powershell"; got != want {
		t.Fatalf("tool activity = %q, want %q", got, want)
	}
}

func TestTUIActivityText_NonASCIIDefault(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")

	if got, want := tuiCommandActivityText(), "执行命令…"; got != want {
		t.Fatalf("command activity = %q, want %q", got, want)
	}
	if got, want := tuiThinkingActivityText(), "思考中…"; got != want {
		t.Fatalf("thinking activity = %q, want %q", got, want)
	}
	if got, want := tuiToolActivityText("powershell"), "执行 powershell"; got != want {
		t.Fatalf("tool activity = %q, want %q", got, want)
	}
}

func TestTUIActivityText_NoMojibakeMarkers(t *testing.T) {
	t.Setenv("COVE_TUI_ASCII", "0")

	candidates := []string{
		tuiCommandActivityText(),
		tuiThinkingActivityText(),
		tuiToolActivityText("powershell"),
	}
	badMarkers := []string{"鎵", "鈿", "闄", "閿", "鍔", "馃", "", ""}

	for _, s := range candidates {
		for _, m := range badMarkers {
			if strings.Contains(s, m) {
				t.Fatalf("activity text contains mojibake marker %q: %q", m, s)
			}
		}
	}
}
