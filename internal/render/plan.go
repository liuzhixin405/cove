package render

import (
	"fmt"
	"regexp"
	"strings"
)

// The task list a todowrite call sets used to show as one line, "Task list
// (5 items): · 共 6 行", so the plan was visible to the model and nobody else.
// It is drawn as the list itself:
//
//	  ▸ 计划 2/5 完成
//	    ✓ 读懂解析器
//	    ▶ 修复分词器
//	    ○ 补测试

// planItem is one line of todowrite's result: "[✓] todo-2. text [high]".
var planItem = regexp.MustCompile(`^\[(.)\] todo-\d+\. (.*?)(?: \[[a-z]*\])?$`)

// planMaxItems bounds the items drawn; the rest are counted.
const planMaxItems = 12

// planGlyphs maps todowrite's marks to the drawn state glyphs (Unicode and
// ASCII).
func planGlyph(mark string, ascii bool) string {
	switch mark {
	case "✓":
		if ascii {
			return "[x]"
		}
		return "✓"
	case ">":
		if ascii {
			return "[>]"
		}
		return "▶"
	case "x":
		if ascii {
			return "[-]"
		}
		return "✗"
	}
	if ascii {
		return "[ ]"
	}
	return "○"
}

// planView renders a todowrite block as the list; ok is false when its
// output is not a task list.
func planView(b Block, width int, st Styles) (string, bool) {
	text := b.Full
	if text == "" {
		return "", false
	}
	type item struct{ mark, text string }
	var items []item
	for _, l := range strings.Split(text, "\n") {
		if m := planItem.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			items = append(items, item{m[1], m[2]})
		}
	}
	if len(items) == 0 {
		return "", false
	}
	done := 0
	for _, it := range items {
		if it.mark == "✓" {
			done++
		}
	}
	ascii := st.glyphs().Cont != UnicodeGlyphs().Cont
	var sb strings.Builder
	sb.WriteString(clipLine(gutter+apply(st.ToolName, st.glyphs().Leaf+" 计划")+" "+apply(st.Summary, fmt.Sprintf("%d/%d 完成", done, len(items))), width))
	// While the list is long, the finished items give way to the rest.
	shown := items
	if len(items) > planMaxItems {
		var open []item
		for _, it := range items {
			if it.mark != "✓" && it.mark != "x" {
				open = append(open, it)
			}
		}
		shown = open
		if len(shown) > planMaxItems {
			shown = shown[:planMaxItems]
		}
	}
	for _, it := range shown {
		line := planGlyph(it.mark, ascii) + " " + it.text
		switch it.mark {
		case "✓", "x":
			line = apply(st.Summary, line)
		case ">":
			line = apply(st.ToolName, line)
		}
		sb.WriteString("\n" + clipLine(contIndent+line, width))
	}
	if hidden := len(items) - len(shown); hidden > 0 {
		sb.WriteString("\n" + contIndent + apply(st.Summary, fmt.Sprintf("… 另有 %d 项已完成或未显示", hidden)))
	}
	return sb.String(), true
}
