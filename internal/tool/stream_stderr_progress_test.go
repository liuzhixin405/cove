package tool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// TestStreamHelperProcess is not a test: it is the child process of
// TestStreamCommandSeparatesStderrProgress, writing one line to each stream.
func TestStreamHelperProcess(t *testing.T) {
	if os.Getenv("COVE_STREAM_HELPER") != "1" {
		t.Skip("helper process")
	}
	fmt.Fprint(os.Stdout, "to-stdout\n")
	fmt.Fprint(os.Stderr, "to-stderr\n")
	os.Exit(0)
}

// Both pipes used to feed one progress callback, so a front end that keeps
// per-stream state (a sanitiser holding back a partial escape or UTF-8
// sequence) mixed the streams: bytes held from stdout were completed by the
// start of a stderr chunk. With an stderr callback given, stderr goes there.
func TestStreamCommandSeparatesStderrProgress(t *testing.T) {
	var mu sync.Mutex
	var out, errOut strings.Builder
	cmd := exec.Command(os.Args[0], "-test.run=^TestStreamHelperProcess$")
	cmd.Env = append(os.Environ(), "COVE_STREAM_HELPER=1")
	var so, se boundedBuffer
	_, err := streamCommand(context.Background(), cmd, &so, &se,
		func(c string) { mu.Lock(); out.WriteString(c); mu.Unlock() },
		func(c string) { mu.Lock(); errOut.WriteString(c); mu.Unlock() }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "to-stdout") || strings.Contains(out.String(), "to-stderr") {
		t.Fatalf("stdout progress = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "to-stderr") || strings.Contains(errOut.String(), "to-stdout") {
		t.Fatalf("stderr progress = %q", errOut.String())
	}
}

// Without an stderr callback both streams still reach OnProgress, as before.
func TestStderrProgressFallsBackToOnProgress(t *testing.T) {
	var got []string
	tctx := Context{OnProgress: func(c string) { got = append(got, c) }}
	onOut, onErr := progressHooks(tctx)
	onOut("a")
	onErr("b")
	if strings.Join(got, "") != "ab" {
		t.Fatalf("got %q", got)
	}
	var errs []string
	tctx.OnStderrProgress = func(c string) { errs = append(errs, c) }
	onOut, onErr = progressHooks(tctx)
	onErr("c")
	if len(errs) != 1 || strings.Join(got, "") != "ab" {
		t.Fatalf("stderr hook not used: %q %q", got, errs)
	}
	_ = onOut
}
