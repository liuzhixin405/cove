package repl

import "github.com/liuzhixin405/cove-agent/internal/termui"

// ANSI color codes: termui's, so the two packages cannot drift apart (they
// were two copies of the same table). The spinner, walking indicator,
// permission box, tool line and banner that used to be copied here as well
// are termui's alone now: nothing used these copies.
const (
	Reset          = termui.Reset
	Bold           = termui.Bold
	Dim            = termui.Dim
	Italic         = termui.Italic
	Underline      = termui.Underline
	Black          = termui.Black
	Red            = termui.Red
	Green          = termui.Green
	Yellow         = termui.Yellow
	Blue           = termui.Blue
	Magenta        = termui.Magenta
	Cyan           = termui.Cyan
	White          = termui.White
	Gray           = termui.Gray
	BrightRed      = termui.BrightRed
	BrightGreen    = termui.BrightGreen
	BrightYellow   = termui.BrightYellow
	BrightBlue     = termui.BrightBlue
	BrightCyan     = termui.BrightCyan
	ReasoningStyle = termui.ReasoningStyle
)

// Prompt returns the styled input prompt
func Prompt() string {
	return BrightCyan + "❯ " + Reset
}

// PromptRunning returns the prompt shown when a background task is running.
func PromptRunning() string {
	return Yellow + "⚡ ❯ " + Reset
}
