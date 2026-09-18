// Package render turns one conversation event into the text that appears in the
// terminal. It is the single rendering layer shared by every front end.
//
// Why this package exists separately from internal/tui:
//
// The old code fused two unrelated concerns. Producing the text for a tool call
// lived in internal/engine (formatToolLine, summarizeResult), producing it for
// the classic path lived in internal/termui (ToolResult), and the full-screen
// layout lived in internal/tui — so the same event had three renderings that
// had already drifted apart (engine's copy had even lost its ✓/✗ glyphs, making
// success and failure visually identical). Anything that needs to show a
// conversation now asks this package for the text and decides only WHERE to put
// it: a scrollback line, a viewport, or a log file.
//
// Everything here is a pure function of its inputs. No I/O, no globals, no
// terminal detection — which is what makes the layout testable at all.
package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// Kind classifies a block so the renderer can pick its gutter symbol and
// collapse rule.
type Kind int

const (
	// KindUser is what the user typed.
	KindUser Kind = iota
	// KindThinking is model reasoning. Always collapsed to a single line: the
	// content is rarely what the user is looking for, and it is the single
	// largest source of screen noise.
	KindThinking
	// KindTool is one tool invocation plus its result.
	KindTool
	// KindAnswer is the model's prose answer. Never collapsed.
	KindAnswer
	// KindSystem is engine output that belongs to no turn (resume notices,
	// command results, errors).
	KindSystem
)

// Block is one renderable conversation event.
//
// Header/Summary are what the collapsed form shows; Full is what the expanded
// form shows. A Block with an empty Full is not expandable, and the renderer
// then omits the expand hint entirely — an affordance that does nothing is
// worse than no affordance.
type Block struct {
	// ID is a short per-session handle used by the expand command ("a4", "t1").
	// Empty means the block can never be referred back to.
	ID string

	Kind Kind

	// Tool is the tool name for KindTool ("bash", "read", …).
	Tool string

	// Header is the one-line title: the command, the path, the query. It is
	// rendered without the gutter, which the renderer adds.
	Header string

	// Summary is the single "⎿" line under the header: "3 matches",
	// "+52 −8", "exit=0 · 输出 12 行".
	Summary string

	// Full is the complete payload. Empty = nothing worth expanding.
	Full string

	// FullPath, when set, is where Full was spilled to disk because it was too
	// large to keep in memory or to reprint. The expanded form points at it
	// instead of dumping it.
	FullPath string

	// IsError marks a failed tool call or an engine error.
	IsError bool

	// Duration is how long the step took. Zero means "do not show".
	Duration time.Duration
}

// Expandable reports whether this block has hidden content to show.
func (b Block) Expandable() bool {
	return b.ID != "" && (b.Full != "" || b.FullPath != "")
}

// ---------------------------------------------------------------------------
// Tool classification
// ---------------------------------------------------------------------------

// collapsePolicy says how much of a tool's output is worth keeping.
type collapsePolicy int

const (
	// policySummaryOnly: the summary line says everything. Expanding adds
	// nothing, so no hint is offered (read: "78 lines" — the lines themselves
	// are the file, which the user can open).
	policySummaryOnly collapsePolicy = iota
	// policyExpandable: the full output is worth a look on demand.
	policyExpandable
	// policyAlwaysExpandable: the output is the point of the call and is
	// routinely long (bash, sub-agents). Always offer the hint.
	policyAlwaysExpandable
)

// toolPolicy maps every tool cove registers to how its output collapses.
//
// The map is explicit rather than heuristic so that adding a tool forces a
// decision here; an unlisted tool falls back to policyExpandable, which is the
// safe default (offering an expand hint that turns out to be dull is better
// than silently hiding output the user needed).
var toolPolicy = map[string]collapsePolicy{
	// Reading a file: the summary (line count) is the whole story.
	"read": policySummaryOnly,

	// Searching: the match list is worth seeing.
	"grep": policyExpandable,
	"glob": policyExpandable,

	// Editing: the diff is worth seeing.
	"edit":  policyExpandable,
	"write": policyExpandable,

	// Executing: the output IS the result, and it is routinely long.
	"bash":       policyAlwaysExpandable,
	"powershell": policyAlwaysExpandable,

	// Network: response bodies are long and often the point.
	"webfetch":  policyExpandable,
	"websearch": policyExpandable,
	"browser":   policyExpandable,

	// Sub-agents: dozens of nested tool calls. Must never be inlined.
	"agent":        policyAlwaysExpandable,
	"execute_plan": policyAlwaysExpandable,

	// Bookkeeping: a one-line result is complete.
	"task_create":  policySummaryOnly,
	"task_list":    policyExpandable,
	"task_update":  policySummaryOnly,
	"task_stop":    policySummaryOnly,
	"task_get":     policyExpandable,
	"task_output":  policyAlwaysExpandable,
	"team_create":  policySummaryOnly,
	"team_delete":  policySummaryOnly,
	"send_message": policySummaryOnly,
	"cron":         policySummaryOnly,
	"todowrite":    policyExpandable,

	// Mode switches: state changes, nothing to expand.
	"plan_mode":      policySummaryOnly,
	"exit_plan_mode": policySummaryOnly,
	"worktree":       policySummaryOnly,
	"exit_worktree":  policySummaryOnly,

	// MCP: opaque third-party output.
	"mcp":               policyExpandable,
	"mcp_resources":     policyExpandable,
	"mcp_read_resource": policyExpandable,

	// Misc.
	"skill":      policyExpandable,
	"brief":      policyExpandable,
	"sleep":      policySummaryOnly,
	"lsp":        policyExpandable,
	"draw_image": policySummaryOnly,
	"question":   policySummaryOnly,
}

