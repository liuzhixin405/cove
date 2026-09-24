package termui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// promptBody is the part of the prompt built from the caller's text: every
// row after the tool line and before the bottom rule.
func promptBody(t *testing.T, prompt string) []string {
	t.Helper()
	rows := strings.Split(prompt, "\n")
	var body []string
	for _, r := range rows {
		plain := ansi.Strip(r)
		if strings.Contains(plain, "说明") || (len(body) > 0 && !strings.Contains(plain, "╰")) {
			body = append(body, r)
		}
		if strings.Contains(plain, "╰") {
			break
		}
	}
	if len(body) == 0 {
		t.Fatalf("no 说明 rows in %q", prompt)
	}
	return body
}

// The box used to clip the command to 60 runes, so a command long enough to
// push its dangerous tail past column 60 was approved unseen:
// "echo <padding> ; rm -rf ~" showed only the echo.
func TestPermissionPromptShowsTheWholeCommand(t *testing.T) {
	cmd := "echo " + strings.Repeat("x", 120) + " ; rm -rf ~"
	got := ansi.Strip(PermissionPrompt("bash", cmd))
	if !strings.Contains(got, "rm -rf ~") {
		t.Fatalf("the end of the command is not shown:\n%s", got)
	}
}

// A \r or a cursor move inside the command let the text shown for approval
// differ from the command that runs: "rm -rf ~\r\x1b[2Kls" displayed as "ls".
func TestPermissionPromptNeutralisesControlsInTheCommand(t *testing.T) {
	for _, cmd := range []string{
		"rm -rf ~\r\x1b[2Kls -la",
		"rm -rf ~\x1b[1A\x1b[2K",
		"rm -rf ~\x1b]0;title\x07",
	} {
		body := strings.Join(promptBody(t, PermissionPrompt("bash", cmd)), "\n")
		if strings.Contains(body, "\r") {
			t.Errorf("carriage return reached the prompt for %q: %q", cmd, body)
		}
		// The box's own colour codes are the only escapes allowed.
		if rest := strings.NewReplacer(Yellow, "", Reset, "").Replace(body); strings.ContainsAny(rest, "\x1b\a") {
			t.Errorf("control sequence from the command reached the prompt for %q: %q", cmd, rest)
		}
		if !strings.Contains(body, "rm -rf ~") {
			t.Errorf("the real command is not visible for %q: %q", cmd, body)
		}
	}
	if got := PermissionPrompt("bash\x1b]0;x\x07", "ls"); strings.Contains(got, "\x1b]") {
		t.Errorf("control sequence in the tool name reached the prompt: %q", got)
	}
}

// A multi-line command (a heredoc, "a && \\\n b") used to continue at column 0
// outside the box; every line of it now sits inside the box's gutter.
func TestPermissionPromptKeepsEveryCommandLineInsideTheBox(t *testing.T) {
	body := promptBody(t, PermissionPrompt("bash", "cat <<EOF > x.sh\nrm -rf ~\nEOF"))
	if len(body) != 3 {
		t.Fatalf("want 3 description rows, got %d: %q", len(body), body)
	}
	for _, r := range body {
		if !strings.HasPrefix(ansi.Strip(r), "  │") {
			t.Errorf("row is outside the box: %q", ansi.Strip(r))
		}
	}
}

// Something enormous (a multi-kilobyte heredoc) is still shortened, but from
// the middle and with a marker, so both ends stay visible and the reader is
// told that something was left out.
func TestPermissionPromptShortensHugeCommandsFromTheMiddle(t *testing.T) {
	cmd := "head-marker " + strings.Repeat("y", 20000) + " tail-marker"
	got := ansi.Strip(PermissionPrompt("bash", cmd))
	for _, want := range []string{"head-marker", "tail-marker", "omitted"} {
		if !strings.Contains(got, want) {
			t.Errorf("shortened prompt is missing %q", want)
		}
	}
	if len(got) > 8000 {
		t.Errorf("prompt is %d bytes, it was not shortened", len(got))
	}
}
