package mcp

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// helperStderrFlood writes one huge stderr line followed by plenty more
// output, then reports on stdout. Like a Node server (an EPIPE on
// process.stderr is an unhandled 'error' event), it dies if a stderr write
// fails.
func helperStderrFlood() {
	write := func(s string) {
		if _, err := os.Stderr.WriteString(s); err != nil {
			os.Exit(3)
		}
	}
	write(strings.Repeat("e", 600*1024) + "\n")
	for i := 0; i < 2000; i++ {
		write("progress line after the long one\n")
	}
	_, _ = os.Stdout.WriteString("stdout-ok\n")
}

// TestStdioTransport_StderrDrainSurvivesHugeLines: the stderr drain used a
// Scanner capped at 256KB per line; at the first longer line it gave up and
// closed the pipe. Every later stderr write of the server then failed with
// EPIPE, which crashes a Node server - killed by its own log line.
func TestStdioTransport_StderrDrainSurvivesHugeLines(t *testing.T) {
	tr := newHelperTransport(t, "stderr-flood")
	defer tr.Close()

	got := make(chan string, 1)
	go func() {
		line, _ := tr.reader.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if strings.TrimSpace(line) != "stdout-ok" {
			t.Fatalf("stdout = %q; want stdout-ok", line)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("server blocked writing stderr: the drain stopped after an over-long line")
	}
}

// TestStdioTransport_SendHonoursContextWhenServerStopsReading: a server that
// is wedged (deadlocked, or busy in synchronous work) stops draining its stdin.
// Once the pipe buffer is full the write blocks, and it used to block forever:
// Send ignored its context and held the transport lock, so the tool call could
// not be interrupted and every other call to that server queued behind it -
// cove froze until the process was killed.
func TestStdioTransport_SendHonoursContextWhenServerStopsReading(t *testing.T) {
	tr := newHelperTransport(t, "ignore-stdin")
	defer tr.Close()

	big := map[string]string{"blob": strings.Repeat("x", 4<<20)} // far beyond any pipe buffer

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tr.Send(ctx, big) }()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Send error = %v; want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked past its context deadline on a server that stopped reading stdin")
	}

	// A second caller must not queue forever behind the wedged write either.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- tr.Send(ctx2, map[string]string{"small": "y"}) }()
	select {
	case err := <-done2:
		if err == nil {
			t.Fatal("Send on a transport with an abandoned partial write returned nil; the stream is corrupt")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a second Send queued forever behind the wedged one")
	}
}
