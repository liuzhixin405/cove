package main

import (
	"strings"
	"testing"
)

// /new replaces the conversation a running task is using.
func TestNewIsRefusedWhileATaskRuns(t *testing.T) {
	if !commandMutatesEngine("/new") {
		t.Fatal("/new must not run while a task is using the session")
	}
	if commandMutatesEngine("/clear") {
		t.Fatal("/clear only touches the screen")
	}
}

func TestNewSessionNotice(t *testing.T) {
	if got := newSessionNotice(""); strings.Contains(got, "已保存") {
		t.Fatalf("nothing was saved but the notice says so: %q", got)
	}
	if got := newSessionNotice("session-1-ab"); !strings.Contains(got, "session-1-ab") || !strings.Contains(got, "/history") {
		t.Fatalf("notice does not say where the old session went: %q", got)
	}
}
