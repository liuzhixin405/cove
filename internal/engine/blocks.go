package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/liuzhixin405/cove/internal/render"
	"github.com/liuzhixin405/cove/internal/textmode"
	"github.com/liuzhixin405/cove/internal/textutil"
)

// Structured conversation events.
//
// The engine used to describe a tool call by building a coloured string inline
// (formatToolLine) and pushing it through engineOutput as an opaque line. That
// had two consequences worth naming, because they are exactly what this file
// removes:
//
//   - The front end received text, not an event, so it could not offer to
//     expand anything. Everything a tool printed was either dumped in full or
//     lost. Collapsing by default is only possible once the full output travels
//     alongside the summary, which needs a struct.
//   - The rendering drifted. engine's copy had lost its ✓/✗ glyphs entirely and
//     printed a literal "?" for BOTH success and failure, so the two states were
//     indistinguishable the moment colour was off or stripped.
//
// So the engine now emits render.Block and the front end decides how to show
// it. render is a pure function of the block, which is what makes the layout
// testable.

// blockIDSeq numbers blocks within the process. It is a package-level counter
// rather than per-Engine so IDs stay unique when a sub-agent runs its own
// Engine against the same front end — two blocks sharing an ID would make "/x"
// ambiguous, and the user would get the wrong output with no way to tell.
var blockIDSeq atomic.Uint64

// nextBlockID returns a short handle the user can type ("7", "42").
//
// Short matters more than descriptive: the ID's only job is to be re-typed
// after "/x", and a scheme like "tool-00007" costs keystrokes for nothing.
func nextBlockID() string {
	return strconv.FormatUint(blockIDSeq.Add(1), 10)
}

// legacyBlockWidth is the width used when rendering a block back down to a line
// for the deprecated OnEngineOutput callback. It is generous because those
// front ends re-wrap or clip the text themselves; the ID is stripped first so
// no expand hint is right-aligned against a width that is not the real one.
const legacyBlockWidth = 120

// blockStyles is how a block is coloured on its way to the terminal.
//
// The glyph set is chosen once, at startup, from the console's code page. On a
// console that is not UTF-8 the disclosure marker and the ✓/✗ pair come out as
// mojibake — and a "?" standing for both success and failure is how those two
// states became indistinguishable in the first place.
var blockStyles = newBlockStyles()

func newBlockStyles() render.Styles {
	st := render.Styles{
		ToolName: func(s string) string { return "\x1b[36m" + s + "\x1b[0m" },
		Summary:  func(s string) string { return "\x1b[2m" + s + "\x1b[0m" },
		Thinking: func(s string) string { return "\x1b[2m" + s + "\x1b[0m" },
		Err:      func(s string) string { return "\x1b[31m" + s + "\x1b[0m" },
		Hint:     func(s string) string { return "\x1b[2m" + s + "\x1b[0m" },
	}
	if textmode.PreferASCII() {
		st.Glyphs = render.ASCIIGlyphs()
	}
	return st
}

// emitBlock sends one structured event to the front end.
//
// A sink gets the block itself, so it can keep the full output and expand it
// later. A front end still on the deprecated callback gets the collapsed
// rendering as a line, because that is all its interface can carry.
func (e *Engine) emitBlock(b render.Block) {
	if e.out != nil {
		e.out.Block(b)
		return
	}
	if e.OnEngineOutput != nil {
		e.OnEngineOutput(render.Collapsed(withoutID(b), legacyBlockWidth, blockStyles) + "\n")
	}
}

func withoutID(b render.Block) render.Block {
	b.ID = ""
	return b
}

// emitToolResult builds and emits the block for one finished tool call.
//
// summary is left to render.ToolBlock unless the caller knows something better;
// output is the raw tool result, which the block carries in full so the front
// end can expand it on demand.
func (e *Engine) emitToolResult(name string, input map[string]any, output string, isError bool, d time.Duration) {
	e.emitBlock(render.ToolBlock(
		nextBlockID(),
		name,
		toolHeaderFor(name, input),
		"",
		output,
		isError,
		d,
	))
}

// maxToolHeaderRunes bounds the header the engine derives before render clips
// it again to the terminal width. It exists so a pathological argument (a
// base64 blob in a write, a 4KB heredoc in a bash command) does not travel
// through the whole pipeline just to be thrown away at the last step.
const maxToolHeaderRunes = 160

// toolHeaderFor derives the human-facing target of a tool call: the command for
// a shell, the path for a file operation, the pattern for a search.
//
// It reads the argument names the tools actually declare rather than guessing
// from the first string value, because the first key of a map is unordered in
// Go and a heuristic here would pick a different field between runs.
func toolHeaderFor(name string, input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	lower := strings.ToLower(strings.TrimSpace(name))

	// Per-tool primary argument, plus a qualifier when one argument alone is
	// ambiguous ("grep TODO" says nothing about where it looked).
	switch lower {
	case "bash", "powershell":
		return clipHeader(str(input, "command"))
	case "read", "write", "edit":
		h := clipHeader(firstStr(input, "filePath", "file_path", "path"))
		if lower == "read" {
			if r := str(input, "range"); r != "" {
				h += " " + r
			}
		}
		return h
	case "grep":
		h := firstStr(input, "pattern", "query")
		if p := firstStr(input, "path", "dir"); p != "" {
			h += " in " + p
		}
		return clipHeader(h)
	case "glob":
		h := firstStr(input, "pattern", "glob")
		if p := firstStr(input, "path", "dir"); p != "" {
			h += " in " + p
		}
		return clipHeader(h)
	case "webfetch", "browser":
		return clipHeader(firstStr(input, "url", "prompt"))
	case "websearch":
		return clipHeader(firstStr(input, "query", "q"))
	case "agent", "execute_plan":
		return clipHeader(firstStr(input, "description", "prompt", "task"))
	case "skill":
		return clipHeader(firstStr(input, "skill", "name"))
	case "todowrite":
		return "" // The summary carries the counts; a header would repeat them.
	}

	// Unknown tool (MCP, plugin-provided): fall back to the conventional
	// argument names, then to the target path the compressor already knows how
	// to find, so a new tool still gets a useful header for free.
	if h := firstStr(input, "command", "filePath", "file_path", "path", "pattern", "query", "url", "name", "description"); h != "" {
		return clipHeader(h)
	}
	return clipHeader(toolTargetPath(input))
}

func clipHeader(s string) string {
	return textutil.ClipRunes(strings.TrimSpace(s), maxToolHeaderRunes)
}

// str returns input[key] when it is a non-empty string.
func str(input map[string]any, key string) string {
	if v, ok := input[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// firstStr returns the first key present as a non-empty string. Numbers are
// accepted too: several tools declare an integer argument (a line number, a
// task index) that reads perfectly well in a header.
func firstStr(input map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := str(input, k); s != "" {
			return s
		}
		switch v := input[k].(type) {
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case int:
			return strconv.Itoa(v)
		case bool:
			return fmt.Sprintf("%t", v)
		}
	}
	return ""
}
