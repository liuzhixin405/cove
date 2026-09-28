package repl

// The relay of typed lines to a waiting prompt (permission, question,
// limit): which lines answer it and which are the next instruction.

import (
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/termui"
)

var permInputCh chan<- string

// permAccepts says which typed lines answer the waiting prompt (nil: any
// non-empty line) and permHint is what to show for a line that does not.
var permAccepts func(string) bool

var permHint string

// SetPromptInput registers the channel a waiting prompt reads its answer
// from, with the test for what counts as an answer and the hint to show for
// a line that does not. Only answers are relayed: the person may be typing
// the next instruction when a prompt appears, and that line used to be
// swallowed as a refusal.
func SetPromptInput(ch chan<- string, accepts func(string) bool, hint string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh = ch
	permAccepts = accepts
	permHint = hint
}

// PromptInputState is what TakePromptInputFor found for a typed line.
type PromptInputState int

const (
	// PromptNone: no prompt is waiting; the line is ordinary input.
	PromptNone PromptInputState = iota
	// PromptAnswer: the line answers the waiting prompt; send it on the
	// returned channel, which is now unregistered.
	PromptAnswer
	// PromptNotAnswer: a prompt is waiting but the line is not an answer to
	// it; the prompt keeps waiting and the line is the caller's to handle.
	PromptNotAnswer
)

// TakePromptInputFor decides what a typed line is for the prompt that may be
// waiting; hint is the prompt's hint when the line is not an answer.
func TakePromptInputFor(line string) (ch chan<- string, state PromptInputState, hint string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if permInputCh == nil {
		return nil, PromptNone, ""
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || (permAccepts != nil && !permAccepts(trimmed)) {
		return nil, PromptNotAnswer, permHint
	}
	ch = permInputCh
	permInputCh, permAccepts, permHint = nil, nil, ""
	return ch, PromptAnswer, ""
}

func ClearPermInputCh() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	permInputCh, permAccepts, permHint = nil, nil, ""
}

// TakePermInputCh takes the waiting prompt's channel unconditionally (Ctrl+C
// answers every prompt); TakePromptInputFor is the typed-line path.
func TakePermInputCh() chan<- string {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	ch := permInputCh
	permInputCh, permAccepts, permHint = nil, nil, ""
	return ch
}

// askMu serialises prompts: one prompt owns the answer relay at a time. The
// permission, question and limit prompts each registered the relay on their
// own, and only the first and last were serialised (by the engine's prompt
// lock): a question and a permission prompt from parallel tool calls
// registered over each other, and the second one's timeout unregistered
// the first.
var askMu sync.Mutex

// Ask shows text above the input line and waits up to timeout for a typed
// line accepts takes as an answer (nil: any non-empty line); hint is shown
// for a line that is not one. ok is false on timeout. Either way nothing is
// registered afterwards, so a later line is an ordinary one.
func Ask(text string, accepts func(string) bool, hint string, timeout time.Duration) (answer string, ok bool) {
	askMu.Lock()
	defer askMu.Unlock()
	ch := make(chan string, 1)
	SetPromptInput(ch, accepts, hint)
	BeginPromptInput()
	termui.PrintAbove(text)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case answer = <-ch:
		ok = true
	case <-timer.C:
	}
	EndPromptInput()
	ClearPermInputCh()
	return answer, ok
}
