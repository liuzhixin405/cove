package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/shell"
	"github.com/liuzhixin405/cove/internal/textutil"
)

const (
	// shellDefaultTimeout applies when the model gives no timeout.
	shellDefaultTimeout = 120 * time.Second
	// shellMaxTimeout caps the timeout a model can ask for: a longer run is
	// almost always a server or watcher that never exits.
	shellMaxTimeout = 10 * time.Minute
	// shellCaptureMax bounds the bytes kept per stream while a command runs.
	// Only ~30KB reach the model, but a build that prints hundreds of MB
	// used to be held in memory in full.
	shellCaptureMax = 2 << 20
	// shellStdoutMax and shellStderrMax are what the result keeps of each.
	shellStdoutMax = 30000
	shellStderrMax = 10000
)

var (
	errCommandTimedOut  = errors.New("command timed out")
	errCommandCancelled = errors.New("command cancelled")
)

// commandTimeout reads the "timeout" input (milliseconds), defaulting to
// shellDefaultTimeout and capped at shellMaxTimeout.
func commandTimeout(input Input) time.Duration {
	var ms float64
	switch v := input["timeout"].(type) {
	case float64:
		ms = v
	case int:
		ms = float64(v)
	case int64:
		ms = float64(v)
	}
	if ms <= 0 {
		return shellDefaultTimeout
	}
	// Compare before converting: a huge float overflows time.Duration.
	if ms >= float64(shellMaxTimeout/time.Millisecond) {
		return shellMaxTimeout
	}
	return time.Duration(ms * float64(time.Millisecond))
}

// shellToolLimits is the part of the bash/powershell descriptions that tells
// the model how long a command may run and what does not carry over.
const shellToolLimits = "Default timeout 120s, maximum 10 min (timeout is in milliseconds); a command still running then is killed and its output so far is returned. " +
	"Every call starts a fresh shell in the project directory: cwd and environment changes (cd, export, $env:) do not carry over to the next call, so chain dependent steps in one command."

// runShell runs cmdStr in sh the way the bash and powershell tools do: in the
// session cwd, with the non-interactive environment, streaming output to
// tctx.OnProgress, bounded by the input's timeout.
func runShell(ctx context.Context, sh shell.Shell, cmdStr string, input Input, tctx Context) Result {
	timeout := commandTimeout(input)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd := tctx.Cwd
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	cmd := exec.CommandContext(execCtx, sh.Path, sh.Args(cmdStr)...)
	cmd.Dir = filepath.Clean(cwd)
	cmd.Env = shell.Env(os.Environ())

	var stdout, stderr boundedBuffer
	exitCode, runErr := streamCommand(execCtx, cmd, &stdout, &stderr, tctx.OnProgress, progressDecoder(sh.Kind, runtime.GOOS))
	return formatShellResult(input, sh.Kind, &stdout, &stderr, exitCode, runErr, timeout)
}

// formatShellResult turns a finished (or killed) command into the tool
// result. Output captured before a timeout or cancel is kept: the last lines
// before a hang are usually what explains it.
func formatShellResult(input Input, kind shell.Kind, stdout, stderr *boundedBuffer, exitCode int, runErr error, timeout time.Duration) Result {
	decode := func(p []byte) string { return decodeShellOutput(p, kind, runtime.GOOS) }

	if runErr != nil && !errors.Is(runErr, errCommandTimedOut) && !errors.Is(runErr, errCommandCancelled) {
		// The command could not be started at all.
		return Result{Data: fmt.Sprintf("Error: %v\nStderr: %s", runErr, stderr.clip(shellStderrMax, decode)), IsError: true}
	}

	var parts []string
	if d, ok := input["description"].(string); ok && d != "" {
		parts = append(parts, "Command: "+d)
	}
	// Long output keeps both ends: test failures, build summaries and the last
	// error lines are at the bottom, which head-only truncation dropped.
	if stdout.written() > 0 {
		parts = append(parts, stdout.clip(shellStdoutMax, decode))
	}
	if stderr.written() > 0 {
		parts = append(parts, "[stderr]\n"+stderr.clip(shellStderrMax, decode))
	}

	res := Result{}
	switch {
	case errors.Is(runErr, errCommandTimedOut):
		parts = append(parts, fmt.Sprintf("[timed out after %ss]", strconv.FormatFloat(timeout.Seconds(), 'f', -1, 64)))
		res.IsError = true
	case errors.Is(runErr, errCommandCancelled):
		parts = append(parts, "[cancelled]")
		res.IsError = true
	case exitCode != 0:
		parts = append(parts, fmt.Sprintf("[exit code: %d]", exitCode))
	}
	res.Data = strings.Join(parts, "\n")
	return res
}

