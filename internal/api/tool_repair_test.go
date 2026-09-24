package api

import (
	"strings"
	"testing"
)

func TestRepairToolArguments_CleanJSON(t *testing.T) {
	args, ok := RepairToolArguments(`{"filePath": "a.go", "content": "package main"}`)
	if !ok {
		t.Fatalf("expected clean JSON to parse")
	}
	if args["filePath"] != "a.go" {
		t.Fatalf("unexpected filePath: %v", args["filePath"])
	}
}

func TestRepairToolArguments_Empty(t *testing.T) {
	args, ok := RepairToolArguments("")
	if !ok {
		t.Fatalf("expected empty arguments to be treated as no-arg call")
	}
	if len(args) != 0 {
		t.Fatalf("expected empty map, got %v", args)
	}
}

// Truncated arguments used to be "repaired" by closing the open string and
// braces. For a write or edit that meant running the tool with half the
// content and writing it to disk, so every truncated shape below must now
// fail and be reported back to the model instead. (These tests used to
// assert the opposite.)
func TestRepairToolArguments_RefusesTruncatedArguments(t *testing.T) {
	cases := map[string]string{
		"cut mid string":        `{"filePath": "a.go", "content": "package main\nfunc main() {`,
		"cut after comma":       `{"oldString": "foo", "newString": "bar",`,
		"cut inside array":      `{"filePath": "a.go", "edits": [{"old": "x", "new": "y"}, {"old": "p", "new": "q`,
		"brace inside open str": `{"command": "echo '{not a real brace'`,
	}
	for name, raw := range cases {
		if args, ok := RepairToolArguments(raw); ok {
			t.Errorf("%s: truncated arguments were accepted as %v", name, args)
		}
	}
}

func TestRepairToolArguments_StrayTrailingTokens(t *testing.T) {
	// Some providers append stray whitespace/newlines after an otherwise-valid object.
	raw := "{\"command\": \"ls -la\"}\n\n"
	args, ok := RepairToolArguments(raw)
	if !ok {
		t.Fatalf("expected trailing whitespace to be tolerated")
	}
	if args["command"] != "ls -la" {
		t.Fatalf("unexpected command: %v", args["command"])
	}
}

func TestRepairToolArguments_StrayTextAroundCompleteObject(t *testing.T) {
	args, ok := RepairToolArguments("```json\n{\"command\": \"ls\"}\n```")
	if !ok || args["command"] != "ls" {
		t.Fatalf("args = %v, ok = %v; want the wrapped object", args, ok)
	}
}

func TestRepairToolArguments_Unrecoverable(t *testing.T) {
	// Binary garbage / no JSON structure at all should fail cleanly, not panic.
	_, ok := RepairToolArguments("not json at all and no braces")
	if ok {
		t.Fatalf("expected unrecoverable input to fail")
	}
}

func TestToolArgsParseErrorExplainsTruncation(t *testing.T) {
	msg, _ := toolArgsParseError(`{"content":"abc`, true)["_cove_parse_error"].(string)
	if !strings.Contains(msg, "output token limit") {
		t.Fatalf("message = %q, want it to name the output limit", msg)
	}
	plain, _ := toolArgsParseError(`{"content":"abc`, false)["_cove_parse_error"].(string)
	if strings.Contains(plain, "output token limit") {
		t.Fatalf("message = %q, must not blame the limit when it was not hit", plain)
	}
}
