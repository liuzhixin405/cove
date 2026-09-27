package api

import "testing"

// A local model wrote a shell command with unescaped double quotes inside
// the JSON string ("cd "D:/github/agent" && …"); the call was rejected as
// invalid JSON and the turn lost a step. Quotes that cannot be closing
// quotes (the next non-space character is not , } ] or :) are literal.
func TestRepairToolArguments_EscapesUnescapedQuotesInsideStrings(t *testing.T) {
	raw := `{"command":"cd "D:/github/agent" && dotnet new sln -n AgentFramework 2>&1","description":"Create the "solution" file"}`
	args, ok := RepairToolArguments(raw)
	if !ok {
		t.Fatalf("not repaired: %s", raw)
	}
	if args["command"] != `cd "D:/github/agent" && dotnet new sln -n AgentFramework 2>&1` {
		t.Fatalf("command = %q", args["command"])
	}
	if args["description"] != `Create the "solution" file` {
		t.Fatalf("description = %q", args["description"])
	}
}

// Already-escaped quotes and quotes that really close a string are left
// alone: clean JSON parses as it did.
func TestRepairToolArguments_QuoteRepairLeavesValidJSONAlone(t *testing.T) {
	raw := `{"command":"echo \"hi\"","filePath":"a.go","n": 3, "list":["x","y"]}`
	args, ok := RepairToolArguments(raw)
	if !ok || args["command"] != `echo "hi"` || args["filePath"] != "a.go" {
		t.Fatalf("clean JSON mis-repaired: %v %v", args, ok)
	}
	if got := escapeUnescapedQuotes(raw); got != raw {
		t.Fatalf("valid JSON was rewritten:\n%s\n%s", raw, got)
	}
}

// Truncated arguments stay a failure: escaping quotes must not turn a
// half-written call into a runnable one.
func TestRepairToolArguments_QuoteRepairDoesNotRescueTruncation(t *testing.T) {
	if _, ok := RepairToolArguments(`{"command":"cd "D:/github/agent" && dotnet`); ok {
		t.Fatal("truncated arguments were accepted")
	}
}
