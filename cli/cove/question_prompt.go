package main

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/engine"
	"github.com/liuzhixin405/cove-agent/internal/mcp"
	"github.com/liuzhixin405/cove-agent/internal/render"
	"github.com/liuzhixin405/cove-agent/internal/repl"
	"github.com/liuzhixin405/cove-agent/internal/termui"
)

// questionPromptTimeout bounds how long the question tool waits for an
// answer; an unanswered question gets an empty answer. A variable for tests.
var questionPromptTimeout = 15 * time.Minute

// installQuestionPrompt gives the question tool a way to ask: Runtime.AskUser
// was never assigned, so the tool always answered "requires interactive
// mode". It uses the answer relay the permission prompt uses
// (repl.SetPromptInput), so only the REPL installs it; -p and headless runs
// do not register the tool at all.
func installQuestionPrompt(eng *engine.Engine) {
	if eng == nil || eng.Runtime() == nil {
		return
	}
	eng.Runtime().AskUser = askUserQuestion
}

// askUserQuestion shows prompt above the input line and returns the next
// line the user enters, or "" when nothing reads answers or on timeout.
// Ctrl+C at the question returns promptInterrupt (tool.AskUserCancelled),
// which ends the question tool.
func askUserQuestion(prompt string) string {
	if !replInteractive {
		return ""
	}
	// The prompt is the model's text (header, question, option labels and
	// descriptions) and was printed raw: a question carrying cursor-up and
	// erase-line sequences repainted the option list above before the user
	// pressed a digit. Show controls as inert text, as the permission box
	// does. The option lines are taken from the shown text too, since they
	// are displayed again on the input line when picked with ↑↓; escaping
	// keeps every '\n', so the options and their numbers do not change.
	prompt = render.VisibleControls(prompt)
	options, keys := questionOptions(prompt)
	hint := "输入选项编号或直接输入回答"
	if len(options) > 0 {
		hint = "按编号键直接选择，↑↓ 挑选后回车，或直接输入回答"
	}
	text := termui.Styled(termui.Bold, strings.TrimRight(prompt, "\n")) + "\n  " +
		termui.Styled(termui.Dim, hint) + "\n"
	answer, ok := repl.AskWith(repl.AskSpec{Text: text, Hint: "模型在等你回答上面的问题：" + hint,
		Timeout: questionPromptTimeout, Keys: keys, Options: options})
	if !ok {
		termui.PrintAbove("  " + termui.Styled(termui.Dim, "等待回答超时，按未回答处理") + "\n")
	}
	// An option picked with ↑↓ arrives as its line ("2. 标签: 说明"); the
	// tool takes the number.
	if m := questionOptionLine.FindStringSubmatch(answer); m != nil && optionIndex(options, answer) >= 0 {
		return m[1]
	}
	return answer
}

// questionOptionLine is one option of the question tool's prompt: "  2. …".
var questionOptionLine = regexp.MustCompile(`^\s*(\d+)\. (.+)$`)

// questionOptions returns the prompt's option lines, as the input line shows
// them when picked with ↑↓, and the keys that pick them on their own (the
// numbers, with at most 9 options).
func questionOptions(prompt string) (options []string, keys string) {
	for _, l := range strings.Split(prompt, "\n") {
		if m := questionOptionLine.FindStringSubmatch(l); m != nil {
			options = append(options, strings.TrimSpace(l))
		}
	}
	if n := len(options); n > 0 && n <= 9 {
		for i := 1; i <= n; i++ {
			keys += strconv.Itoa(i)
		}
	}
	return options, keys
}

func optionIndex(options []string, s string) int {
	for i, o := range options {
		if o == strings.TrimSpace(s) {
			return i
		}
	}
	return -1
}

// wireToolDefsVersion makes the engine rebuild its tool definitions when the
// MCP pool's tool list changes (/mcp connect, tools/list_changed): the mcp
// proxy's description lists the pool's tools.
func wireToolDefsVersion(eng *engine.Engine, pool *mcp.Pool) {
	if eng == nil || pool == nil {
		return
	}
	eng.SetToolDefsVersion(func() int { return int(pool.Version()) })
}
