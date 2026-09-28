package repl

import (
	"testing"
	"time"
)

// Two prompts at once (a question and a permission prompt from parallel
// tool calls) take the relay one after the other: the first line answers
// the first prompt, the second line the second.
func TestAskSerialisesPrompts(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	// A prompt runs inside a streamed turn and EndPromptInput goes back to
	// streaming; here there is no turn, so leave the console idle again.
	t.Cleanup(func() {
		ClearPermInputCh()
		consoleMu.Lock()
		streamingActive = false
		consoleMu.Unlock()
	})

	type res struct {
		answer string
		ok     bool
	}
	first, second := make(chan res, 1), make(chan res, 1)
	go func() { a, ok := Ask("first?\n", nil, "", 5*time.Second); first <- res{a, ok} }()
	waitRegistered(t)
	go func() { a, ok := Ask("second?\n", nil, "", 5*time.Second); second <- res{a, ok} }()

	answer(t, "one")
	if r := <-first; !r.ok || r.answer != "one" {
		t.Fatalf("first prompt got %+v", r)
	}
	waitRegistered(t)
	answer(t, "two")
	if r := <-second; !r.ok || r.answer != "two" {
		t.Fatalf("second prompt got %+v", r)
	}
}

func waitRegistered(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		consoleMu.Lock()
		on := permInputCh != nil
		consoleMu.Unlock()
		if on {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no prompt registered")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func answer(t *testing.T, line string) {
	t.Helper()
	ch, state, _ := TakePromptInputFor(line)
	if state != PromptAnswer {
		t.Fatalf("%q was not taken as an answer (state %v)", line, state)
	}
	ch <- line
}
