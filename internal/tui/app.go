package tui

import (
	tea "charm.land/bubbletea/v2"
)

// App wraps a tea.Program and the root Model, exposing thread-safe helpers for
// background goroutines (e.g. the engine streaming callbacks) to push updates
// into the UI via (*tea.Program).Send.
type App struct {
	model   *Model
	program *tea.Program
}

// NewApp builds an App. onSubmit is called on the UI goroutine when the user
// submits an input line; the caller typically forwards it to the task runner.
// onResume is called with a session ID when the user picks a history entry.
// onInterrupt is called when the user requests cancel from the main view (Esc).
// commands is the static catalog shown in the / command palette.
func NewApp(modelName string, onSubmit, onResume func(string), onInterrupt func(), commands []CommandItem) *App {
	m := New(modelName, onSubmit, onResume, onInterrupt, commands)
	// In Bubble Tea v2 the alternate screen is declared per-frame on the
	// tea.View (see Model.View), not as a program option.
	p := tea.NewProgram(m)
	return &App{model: m, program: p}
}

// Run starts the UI event loop and blocks until the user quits.
func (a *App) Run() error {
	_, err := a.program.Run()
	return err
}

// Quit asks the program to terminate.
func (a *App) Quit() { a.program.Quit() }

// --- Bridge helpers: engine/task callbacks -> UI messages ---

// BeginStream marks the start of an assistant response. echo, if non-empty, is
// appended to the transcript first (e.g. a heading).
func (a *App) BeginStream(echo string) { a.program.Send(streamBeginMsg{echo: echo}) }

// Delta appends a streamed answer chunk.
func (a *App) Delta(s string) { a.program.Send(streamDeltaMsg(s)) }

// Reasoning appends a streamed reasoning/thinking chunk (rendered dim).
func (a *App) Reasoning(s string) { a.program.Send(streamReasoningMsg(s)) }

// EngineLine appends a diagnostic line from the engine (tool start/finish, etc).
func (a *App) EngineLine(s string) { a.program.Send(engineLineMsg(s)) }

// EndStreamAlign marks the end of an assistant response and provides the
// ground-truth final text. If any streaming deltas were dropped by the
// message channel, the UI model fills the gap to prevent truncation.
func (a *App) EndStreamAlign(final string) { a.program.Send(streamEndMsg{final: final}) }

// SetTask updates the task-queue sidebar and status bar.
func (a *App) SetTask(info TaskInfo) { a.program.Send(taskStateMsg(info)) }

// SetStatus updates the top status bar (model, tokens, elapsed).
func (a *App) SetStatus(info StatusInfo) { a.program.Send(statusUpdateMsg(info)) }

// SetHistory updates the history overlay entries.
func (a *App) SetHistory(items []HistoryItem) { a.program.Send(historyMsg(items)) }

// SetActivity sets the transient activity line shown above the input.
func (a *App) SetActivity(s string) { a.program.Send(activityMsg(s)) }

// ClearActivity hides the transient activity line.
func (a *App) ClearActivity() { a.program.Send(activityMsg("")) }

// RequestPermission shows the interactive permission overlay and BLOCKS the
// calling (worker) goroutine until the user answers. It is safe to call from a
// background goroutine: the reply travels back over a buffered channel so the UI
// goroutine never blocks delivering the decision. tool is the tool name and
// desc a short human-readable summary (file path, command, …) for the prompt.
func (a *App) RequestPermission(tool, desc string) PermDecision {
	ch := make(chan PermDecision, 1)
	a.program.Send(permRequestMsg(permRequest{tool: tool, desc: desc, reply: ch}))
	return <-ch
}
