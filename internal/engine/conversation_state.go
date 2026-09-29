package engine

// conversation is the Engine state that belongs to one conversation. It is
// embedded in Engine, so its fields read as e.interrupted and so on, and
// resetConversationState clears it in one assignment: a field added here is
// reset on /new and /resume without anyone having to remember it. Fields
// were reset one by one before, in four places, and each missed some.
//
// Only fields the turn goroutine owns belong here. Conversation state that
// is guarded by a mutex (pending steer, file history, the review throttle,
// shown skills and memories) is reset under its lock in
// resetConversationState, and state that needs construction (loop detector,
// guardrails) is rebuilt there.
type conversation struct {
	// interrupted records a turn that ended before completing (API error,
	// cancel, budget, loop). Its completed tool rounds stay in history;
	// re-sending the same message resumes it.
	interrupted *interruption
	// interruptMarked: the current interruption already left its history
	// marker (interrupt); cleared when a turn completes.
	interruptMarked bool
	// lastWrapUp is the no-tool summary the last stopped turn ended with
	// (LastWrapUp).
	lastWrapUp string
	// lastRoutedModel is the model the previous turn ran on (routing
	// stickiness).
	lastRoutedModel string
	// lastEnvGit is the git snapshot last sent to the model, so an unchanged
	// working tree is not repeated every turn.
	lastEnvGit string

	// iterCount is how many tool/LLM loops have run.
	iterCount int
	// consecutiveErrors counts consecutive tool failures for circuit
	// breaking.
	consecutiveErrors int
	// loopHistory holds recent tool-call fingerprints for loop detection
	// when the loop detector is off.
	loopHistory []string
	// verifyAttempts is how many times the gate has rejected completion this
	// turn.
	verifyAttempts int
	// roundsSinceTodo counts tool rounds since the last todowrite call while
	// the task list has open items (todoRoundReminder).
	roundsSinceTodo int
	// planOffered: the first turn already checked for an unfinished plan of
	// an earlier session (previousPlanNote).
	planOffered bool
	// selfReviewed: this turn's diff was already reviewed (selfReview).
	selfReviewed bool

	// Notices shown once per conversation.

	// verifyAnnounced: the gate's commands were shown to the user (first
	// turn, and again after a working-directory change).
	verifyAnnounced bool
	// instrTruncNoticed: the "instruction files truncated" notice was shown.
	instrTruncNoticed bool
	// smallToolsNoted: the reduced tool set for a small window was announced.
	smallToolsNoted bool
	// outsideDirHinted names the directories (lower-cased) the user was
	// already told a tool refused to touch (noteOutsideDirectory).
	outsideDirHinted map[string]bool
}
