package engine

// weakModelGuidance returns additional system-prompt content, appended via
// ChatRequest.System (kept separate from the cached base SystemPrompt so
// top-tier-model turns pay zero extra prompt-token cost), for turns that
// were routed to a fast/mid-tier model.
//
// Top-tier models already do implicit task decomposition, context-checking,
// and self-verification reasonably well without being told to. Mid-tier
// models measurably benefit from having those expectations spelled out
// explicitly — that's exactly the kind of "read between the lines" judgment
// they're weaker at — so this makes the same behavior an explicit
// instruction instead of an assumption.
func weakModelGuidance(routedModel string) string {
	if !isFastModelName(routedModel) {
		return ""
	}
	return `

# Additional Guidance For This Turn (fast/mid-tier model routed)

You are running on a lighter/faster model for this turn. Be extra disciplined:

1. One tool call at a time. Do not try to plan and execute multiple steps in a single response — call one tool, look at its real output, then decide the next step.
2. Confirm before you assume. Before editing a file, read or grep it first to confirm the exact text you intend to match actually exists — do not guess at file contents from memory or from earlier context in this conversation.
3. State your verification plan before claiming a task is done: name the exact command you will run (build/test/lint), run it, and only report completion after seeing its real output.
4. If a tool call fails or a match is not found, stop and re-read the error before retrying — do not immediately repeat the same call with the same arguments.
5. Keep each step small. Prefer several small, verified edits over one large edit you cannot fully verify at once.`
}
