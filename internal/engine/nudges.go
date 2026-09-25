package engine

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
)

// Per-turn caps of every prompt the engine injects on its own ("nudges").
// Each one is bounded so no nudge can keep a turn alive forever; the values
// live here, together, so the documentation can quote them.
const (
	// maxContinuationNudgesPerTurn bounds the "you announced a next step but
	// did not do it" and "your ending is too brief" nudges together: after
	// that many, the model's reply is accepted as the final answer.
	maxContinuationNudgesPerTurn = 2
	// maxEmptyResponseRetries bounds how often an empty (or thinking-only)
	// reply is sent back; the next one ends the turn as it is.
	maxEmptyResponseRetries = 2
	// doneCheckOncePerTurn: the "check the request is fully met" prompt is
	// shown at most this many times per turn (see doneCheckEnabled).
	doneCheckOncePerTurn = 1
	// loopPromptOnHit is the non-fatal loop detection hit at which an
	// interactive front end is asked whether to go on (LimitReasonLoop).
	loopPromptOnHit = 2
	// budgetNoticeRatio is the share of an iteration or time window after
	// which the model is told, once per window, how much of it remains.
	budgetNoticeRatio = 0.8
	// costBudgetNoticeRatio is the share of max_budget_usd at which the
	// user (not the model) is told once that the spend cap is near.
	costBudgetNoticeRatio = 0.8

	// degenerateEndingMaxRunes: a final reply shorter than this after a turn
	// that used tools is treated as a fragment, not an answer.
	degenerateEndingMaxRunes = 40
	// degenerateEndingMaxRunesCJK is the same bar for text with Chinese in it.
	degenerateEndingMaxRunesCJK = 20
)

// Texts of the nudges (English, like every engine-injected instruction).
const (
	nudgeAnnouncedText  = "[system: You announced a next step but did not perform it. Continue now by calling the required tools, or state clearly that the task is complete.]"
	nudgeDegenerateText = "[system: Your last message is too brief to be a final answer after doing work. Summarize what you changed and what remains, or continue.]"
	nudgeEmptyText      = "[system: Your response was empty. Provide the answer or call a tool.]"
	nudgeDoneCheckText  = "[system: Before finishing, check whether the user's request has been fully met. If anything remains, continue working now; if everything is done, reply with the final answer.]"
)

// completionMarkers are words that make an ending read as "finished".
var completionMarkers = []string{"完成", "已提交", "已修复", "已修改", "已更新", "已添加", "已删除", "已创建", "搞定", "done", "finished", "complete", "committed"}

// completedVerbZH catches "已按要求修改" / "已全部更新": 已, a few qualifying
// characters, then a verb of finished work.
var completedVerbZH = regexp.MustCompile(`已[^\s，。,.!！;；]{0,6}?(修改|更新|添加|删除|创建|修复|提交)`)

// hasCompletionMarker reports whether lower (lower-cased text) reads as
// "finished".
func hasCompletionMarker(lower string) bool {
	for _, m := range completionMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return completedVerbZH.MatchString(lower)
}

// announceStartsZH / announceStartsEN open a sentence that states what the
// model is about to do. They only count at the start of a sentence or line:
// "next iteration" in the middle of a sentence announces nothing.
var (
	announceStartsZH = []string{"接下来", "下一步", "现在我来", "现在让我", "让我", "我这就", "我现在就", "我将", "现在开始", "然后我"}
	announceStartsEN = []string{"let me ", "i'll ", "i will ", "next, ", "now i'll ", "now i will ", "now let me ", "i'm going to ", "i am going to ", "then i'll "}
)

// announceExclusions turn an apparent announcement into something else: an
// offer to the user, a recommendation, work handed over or waiting on the
// user, a negation. Any second person ("you", "你") counts as handing over.
var announceExclusions = []string{
	"let me know", "建议", "如需", "如果需要", "是否需要", "如有问题", "请", "我将不", "不会", "不再", "等待", "确认",
}

// announceExclusionWords are the English words (whole words only) with the
// same effect.
var announceExclusionWords = map[string]bool{
	"you": true, "your": true, "yours": true, "please": true, "confirm": true, "once": true,
	"not": true, "won't": true, "don't": true, "can't": true, "cannot": true, "never": true,
}

// sentenceBreak splits a paragraph into sentences and lines.
var sentenceBreak = regexp.MustCompile(`[\n.。!！;；]`)

// lastParagraph is the last non-blank paragraph of s, trimmed.
func lastParagraph(s string) string {
	paras := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n")
	for i := len(paras) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(paras[i]); p != "" {
			return p
		}
	}
	return ""
}

// englishWords are the lower-case words of s (letters and apostrophes).
func englishWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && r != '\''
	})
}

// announcedNextStepWithoutAction reports whether a reply without tool calls
// ends by announcing work it did not start ("接下来我将修改 main.go。",
// "Let me now update the config."): a sentence of its last paragraph starts
// with an announcement. A question, a recommendation, a completion
// statement, anything addressed to the user or negated is not one.
func announcedNextStepWithoutAction(content string) bool {
	p := lastParagraph(content)
	if p == "" || utf8.RuneCountInString(p) > 300 {
		return false
	}
	if strings.HasSuffix(p, "?") || strings.HasSuffix(p, "？") || strings.Contains(p, "你") {
		return false
	}
	lower := strings.ToLower(p)
	for _, x := range announceExclusions {
		if strings.Contains(lower, x) {
			return false
		}
	}
	if hasCompletionMarker(lower) {
		return false
	}
	for _, w := range englishWords(lower) {
		if announceExclusionWords[w] {
			return false
		}
	}
	for _, s := range sentenceBreak.Split(lower, -1) {
		s = strings.TrimSpace(s)
		// "让我们回顾一下" invites the reader along; it announces no step.
		if strings.HasPrefix(s, "让我们") {
			continue
		}
		for _, m := range announceStartsZH {
			if strings.HasPrefix(s, m) {
				return true
			}
		}
		for _, m := range announceStartsEN {
			if strings.HasPrefix(s+" ", m) {
				return true
			}
		}
	}
	return false
}

