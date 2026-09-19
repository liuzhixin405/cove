package repl

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/textutil"
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

	// Reasoning
	ReasoningStyle = "\x1b[2;3m\x1b[90m" // Dim + Italic + Gray
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

// PermissionPrompt formats the permission request as a distinct block.
// Starts with \r\x1b[K to clear any spinner, then \a for terminal bell.
func PermissionPrompt(toolName, desc string) string {
	var sb strings.Builder
	sb.WriteString("\r\x1b[K") // clear spinner line
	sb.WriteString("\a")       // terminal bell to alert user
	fmt.Fprintf(&sb, "\n  %s╭── 需要授权 ──────────────────────╮%s\n", Yellow, Reset)
	fmt.Fprintf(&sb, "  %s│%s  工具: %s%s%s\n", Yellow, Reset, Cyan, toolName, Reset)
	if desc != "" {
		// Truncate desc for display if too long
		d := desc
		if len(d) > 60 {
			d = textutil.ClipRunes(d, 60)
		}
		fmt.Fprintf(&sb, "  %s│%s  说明: %s\n", Yellow, Reset, d)
	}
	fmt.Fprintf(&sb, "  %s╰───────────────────────────────────╯%s\n", Yellow, Reset)
	return sb.String()
}

// Thinking indicator (spinner)
type Spinner struct {
	mu      sync.Mutex
	active  bool
	stopCh  chan struct{}
	doneCh  chan struct{}
	message string
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func NewSpinner(message string) *Spinner {
	return &Spinner{message: message}
}

func (s *Spinner) Start() {
	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return
	}
	s.active = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.mu.Unlock()

	go func() {
		defer close(doneCh)
		i := 0
		for {
			select {
			case <-stopCh:
				PrintTransientStatus("")
				return
			default:
				frame := spinnerFrames[i%len(spinnerFrames)]
				PrintTransientStatus(fmt.Sprintf("  %s%s %s%s", Cyan, frame, s.message, Reset))
				i++
				time.Sleep(80 * time.Millisecond)
			}
		}
	}()
}

func (s *Spinner) Stop() {
	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return
	}
	s.active = false
	stopCh := s.stopCh
	doneCh := s.doneCh
	s.mu.Unlock()

	close(stopCh)
	<-doneCh
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
	doneCh  chan struct{}
	message string
}

// Compact walking frames in pure ASCII to avoid terminal mojibake.
var walkFrames = []string{
	"> .   .   .",
	">  .   .   ",
	">   .   .  ",
	">    .   . ",
	"> .   .   .",
	">  .   .   ",
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
	w.doneCh = make(chan struct{})
	// stopCh is recreated on every Start. It is closed by Stop, and a closed
	// channel stays closed — so reusing the one from the constructor meant the
	// goroutine of a second Start saw an immediately-ready stopCh and exited
	// at once, leaving the indicator permanently dead after the first Stop.
	// (Callers currently build a fresh instance per turn, which is why nobody
	// hit it, but Start/Stop/Start is the API this type advertises.)
	w.stopCh = make(chan struct{})
	stopCh := w.stopCh
	doneCh := w.doneCh
	w.mu.Unlock()

	go func() {
		defer close(doneCh)
		i := 0
		for {
			select {
			case <-stopCh:
				PrintTransientStatus("")
				return
			default:
				frame := walkFrames[i%len(walkFrames)]
				w.mu.Lock()
				msg := w.message
				w.mu.Unlock()
				PrintTransientStatus(fmt.Sprintf("  %s%s%s %s%s%s",
					Cyan, frame, Reset, Dim, msg, Reset))
				i++
				time.Sleep(120 * time.Millisecond)
			}
		}
	}()
}

func (w *WalkingIndicator) Stop() {
	w.mu.Lock()
	if !w.active {
		w.mu.Unlock()
		return
	}
	w.active = false
	close(w.stopCh)
	doneCh := w.doneCh
	w.mu.Unlock()
	// Wait for goroutine to finish clearing the line
	<-doneCh
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
	sb.WriteString(Styled(BrightCyan+Bold, "     ______   ____  _    __  ______") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "    / ____/  / __ \\ | |  / / / ____/") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "   / /      / / / / | | / / / __/   ") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "  / /___  / /_/ /  | |/ / / /___   ") + "\n")
	sb.WriteString(Styled(BrightCyan+Bold, "  \\____/  \\____/   |___/ /_____/   ") + "\n")
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "    %scove v%s%s  •  高效、安全的本地 AI 协同编程终端\n", Bold, version, Reset)
	sb.WriteString("\n")
	// Info line
	fmt.Fprintf(&sb, "  %s模型:%s %s%s%s", Dim, Reset, Bold, model, Reset)
	fmt.Fprintf(&sb, "  %s│%s  %s供应商:%s %s", Dim, Reset, Dim, Reset, provider)
	fmt.Fprintf(&sb, "  %s│%s  %s模式:%s %s\n", Dim, Reset, Dim, Reset, mode)

	if isGit {
		fmt.Fprintf(&sb, "  %sGit:%s %s%s%s\n",
			Dim, Reset, Green, gitBranch, Reset)
	}
	fmt.Fprintf(&sb, "  %s目录:%s %s\n", Dim, Reset, cwd)
	fmt.Fprintf(&sb, "  %s工具:%s %d 个\n", Dim, Reset, toolCount)
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "  %s提示: 输入 %s/%s%s 查看命令, %sCtrl+C%s 中断%s\n",
		Dim, Reset, "help", Dim, Reset, Dim, Reset)
	sb.WriteString("\n")

	return sb.String()
}

// Prompt returns the styled input prompt
func Prompt() string {
	return BrightCyan + "❯ " + Reset
}

// PromptRunning returns the prompt shown when a background task is running.
func PromptRunning() string {
	return Yellow + "⚡ ❯ " + Reset
}

// SectionHeader renders a section label
func SectionHeader(label string) string {
	return fmt.Sprintf("\n  %s%s─── %s ───%s\n", Dim, "", label, Reset)
}