// decodeShellOutput converts captured output to UTF-8. On Windows, native
// programs run from Git Bash or cmd write in the console code page — GBK on a
// Chinese system — so a line that is not UTF-8 but decodes as GBK is decoded.
// PowerShell is switched to UTF-8 output (see shell.Args) and left alone.
// Lines are handled one by one: git and Go tools write UTF-8 into the same
// stream, and decoding those as GBK would garble them.
func decodeShellOutput(p []byte, kind shell.Kind, goos string) string {
	if goos != "windows" || kind == shell.PowerShell || utf8.Valid(p) {
		return string(p)
	}
	var sb strings.Builder
	sb.Grow(len(p))
	for len(p) > 0 {
		line := p
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			line = p[:i+1]
		}
		p = p[len(line):]
		if utf8.Valid(line) {
			sb.Write(line)
		} else if lt, ok := decodeGBK(line); ok {
			sb.WriteString(lt.text)
		} else {
			sb.Write(line)
		}
	}
	return sb.String()
}

// boundedBuffer captures a stream up to shellCaptureMax bytes: the first half
// is kept as written, the rest in a ring holding the latest bytes, so a
// runaway command neither exhausts memory nor loses its last lines.
type boundedBuffer struct {
	mu     sync.Mutex
	head   []byte
	ring   []byte // allocated once head is full
	pos    int    // next write index in ring
	filled bool   // ring has wrapped at least once
	total  int64  // every byte ever written
}

const (
	boundedHeadCap = shellCaptureMax / 2
	boundedTailCap = shellCaptureMax - boundedHeadCap
)

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	if room := boundedHeadCap - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if b.ring == nil {
		b.ring = make([]byte, boundedTailCap)
	}
	if len(p) >= boundedTailCap {
		copy(b.ring, p[len(p)-boundedTailCap:])
		b.pos, b.filled = 0, true
		return n, nil
	}
	k := copy(b.ring[b.pos:], p)
	if k < len(p) {
		b.pos = copy(b.ring, p[k:])
		b.filled = true
	} else if b.pos += k; b.pos == boundedTailCap {
		b.pos, b.filled = 0, true
	}
	return n, nil
}

// tail returns the ring's bytes in write order.
func (b *boundedBuffer) tail() []byte {
	if !b.filled {
		return b.ring[:b.pos]
	}
	out := make([]byte, 0, boundedTailCap)
	return append(append(out, b.ring[b.pos:]...), b.ring[:b.pos]...)
}

// written is how many bytes were ever written to the buffer.
func (b *boundedBuffer) written() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// stored is how many bytes the buffer holds.
func (b *boundedBuffer) stored() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.filled {
		return len(b.head) + boundedTailCap
	}
	return len(b.head) + b.pos
}

// clip decodes the captured bytes and limits them to about max bytes, keeping
// 40% from the start and 60% from the end like textutil.ClipMiddleBytes; the
// omitted count includes what the buffer itself dropped.
func (b *boundedBuffer) clip(maxBytes int, decode func([]byte) string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	tail := b.tail()
	dropped := b.total - int64(len(b.head)+len(tail))
	if dropped <= 0 {
		// Nothing was lost: decode as one piece, so a character split between
		// head and ring is not broken.
		return textutil.ClipMiddleBytes(decode(append(append([]byte(nil), b.head...), tail...)), maxBytes)
	}
	// The head ends and the ring starts at arbitrary bytes, possibly inside a
	// character. Both are cut at a line boundary: decodeShellOutput decodes
	// line by line, and in GBK — unlike UTF-8 — a trail byte cannot be told
	// from a lead byte, so skipping "continuation bytes" finds nothing and the
	// first tail line decoded as garbage. Without a newline in the first
	// clipNewlineSearch bytes (a very long line) the UTF-8 rule is the
	// fallback, rather than dropping most of that line.
	head := b.head
	if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
		dropped += int64(len(head) - (i + 1))
		head = head[:i+1]
	}
	if i := bytes.IndexByte(tail[:min(len(tail), clipNewlineSearch)], '\n'); i >= 0 && i+1 < len(tail) {
		dropped += int64(i + 1)
		tail = tail[i+1:]
	} else {
		for i := 0; i < 3 && len(tail) > 0 && !utf8.RuneStart(tail[0]); i++ {
			tail = tail[1:]
			dropped++
		}
	}
	headStr := decode(head)
	tailStr := decode(tail)
	h := textutil.ClipBytes(headStr, maxBytes*2/5, "")
	start := len(tailStr) - (maxBytes - len(h))
	if start < 0 {
		start = 0
	}
	for start < len(tailStr) && !utf8.RuneStart(tailStr[start]) {
		start++
	}
	omitted := int64(len(headStr)-len(h)) + dropped + int64(start)
	return h + fmt.Sprintf("\n... [%d bytes omitted] ...\n", omitted) + tailStr[start:]
}

// progressDecoder is how live output is decoded for OnProgress: nil (raw
// chunks, forwarded at once) unless decodeShellOutput would change it, i.e.
// on Windows for shells other than PowerShell.
func progressDecoder(kind shell.Kind, goos string) func([]byte) string {
	if goos != "windows" || kind == shell.PowerShell {
		return nil
	}
	return func(p []byte) string { return decodeShellOutput(p, kind, goos) }
}

