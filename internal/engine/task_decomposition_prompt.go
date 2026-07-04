package engine

import (
	"regexp"
	"strings"
)

// complexTaskFilePattern is a light, best-effort heuristic for "the user
// named several specific files" — independently duplicated from the same
// idea as internal/api/router.go's file-scope routing signal, to avoid
// adding an engine<->api coupling purely for a prompt-guidance heuristic.
var complexTaskFilePattern = regexp.MustCompile(`[\w./\\-]+\.(go|py|js|ts|tsx|jsx|java|rs|c|cpp|h|hpp|rb|php|json|yaml|yml|md|sql)\b`)

var complexTaskKeywords = []string{
	"refactor", "migrate", "rewrite", "redesign", "architecture", "overhaul",
	"重构", "迁移", "重写", "架构", "全面", "梳理",
}

// complexTaskLengthThreshold: messages at least this long are, empirically,
// far more likely to describe a multi-step task than a one-off request.
const complexTaskLengthThreshold = 300

// suggestsComplexTask is a cheap heuristic for "this message probably
// describes a multi-step task that would benefit from an explicit plan
// before diving in." It is intentionally permissive (a false positive just
// means the model sees an extra, ignorable suggestion) since the guidance
// this drives is a suggestion, never a gate.
func suggestsComplexTask(userMessage string) bool {
	if len(userMessage) >= complexTaskLengthThreshold {
		return true
	}
	if len(complexTaskFilePattern.FindAllString(userMessage, -1)) >= 3 {
		return true
	}
	lower := strings.ToLower(userMessage)
	for _, kw := range complexTaskKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// taskDecompositionGuidance returns additional system-prompt content
// (appended via ChatRequest.System, the same mechanism as
// weakModelGuidance in model_tier_prompt.go) suggesting the model lay out
// an explicit multi-step plan before acting, for messages that look like
// multi-step tasks.
//
// This is deliberately a nudge, not a gate: Cove does not block execution
// or force a decomposition tool call. Forcing a plan onto a message that
// only *looks* complex (per the cheap heuristic above) but turns out to be
// simple would just waste a round trip; the guidance text itself says so.
// Mid-tier models in particular tend to skip decomposition and attempt a
// wide-scope change in one shot — this is aimed at that specific pattern.
func taskDecompositionGuidance(userMessage string) string {
	if !suggestsComplexTask(userMessage) {
		return ""
	}
	return `

# This looks like a multi-step task

Before making changes, lay out a short plan first:
1. List the distinct sub-tasks (aim for 3-5 concrete steps), e.g. via todowrite.
2. Tackle them one at a time — finish and verify one step before starting the next.
3. Prefer several small, verifiable changes over one large, hard-to-verify change.

If, after looking closer, this turns out to be simpler than it first looked, it's fine to skip the plan and just do it directly.`
}
