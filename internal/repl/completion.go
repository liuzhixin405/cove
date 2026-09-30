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
	// The cursor indexes the rune buffer, so it is the completion's rune
	// count. It used to be its byte length: "请看 @internal/" (16 bytes, 12
	// runes) put the cursor past the end and the redraw panicked.
	if len(suggestions) == 1 {
		*buf = []rune(texts[0])
		*cursor = len(*buf)
		lr.resetCompletionCycle()
		lr.redraw(*buf, *cursor)
		return
	}
	common := commonPrefix(texts)
	if len(common) > len(line) {
		*buf = []rune(common)
		*cursor = len(*buf)
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
	*buf = []rune(next)
	*cursor = len(*buf) // runes, not bytes (see complete)
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

// commonPrefix is the longest prefix of whole characters the candidates
// share. It used to trim a byte at a time, so "@档案" and "@案卷" (档 and 案
// share their first two bytes) left half a character, shown as U+FFFD.
func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := []rune(ss[0])
	for _, s := range ss[1:] {
		n := 0
		for _, r := range s {
			if n >= len(p) || p[n] != r {
				break
			}
			n++
		}
		p = p[:n]
	}
	return string(p)
}