// progressHoldMax is how much of an unterminated line progressWriter holds
// back waiting for its end before forwarding it anyway.
const progressHoldMax = 4096

// clipNewlineSearch bounds how far into the kept tail clip looks for the line
// boundary it cuts at.
const clipNewlineSearch = 512

// progressWriter forwards everything written to it into a buffer and (if set)
// to an onProgress callback, so callers get both the captured output and a
// live stream of chunks. os/exec invokes Write from a dedicated copy goroutine.
//
// With a decode function, chunks are forwarded decoded and whole lines at a
// time (up to the last "\n" or "\r"): decodeShellOutput works per line, and a
// chunk boundary can fall inside a GBK character. flush forwards the rest.
type progressWriter struct {
	buf        io.Writer
	onProgress func(chunk string)
	decode     func([]byte) string
	pending    []byte
}

func (w *progressWriter) Write(p []byte) (int, error) {
	_, _ = w.buf.Write(p)
	if w.onProgress == nil {
		return len(p), nil
	}
	if w.decode == nil {
		w.onProgress(string(p))
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	if i := bytes.LastIndexAny(w.pending, "\n\r"); i >= 0 {
		w.onProgress(w.decode(w.pending[:i+1]))
		w.pending = append(w.pending[:0], w.pending[i+1:]...)
	} else if len(w.pending) > progressHoldMax {
		// Forced out mid-line: cut between characters, keeping an incomplete
		// one for the next write.
		k := charBoundary(w.pending)
		w.onProgress(w.decode(w.pending[:k]))
		w.pending = append(w.pending[:0], w.pending[k:]...)
	}
	return len(p), nil
}

// charBoundary returns the length of p's longest prefix that does not end
// inside a character: an incomplete trailing UTF-8 sequence when p is UTF-8,
// else a dangling GBK lead byte (a byte >= 0x81 whose pair is missing,
// found by walking the one- and two-byte units from the start).
func charBoundary(p []byte) int {
	for back := 1; back <= utf8.UTFMax && back <= len(p); back++ {
		i := len(p) - back
		if !utf8.RuneStart(p[i]) {
			continue
		}
		if utf8.Valid(p[:i]) && !utf8.FullRune(p[i:]) {
			return i
		}
		break
	}
	if utf8.Valid(p) {
		return len(p)
	}
	i := 0
	for i < len(p) {
		if p[i] < 0x81 {
			i++
		} else {
			i += 2
		}
	}
	if i > len(p) {
		return len(p) - 1
	}
	return len(p)
}

// flush forwards what Write held back; streamCommand calls it once the
// command's output is complete.
func (w *progressWriter) flush() {
	if len(w.pending) == 0 || w.onProgress == nil {
		return
	}
	w.onProgress(w.decode(w.pending))
	w.pending = w.pending[:0]
}

// streamCommand runs cmd, streaming its stdout/stderr live into the provided
// writers (and through onProgress) while remaining cancellable.
//
// It returns the process exit code and any non-exit error. A non-zero exit
// status is reported via exitCode with runErr==nil (mirroring the previous
// cmd.Run() handling). A timeout is reported as errCommandTimedOut and a
// cancellation as errCommandCancelled; what was written before stays in the
// writers.
//
// Robustness against hangs: a command may spawn children (servers, build
// daemons, etc.) that inherit the output pipes. The default ctx-cancel only
// kills the top process, leaving those children alive and the pipes open, which
// would block reads forever. We defend against this two ways:
//   - Cancel kills the whole process tree (taskkill /T on Windows, the process
//     group on Unix) so inherited pipes close promptly.
//   - WaitDelay is a backstop: if I/O is still pending shortly after the process
//     exits, os/exec force-closes the pipes so Run() returns instead of hanging.
func streamCommand(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Writer, onProgress func(chunk string), decode func([]byte) string) (exitCode int, runErr error) {
	outW := &progressWriter{buf: stdout, onProgress: onProgress, decode: decode}
	errW := &progressWriter{buf: stderr, onProgress: onProgress, decode: decode}
	cmd.Stdout, cmd.Stderr = outW, errW

	// Put the child in its own process group (Unix) so the whole tree can be
	// signalled at once. No-op on Windows, where taskkill /T handles the tree.
	setupProcessGroup(cmd)

	// Override the default ctx-cancel (which only kills the lead process) with a
	// process-tree kill so children releasing the pipes can't keep us hanging.
	if ctx != nil && ctx.Done() != nil {
		cmd.Cancel = func() error { return killProcessTree(cmd) }
	}
	// Backstop: even if the tree kill misses something, force-close the pipes a
	// few seconds after the process exits so Run() can't block indefinitely.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	// Run has returned, so the copy goroutines are done writing.
	outW.flush()
	errW.flush()

	if ctx != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return 0, errCommandTimedOut
			}
			return 0, errCommandCancelled
		}
	}

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, err
	}
	return 0, nil
}
