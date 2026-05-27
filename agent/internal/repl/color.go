package repl

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ANSI color codes
const (
	Reset     = "\x1b[0m"
	Bold      = "\x1b[1m"
	Dim       = "\x1b[2m"
	Italic    = "\x1b[3m"
	Underline = "\x1b[4m"

	// Foreground colors
	Black   = "\x1b[30m"
	Red     = "\x1b[31m"
	Green   = "\x1b[32m"
	Yellow  = "\x1b[33m"
	Blue    = "\x1b[34m"
	Magenta = "\x1b[35m"
	Cyan    = "\x1b[36m"
	White   = "\x1b[37m"
	Gray    = "\x1b[90m"

	// Bright foreground
	BrightRed    = "\x1b[91m"
	BrightGreen  = "\x1b[92m"
	BrightYellow = "\x1b[93m"
	BrightBlue   = "\x1b[94m"
	BrightCyan   = "\x1b[96m"
)

// Styled returns colored text
func Styled(color, text string) string {
	return color + text + Reset
}

// ToolResult formats a tool result line with appropriate color
func ToolResult(name, summary string, isError bool) string {
	nameColor := Cyan
	icon := "✓"
	summaryColor := ""
	if isError {
		nameColor = Red
		icon = "✗"
		summaryColor = Red
	}
	return fmt.Sprintf("  %s%s%s %s[%s]%s %s%s%s",
		Dim, icon, Reset,
		nameColor, name, Reset,
		summaryColor, summary, Reset)
}

// PermissionPrompt formats the permission request
func PermissionPrompt(toolName, desc string) string {
	return fmt.Sprintf("\n  %s⚠%s %sPermission required:%s [%s%s%s] %s",
		Yellow, Reset,
		Bold, Reset,
		Cyan, toolName, Reset,
		desc)
}

// Thinking indicator (spinner)
type Spinner struct {
	mu      sync.Mutex
	active  bool
	stopCh  chan struct{}
	message string
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func NewSpinner(message string) *Spinner {
	return &Spinner{message: message, stopCh: make(chan struct{})}
}

func (s *Spinner) Start() {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return
	}
	s.active = true
	s.mu.Unlock()

	go func() {
		i := 0
		for {
			select {
			case <-s.stopCh:
				fmt.Fprintf(os.Stderr, "\r\x1b[K") // Clear spinner line
				return
			default:
				frame := spinnerFrames[i%len(spinnerFrames)]
				fmt.Fprintf(os.Stderr, "\r  %s%s %s%s", Cyan, frame, s.message, Reset)
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()
}

func (s *Spinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active {
		return
	}
	s.active = false
	close(s.stopCh)
}

func (s *Spinner) SetMessage(msg string) {
	s.mu.Lock()
	s.message = msg
	s.mu.Unlock()
}

// Walking person animation - compact single-line indicator
type WalkingIndicator struct {
	mu      sync.Mutex
	active  bool
	stopCh  chan struct{}
	message string
}

// Compact walking frames: a tiny figure with animated footstep trail
var walkFrames = []string{
	"⟩ ∙   ∙   ∙",
	"⟩  ∙   ∙   ",
	"⟩   ∙   ∙  ",
	"⟩    ∙   ∙ ",
	"⟩ ∙   ∙   ∙",
	"⟩  ∙   ∙   ",
}

func NewWalkingIndicator(message string) *WalkingIndicator {
	return &WalkingIndicator{message: message, stopCh: make(chan struct{})}
}

func (w *WalkingIndicator) Start() {
	w.mu.Lock()
	if w.active {
		w.mu.Unlock()
		return
	}
	w.active = true
	w.mu.Unlock()

	go func() {
		i := 0
		for {
			select {
			case <-w.stopCh:
				fmt.Fprintf(os.Stderr, "\r\x1b[K")
				return
			default:
				frame := walkFrames[i%len(walkFrames)]
				w.mu.Lock()
				msg := w.message
				w.mu.Unlock()
				fmt.Fprintf(os.Stderr, "\r  %s%s%s %s%s%s",
					Cyan, frame, Reset, Dim, msg, Reset)
				i++
				time.Sleep(120 * time.Millisecond)
			}
		}
	}()
}

func (w *WalkingIndicator) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active {
		return
	}
	w.active = false
	close(w.stopCh)
	time.Sleep(20 * time.Millisecond) // let goroutine clean up
}

func (w *WalkingIndicator) SetMessage(msg string) {
	w.mu.Lock()
	w.message = msg
	w.mu.Unlock()
}

// Banner renders a styled startup banner
func Banner(version, model, provider, mode, cwd, gitBranch, gitStatus string, toolCount int, isGit bool) string {
	var sb strings.Builder

	sb.WriteString("\n")
	sb.WriteString(Styled(BrightCyan+Bold, "  ╭─────────────────────────────────────────╮") + "\n")
	sb.WriteString(fmt.Sprintf("  %s│%s  %s◆ agentgo%s %-30s%s│%s\n",
		BrightCyan+Bold, Reset,
		BrightCyan+Bold, Reset,
		"v"+version,
		BrightCyan+Bold, Reset))
	sb.WriteString(Styled(BrightCyan+Bold, "  ╰─────────────────────────────────────────╯") + "\n")
	sb.WriteString("\n")

	// Info line
	sb.WriteString(fmt.Sprintf("  %sModel:%s %s%s%s", Dim, Reset, Bold, model, Reset))
	sb.WriteString(fmt.Sprintf("  %s│%s  %sProvider:%s %s", Dim, Reset, Dim, Reset, provider))
	sb.WriteString(fmt.Sprintf("  %s│%s  %sMode:%s %s\n", Dim, Reset, Dim, Reset, mode))

	if isGit {
		sb.WriteString(fmt.Sprintf("  %sGit:%s %s%s%s %s(%s)%s\n",
			Dim, Reset, Green, gitBranch, Reset, Dim, gitStatus, Reset))
	}
	sb.WriteString(fmt.Sprintf("  %sCWD:%s %s\n", Dim, Reset, cwd))
	sb.WriteString(fmt.Sprintf("  %sTools:%s %d\n", Dim, Reset, toolCount))
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  %sTip: Type %s/%s%s for commands, %sCtrl+C%s to interrupt%s\n",
		Dim, Reset, "help", Dim, Reset, Dim, Reset))
	sb.WriteString("\n")

	return sb.String()
}

// Prompt returns the styled input prompt
func Prompt() string {
	return BrightCyan + "❯ " + Reset
}

// SectionHeader renders a section label
func SectionHeader(label string) string {
	return fmt.Sprintf("\n  %s%s─── %s ───%s\n", Dim, "", label, Reset)
}