// policyFor returns the collapse policy for a tool name.
func policyFor(tool string) collapsePolicy {
	if p, ok := toolPolicy[strings.ToLower(strings.TrimSpace(tool))]; ok {
		return p
	}
	return policyExpandable
}

// ---------------------------------------------------------------------------
// Summary construction
// ---------------------------------------------------------------------------

// maxSummaryRunes bounds the "⎿" line. It is deliberately short: the summary
// exists to let the eye skip the block, not to convey the content.
const maxSummaryRunes = 72

// maxHeaderRunes bounds the header before the renderer applies the terminal
// width. A command longer than this is not readable on one line anyway.
const maxHeaderRunes = 120

// ToolBlock builds a collapsed-by-default Block for one tool invocation.
//
// header is the human-facing target (the command, the path, the query) and
// output is the tool's raw result. The caller supplies summary when it can say
// something better than the generic line count — "+52 −8" from an edit, say.
func ToolBlock(id, tool, header, summary, output string, isError bool, d time.Duration) Block {
	b := Block{
		ID:       id,
		Kind:     KindTool,
		Tool:     tool,
		Header:   textutil.ClipRunes(oneLine(header), maxHeaderRunes),
		IsError:  isError,
		Duration: d,
	}

	if summary == "" {
		summary = defaultSummary(output, isError)
	}
	b.Summary = textutil.ClipRunes(oneLine(summary), maxSummaryRunes)

	// An error's full output is always worth keeping, whatever the policy: the
	// reason a call failed is exactly what the user needs next.
	if isError || policyFor(tool) != policySummaryOnly {
		if strings.TrimSpace(output) != "" && moreThanSummary(output, b.Summary) {
			b.Full = output
		}
	}
	return b
}

// ThinkingBlock builds the always-collapsed reasoning block.
func ThinkingBlock(id, reasoning string, d time.Duration, tokens int) Block {
	b := Block{
		ID:       id,
		Kind:     KindThinking,
		Duration: d,
	}
	var parts []string
	if d > 0 {
		parts = append(parts, humanDuration(d))
	}
	if tokens > 0 {
		parts = append(parts, humanCount(tokens)+" tokens")
	}
	// Header carries only the metadata (duration, tokens). The "思考" label
	// belongs to the renderer, which always prints it — putting a fallback
	// here as well produced "思考 思考" whenever there was no metadata.
	b.Header = strings.Join(parts, " · ")
	if strings.TrimSpace(reasoning) != "" {
		b.Full = reasoning
	}
	return b
}

// UserBlock builds the block for what the user typed.
func UserBlock(text string) Block {
	return Block{Kind: KindUser, Header: strings.TrimSpace(text)}
}

// AnswerBlock builds the block for the model's prose. Never collapsed.
func AnswerBlock(text string) Block {
	return Block{Kind: KindAnswer, Header: strings.TrimSpace(text)}
}

// SystemBlock builds the block for engine output that belongs to no turn.
func SystemBlock(text string, isError bool) Block {
	return Block{Kind: KindSystem, Header: strings.TrimSpace(text), IsError: isError}
}

// defaultSummary derives a one-line summary from raw tool output when the
// caller has nothing better.
func defaultSummary(output string, isError bool) string {
	s := strings.TrimSpace(output)
	if s == "" {
		if isError {
			return "失败（无输出）"
		}
		return "完成"
	}
	lines := strings.Count(s, "\n") + 1
	first := oneLine(firstLine(s))
	if lines == 1 {
		return first
	}
	// Lead with the first line, which is where tools put their verdict, and
	// state how much was withheld so the user can judge whether to expand.
	return fmt.Sprintf("%s · 共 %d 行", textutil.ClipRunes(first, maxSummaryRunes-12), lines)
}

// moreThanSummary reports whether output carries information the summary does
// not already show. It is what suppresses a useless expand hint on a
// single-line result.
func moreThanSummary(output, summary string) bool {
	s := strings.TrimSpace(output)
	if strings.Count(s, "\n") > 0 {
		return true
	}
	return oneLine(s) != strings.TrimSuffix(summary, "…")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// oneLine collapses whitespace so a value can safely occupy a single row.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// humanDuration renders a step duration compactly ("1.2s", "340ms", "2m05s").
func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Minute:
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	case d >= time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
}

// humanCount renders large counts compactly (1234 -> 1.2k).
func humanCount(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
