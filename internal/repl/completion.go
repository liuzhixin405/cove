package repl

// Tab completion and the inline command hints.

import (
	"fmt"
	"strings"
)

func (lr *LineReader) showInlineSuggestions(suggestions []string, offset int) {
	const maxHints = 8
	consoleMu.Lock()
	defer consoleMu.Unlock()
	// Candidates are whole lines; the hint shows only the word being
	// completed ("@internal/" rather than the sentence before it).
	line := string(lr.renderBuf)
	lead := line[:strings.LastIndexAny(line, " \t")+1]
	printHints := func() string {
		var sb strings.Builder
		sb.WriteString("\x1b[90m  ")
		for i, s := range suggestions {
			if i >= maxHints {
				break
			}
			text := s
			if idx := strings.IndexByte(s, '\t'); idx >= 0 {
				text = s[:idx]
			}
			if lead != "" {
				text = strings.TrimPrefix(text, lead)
			}
			sb.WriteString(text + "  ")
		}
		if len(suggestions) > maxHints {
			fmt.Fprintf(&sb, "...(+%d)", len(suggestions)-maxHints)
		}
		sb.WriteString("\x1b[0m")
		return sb.String()
	}
	lr.activeHint = printHints()
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) showCommandCountHint(count int, offset int) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	hint := fmt.Sprintf("\x1b[90m  (按 Tab 查看 %d 个命令)\x1b[0m", count)
	lr.activeHint = hint
	lr.redrawLocked(lr.renderBuf, lr.renderCursor)
}

func (lr *LineReader) complete(buf *[]rune, cursor *int) {
	if lr.advanceCompletionCycle(buf, cursor) {
		return
	}
	if lr.completer == nil {
		return
	}
	line := string(*buf)
	suggestions := lr.completer(line)
	if len(suggestions) == 0 {
		lr.resetCompletionCycle()
		return
	}
	texts := completionTexts(suggestions)
	if len(suggestions) == 1 {
		*buf, *cursor = []rune(texts[0]), len(texts[0])
		lr.resetCompletionCycle()
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(texts)
	if len(common) > len(line) {
		*buf, *cursor = []rune(common), len(common)
		lr.completionBase, lr.completionList, lr.completionIdx = common, texts, -1
		lr.redraw(*buf, *cursor)
		return
	}
	lr.completionBase, lr.completionList, lr.completionIdx = line, texts, -1
	lr.showInlineSuggestions(suggestions, lr.promptWidth+*cursor)
}

func (lr *LineReader) advanceCompletionCycle(buf *[]rune, cursor *int) bool {
	next, idx, ok := completionCycleNext(string(*buf), lr.completionBase, lr.completionList, lr.completionIdx)
	if !ok {
		lr.resetCompletionCycle()
		return false
	}
	lr.completionIdx = idx
	*buf, *cursor = []rune(next), len(next)
	lr.redraw(*buf, *cursor)
	return true
}

func completionCycleNext(line, base string, list []string, idx int) (string, int, bool) {
	if len(list) == 0 {
		return "", idx, false
	}
	if line != base {
		current := ""
		if idx >= 0 && idx < len(list) {
			current = list[idx]
		}
		if line != current {
			return "", idx, false
		}
	}
	nextIdx := (idx + 1) % len(list)
	return list[nextIdx], nextIdx, true
}

func (lr *LineReader) resetCompletionCycle() {
	lr.completionBase, lr.completionList, lr.completionIdx = "", nil, -1
}

func completionTexts(ss []string) []string {
	res := make([]string, len(ss))
	for i, s := range ss {
		text := s
		if idx := strings.IndexByte(s, '\t'); idx >= 0 {
			text = s[:idx]
		}
		res[i] = text
	}
	return res
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	return p
}