// hasCJK reports whether s contains Han characters.
func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// degenerateEnding reports whether a final reply is too short to be an
// answer after a turn that did work (usedToolsThisTurn: a tool that is not
// read-only ran). Chinese carries more per character, so its bar is lower.
func degenerateEnding(content string, usedToolsThisTurn bool) bool {
	if !usedToolsThisTurn {
		return false
	}
	t := strings.TrimSpace(content)
	limit := degenerateEndingMaxRunes
	if hasCJK(t) {
		limit = degenerateEndingMaxRunesCJK
	}
	if utf8.RuneCountInString(t) >= limit {
		return false
	}
	return !hasCompletionMarker(strings.ToLower(t))
}

// noteWorkTool records, for degenerateEnding, that a tool which may change
// something ran this turn; read-only tools (reading, searching) do not count,
// so a short answer after a lookup is not taken for a fragment.
// A call that failed or was blocked changed nothing and does not count.
func (e *Engine) noteWorkTool(l *turnLimits, name, result string) {
	if strings.HasPrefix(result, "Error:") || strings.HasPrefix(result, "BLOCKED") {
		return
	}
	if t, ok := e.registry.Find(name); ok && t.Def().IsReadOnly {
		return
	}
	l.usedTools = true
}

// emptyOrThinkOnly reports whether a reply has no visible text and no tool
// calls: nothing at all, or only reasoning.
func emptyOrThinkOnly(resp *api.ChatResponse) bool {
	if resp == nil {
		return true
	}
	return strings.TrimSpace(resp.Content) == "" && len(resp.ToolCalls) == 0
}

// canNudge reports whether the turn may spend one more model call on a
// nudge: not cancelled, budget left, and the next call would not run into
// the iteration cap or the time limit (a nudge must never be what stops a
// turn that already has an answer).
func (e *Engine) canNudge(ctx context.Context, l *turnLimits, iter int) bool {
	if ctx.Err() != nil || e.costTracker.OverBudget() {
		return false
	}
	if l.iterWindow > 0 && iter+1 >= l.iterCap {
		return false
	}
	if l.timeWindow > 0 && time.Since(l.start) >= l.deadline {
		return false
	}
	return true
}

// stopNudge decides whether a reply without tool calls is sent back to the
// model instead of ending the turn, in this order: an empty reply, an
// announced-but-not-taken next step, a degenerate ending, the one-time done
// check. It returns the nudge ("" = accept the reply) and whether the reply
// itself goes into history first (an empty one never does).
func (e *Engine) stopNudge(ctx context.Context, l *turnLimits, iter int, resp *api.ChatResponse, routedModel string) (nudge string, keepReply bool) {
	if !e.canNudge(ctx, l, iter) {
		return "", false
	}
	if emptyOrThinkOnly(resp) {
		if l.emptyRetries < maxEmptyResponseRetries {
			l.emptyRetries++
			return nudgeEmptyText, false
		}
		return "", false
	}
	if l.continuationNudges < maxContinuationNudgesPerTurn {
		switch {
		case announcedNextStepWithoutAction(resp.Content):
			l.continuationNudges++
			return nudgeAnnouncedText, true
		case degenerateEnding(resp.Content, l.usedTools):
			l.continuationNudges++
			return nudgeDegenerateText, true
		}
	}
	if l.doneChecks < doneCheckOncePerTurn && e.filesChangedThisTurn() && e.doneCheckEnabled(routedModel) {
		l.doneChecks++
		return nudgeDoneCheckText, true
	}
	return "", false
}

// doneCheckEnabled reports whether the one-time done check applies to a turn
// on routedModel. "auto" (the default) limits it to the models most
// likely to stop early: the fast tier (isFastModelName, as weakModelGuidance)
// and any provider other than anthropic.
func (e *Engine) doneCheckEnabled(routedModel string) bool {
	switch strings.ToLower(strings.TrimSpace(e.config.DoneCheck)) {
	case "off":
		return false
	case "on":
		return true
	}
	return isFastModelName(routedModel) || e.fallback.Current().Name() != "anthropic"
}

// doneCheckNote is the dim line shown while the done check runs.
const doneCheckNote = "  \x1b[2m（自检中…）\x1b[0m"

// emitSeparator is called when a reply without tool calls did not end the
// turn (a nudge, the done check, a failed verify gate): the next reply is
// streamed straight after the first one, so a blank line keeps the two
// apart, as stopWithWrapUp does for its summary. -p (no onDelta) prints only
// the final text and gets nothing. doneCheck adds a dim note for the user.
func (e *Engine) emitSeparator(onDelta func(string), doneCheck bool) {
	if onDelta == nil {
		return
	}
	onDelta("\n\n")
	if doneCheck {
		e.engineOutput(doneCheckNote)
	}
}
