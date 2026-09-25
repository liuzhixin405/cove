package tool

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/shell"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// (a) A command that runs past its timeout still returns what it printed,
// followed by a fixed marker, and is flagged as an error.
func TestBashTimeoutKeepsOutput(t *testing.T) {
	t.Parallel() // waits for a process tree kill
	if shell.Default().Kind != shell.Bash {
		t.Skip("needs a bash shell")
	}
	start := time.Now()
	res, err := NewBashTool().Call(context.Background(),
		Input{"command": "echo started; sleep 5", "timeout": float64(300)},
		Context{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("timed-out command not flagged as an error: %q", res.Data)
	}
	if !strings.Contains(res.Data, "[timed out after 0.3s]") {
		t.Errorf("result lacks the timeout marker: %q", res.Data)
	}
	if !strings.Contains(res.Data, "started") {
		t.Errorf("output printed before the timeout was lost: %q", res.Data)
	}
	// The tree kill (taskkill /T on Windows) can be slow on a cold start, so
	// this is informational rather than a hard bound.
	if el := time.Since(start); el > 4*time.Second {
		t.Logf("timeout took %v to take effect", el)
	}
}

func TestPowerShellTimeoutKeepsOutput(t *testing.T) {
	t.Parallel() // waits out a 3s timeout
	if runtime.GOOS != "windows" {
		t.Skip("powershell tool is Windows-only")
	}
	if _, err := exec.LookPath(findPowerShell()); err != nil {
		t.Skip("PowerShell not installed")
	}
	res, err := NewPowerShellTool().Call(context.Background(),
		Input{"command": "Write-Output started; Start-Sleep 5", "timeout": float64(3000)},
		Context{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// 3s leaves room for PowerShell 5.1 startup (~0.7s), so the output
	// printed before the timeout can be checked.
	if !res.IsError {
		t.Errorf("timed-out command not flagged as an error: %q", res.Data)
	}
	if !strings.Contains(res.Data, "[timed out after 3s]") {
		t.Errorf("result lacks the timeout marker: %q", res.Data)
	}
	if !strings.Contains(res.Data, "started") {
		t.Errorf("output printed before the timeout was lost: %q", res.Data)
	}
}

// The formatting half of (a), independent of how fast a shell starts.
func TestShellResultOnTimeoutKeepsCapturedOutput(t *testing.T) {
	var out, errb boundedBuffer
	out.Write([]byte("partial build log\n"))
	errb.Write([]byte("warning: slow\n"))
	res := formatShellResult(Input{}, shell.Bash, &out, &errb, 0, errCommandTimedOut, 300*time.Millisecond)
	if !res.IsError {
		t.Error("timeout not flagged as an error")
	}
	for _, want := range []string{"partial build log", "warning: slow", "[timed out after 0.3s]"} {
		if !strings.Contains(res.Data, want) {
			t.Errorf("result lacks %q: %q", want, res.Data)
		}
	}
}

// (b) The timeout is capped at ten minutes; the default stays 120s.
func TestShellTimeoutClampedToTenMinutes(t *testing.T) {
	cases := []struct {
		in   Input
		want time.Duration
	}{
		{Input{}, 120 * time.Second},
		{Input{"timeout": float64(300)}, 300 * time.Millisecond},
		{Input{"timeout": float64(10 * 60 * 1000)}, 10 * time.Minute},
		{Input{"timeout": float64(60 * 60 * 1000)}, 10 * time.Minute},
		{Input{"timeout": 60 * 60 * 1000}, 10 * time.Minute},
		{Input{"timeout": float64(-5)}, 120 * time.Second},
	}
	for _, c := range cases {
		if got := commandTimeout(c.in); got != c.want {
			t.Errorf("commandTimeout(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// (d) On Windows, bash (and cmd) output from native programs is often in the
// console code page; GBK bytes are decoded to UTF-8. PowerShell is switched
// to UTF-8 output already, and nothing is touched elsewhere.
func TestDecodeShellOutputGBK(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("编译失败：找不到文件\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeShellOutput(gbk, shell.Bash, "windows"); got != "编译失败：找不到文件\n" {
		t.Errorf("bash on windows: got %q", got)
	}
	if got := decodeShellOutput(gbk, shell.Cmd, "windows"); got != "编译失败：找不到文件\n" {
		t.Errorf("cmd on windows: got %q", got)
	}
	if got := decodeShellOutput(gbk, shell.PowerShell, "windows"); got != string(gbk) {
		t.Errorf("powershell output was re-decoded: %q", got)
	}
	if got := decodeShellOutput(gbk, shell.Bash, "linux"); got != string(gbk) {
		t.Errorf("linux output was re-decoded: %q", got)
	}
	utf := []byte("已经是 UTF-8\n")
	if got := decodeShellOutput(utf, shell.Bash, "windows"); got != string(utf) {
		t.Errorf("valid UTF-8 changed: %q", got)
	}
}

// (e) Captured output is bounded at 2MB and keeps both ends.
func TestBoundedBufferKeepsHeadAndTail(t *testing.T) {
	var b boundedBuffer
	chunk := bytes.Repeat([]byte("x"), 64*1024)
	b.Write([]byte("HEAD-MARKER\n"))
	for i := 0; i < 64; i++ { // 4MB
		b.Write(chunk)
	}
	b.Write([]byte("\nTAIL-MARKER"))
	if b.stored() > shellCaptureMax {
		t.Fatalf("buffer holds %d bytes, cap is %d", b.stored(), shellCaptureMax)
	}
	if shellCaptureMax != 2<<20 {
		t.Errorf("capture cap = %d, want 2MB", shellCaptureMax)
	}
	if b.total != int64(len("HEAD-MARKER\n")+64*len(chunk)+len("\nTAIL-MARKER")) {
		t.Errorf("total = %d, not every written byte was counted", b.total)
	}
	out := b.clip(30000, func(p []byte) string { return string(p) })
	if !strings.HasPrefix(out, "HEAD-MARKER") || !strings.HasSuffix(out, "TAIL-MARKER") {
		t.Errorf("clipped output lost an end: %q ... %q", out[:20], out[len(out)-20:])
	}
	if !strings.Contains(out, "bytes omitted") {
		t.Error("no marker for the dropped middle")
	}
	if len(out) > 30000+100 {
		t.Errorf("clipped output is %d bytes", len(out))
	}
}

// Step 5: the model is told the limits and which shell it is writing for.
func TestShellToolDescriptionsNameLimitsAndShell(t *testing.T) {
	desc := NewBashTool().Def().Description
	for _, want := range []string{"120s", "10 min", shell.Default().Describe(), "cwd"} {
		if !strings.Contains(desc, want) {
			t.Errorf("bash description lacks %q: %s", want, desc)
		}
	}
	ps := NewPowerShellTool().Def().Description
	for _, want := range []string{"120s", "10 min"} {
		if !strings.Contains(ps, want) {
			t.Errorf("powershell description lacks %q: %s", want, ps)
		}
	}
}
