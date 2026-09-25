package guardrail

import "testing"

// TestResetClearsRapidFailureWindow: Reset is documented to clear all tracking
// state, but it left the 30-second rapid-failure window alone. Five failures
// just before a reset (a new user turn) still tripped the circuit breaker on
// the first call afterwards. (The breaker is per tool, so bash is probed.)
func TestResetClearsRapidFailureWindow(t *testing.T) {
	tr := New()
	for i := 0; i < rapidFailBlock; i++ {
		tr.AfterCall("bash", map[string]any{"command": i}, "exit 1", true)
	}
	if got := tr.BeforeCall("bash", map[string]any{"command": "next"}); got.Action != Block {
		t.Fatalf("precondition: expected the breaker to be tripped, got %v", got.Action)
	}

	tr.Reset()
	if got := tr.BeforeCall("bash", map[string]any{"command": "next"}); got.Action != Allow {
		t.Fatalf("after Reset: %v (%s), want Allow", got.Action, got.Message)
	}
}
