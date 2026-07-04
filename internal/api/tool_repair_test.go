package api

import "testing"

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

func TestRepairToolArguments_TruncatedString(t *testing.T) {
	// Truncated mid-value: a long "content" string cut off before its closing quote.
	raw := `{"filePath": "a.go", "content": "package main\nfunc main() {`
	args, ok := RepairToolArguments(raw)
	if !ok {
		t.Fatalf("expected truncated JSON to be repaired")
	}
	if args["filePath"] != "a.go" {
		t.Fatalf("expected filePath to survive repair, got %v", args["filePath"])
	}
}

func TestRepairToolArguments_TruncatedAfterComma(t *testing.T) {
	// Truncated right after a trailing comma, before the next key started.
	raw := `{"oldString": "foo", "newString": "bar",`
	args, ok := RepairToolArguments(raw)
	if !ok {
		t.Fatalf("expected trailing-comma truncation to be repaired")
	}
	if args["oldString"] != "foo" || args["newString"] != "bar" {
		t.Fatalf("unexpected repaired args: %v", args)
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

func TestRepairToolArguments_NestedTruncation(t *testing.T) {
	raw := `{"filePath": "a.go", "edits": [{"old": "x", "new": "y"}, {"old": "p", "new": "q`
	args, ok := RepairToolArguments(raw)
	if !ok {
		t.Fatalf("expected nested truncation to be repaired")
	}
	edits, ok := args["edits"].([]any)
	if !ok || len(edits) != 2 {
		t.Fatalf("expected 2 recovered edit entries, got %v", args["edits"])
	}
}

func TestRepairToolArguments_Unrecoverable(t *testing.T) {
	// Binary garbage / no JSON structure at all should fail cleanly, not panic.
	_, ok := RepairToolArguments("not json at all and no braces")
	if ok {
		t.Fatalf("expected unrecoverable input to fail")
	}
}

func TestRepairToolArguments_QuoteInsideTruncatedString(t *testing.T) {
	// Ensure the brace/bracket tracker correctly ignores braces that appear
	// inside string literals when deciding how many closers to append.
	raw := `{"command": "echo '{not a real brace'`
	args, ok := RepairToolArguments(raw)
	if !ok {
		t.Fatalf("expected repair to succeed despite brace-like characters inside a string")
	}
	if _, exists := args["command"]; !exists {
		t.Fatalf("expected command key to survive repair: %v", args)
	}
}
