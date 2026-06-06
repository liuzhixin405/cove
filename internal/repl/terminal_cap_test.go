package repl

import "testing"

func TestForcePlainReadlineEnv(t *testing.T) {
	t.Setenv("COVE_PLAIN_REPL", "1")
	if !forcePlainReadline() {
		t.Fatalf("expected plain repl mode when COVE_PLAIN_REPL=1")
	}
}

func TestForcePlainReadlineEnvOff(t *testing.T) {
	t.Setenv("COVE_PLAIN_REPL", "0")
	if forcePlainReadline() {
		t.Fatalf("expected plain repl mode off when COVE_PLAIN_REPL=0")
	}
}
