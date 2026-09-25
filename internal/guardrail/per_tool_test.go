package guardrail

import "testing"

// The 30-second breaker counts each tool on its own: five failed bash
// commands must not block the next read, and a read that succeeds must not
// clear bash's failures.
func TestRapidFailureBreakerIsPerTool(t *testing.T) {
	tr := New()
	for i := 0; i < rapidFailBlock; i++ {
		tr.AfterCall("bash", map[string]any{"command": string(rune('a' + i))}, "boom", true)
	}
	if d := tr.BeforeCall("read", map[string]any{"filePath": "x.go"}); d.Action != Allow {
		t.Fatalf("read blocked by bash failures: %+v", d)
	}
	tr.AfterCall("read", map[string]any{"filePath": "x.go"}, "ok", false)
	if d := tr.BeforeCall("bash", map[string]any{"command": "new"}); d.Action != Block {
		t.Fatalf("bash breaker cleared by another tool's success: %+v", d)
	}
	tr.AfterCall("bash", map[string]any{"command": "ok"}, "fine", false)
	if d := tr.BeforeCall("bash", map[string]any{"command": "new"}); d.Action != Allow {
		t.Fatalf("bash success did not reset its own breaker: %+v", d)
	}
}
