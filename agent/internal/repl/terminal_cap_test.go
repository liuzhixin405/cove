package repl

import "testing"

func TestForcePlainReadlineEnv(t *testing.T) {
	t.Setenv("AGENTGO_PLAIN_REPL", "1")
	if !forcePlainReadline() {
		t.Fatalf("expected plain repl mode when AGENTGO_PLAIN_REPL=1")
	}
}

func TestForcePlainReadlineEnvOff(t *testing.T) {
	t.Setenv("AGENTGO_PLAIN_REPL", "0")
	if forcePlainReadline() {
		t.Fatalf("expected plain repl mode off when AGENTGO_PLAIN_REPL=0")
	}
}
