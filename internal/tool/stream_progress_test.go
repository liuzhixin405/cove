package tool

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	"github.com/liuzhixin405/cove/internal/shell"
)

func gbkBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func gbkDecode(p []byte) string { return decodeShellOutput(p, shell.Bash, "windows") }

// Live progress used to forward raw chunks, so on a Chinese Windows the TUI
// showed GBK bytes as mojibake while the final result was decoded. Chunks are
// now decoded per line; a character split across two writes is held back
// until the line is complete.
func TestProgressWriterDecodesGBKPerLine(t *testing.T) {
	line := gbkBytes(t, "编译失败：找不到文件\n")
	var got []string
	var buf bytes.Buffer
	w := &progressWriter{buf: &buf, onProgress: func(c string) { got = append(got, c) }, decode: gbkDecode}

	_, _ = w.Write(line[:3]) // splits the second character
	if len(got) != 0 {
		t.Fatalf("a partial line was forwarded: %q", got)
	}
	_, _ = w.Write(line[3:])
	_, _ = w.Write(gbkBytes(t, "下载 50%\r"))
	_, _ = w.Write(gbkBytes(t, "尾部"))
	w.flush()

	want := []string{"编译失败：找不到文件\n", "下载 50%\r", "尾部"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("progress chunks = %q, want %q", got, want)
	}
	if !bytes.HasPrefix(buf.Bytes(), line) {
		t.Fatal("the captured buffer is not the raw bytes")
	}
}

// Without a decoder (not Windows, or PowerShell) chunks pass through at once.
func TestProgressWriterPassesThroughWithoutDecoder(t *testing.T) {
	var got []string
	w := &progressWriter{buf: &bytes.Buffer{}, onProgress: func(c string) { got = append(got, c) }}
	_, _ = w.Write([]byte("partial"))
	if len(got) != 1 || got[0] != "partial" {
		t.Fatalf("got %q", got)
	}
	w.flush()
	if len(got) != 1 {
		t.Fatalf("flush re-sent data: %q", got)
	}
}

// A long line without a newline is not held back forever.
func TestProgressWriterFlushesLongLine(t *testing.T) {
	var got []string
	w := &progressWriter{buf: &bytes.Buffer{}, onProgress: func(c string) { got = append(got, c) }, decode: gbkDecode}
	_, _ = w.Write(bytes.Repeat([]byte("x"), progressHoldMax+10))
	if len(got) != 1 {
		t.Fatalf("a %d-byte line was held back", progressHoldMax+10)
	}
}

// When the capture buffer dropped its middle, the kept tail starts at an
// arbitrary byte. For GBK that byte may be the second half of a character,
// and "skip UTF-8 continuation bytes" does not find the next character start,
// so the first tail line decoded as garbage. The tail now starts at a line
// boundary.
func TestBoundedBufferClipStartsTailAtLineBoundary(t *testing.T) {
	var b boundedBuffer
	b.Write(gbkBytes(t, "开头\n"))
	filler := gbkBytes(t, "中文输出行\n")
	for b.total < int64(shellCaptureMax)*2 {
		b.Write(filler)
	}
	b.Write(gbkBytes(t, "最后一行"))
	// A limit above what the buffer holds shows the tail from its first byte,
	// which is where the cut into a character would show.
	out := b.clip(4*shellCaptureMax, gbkDecode)
	if !strings.HasPrefix(out, "开头\n") || !strings.HasSuffix(out, "最后一行") {
		t.Fatalf("clipped output lost an end: %q ... %q", out[:20], out[len(out)-40:])
	}
	_, tail, ok := strings.Cut(out, "bytes omitted] ...\n")
	if !ok {
		t.Fatal("no omission marker")
	}
	for _, l := range strings.Split(tail, "\n") {
		if l != "中文输出行" && l != "最后一行" {
			t.Fatalf("tail has a garbled line %q", l)
		}
	}
}

// Fix round 1 (item 9): an unterminated GBK line forced out at progressHoldMax
// is cut between characters, never inside one.
func TestProgressWriterForcedFlushKeepsGBKCharacters(t *testing.T) {
	var got []string
	w := &progressWriter{buf: &bytes.Buffer{}, onProgress: func(c string) { got = append(got, c) }, decode: gbkDecode}
	long := append([]byte("x"), bytes.Repeat(gbkBytes(t, "中"), progressHoldMax/2+5)...) // odd offset
	_, _ = w.Write(long[:len(long)-1])                                                  // ends on the lead byte of the last character
	_, _ = w.Write(long[len(long)-1:])
	w.flush()
	joined := strings.Join(got, "")
	if joined != "x"+strings.Repeat("中", progressHoldMax/2+5) {
		t.Fatalf("forced flush split a character: %d chunks, first ends %q", len(got), got[0][len(got[0])-6:])
	}
	if len(got) < 2 {
		t.Fatalf("the long line was not forwarded before flush: %d chunks", len(got))
	}
}

// Fix round 1 (item 9): the line-boundary cut only looks at the first 512
// bytes of the tail; a tail without a newline there keeps the UTF-8 rule
// instead of dropping up to a whole long line.
func TestBoundedBufferClipNewlineSearchIsBounded(t *testing.T) {
	var b boundedBuffer
	b.Write([]byte("head\n"))
	b.Write(bytes.Repeat([]byte("y"), shellCaptureMax*2))
	b.Write([]byte("\nend"))
	out := b.clip(4*shellCaptureMax, func(p []byte) string { return string(p) })
	_, tail, _ := strings.Cut(out, "bytes omitted] ...\n")
	if !strings.HasPrefix(tail, "yyyy") {
		t.Fatalf("the tail was cut at a newline %d KB in: starts %q", (shellCaptureMax/2)>>10, tail[:10])
	}
}
