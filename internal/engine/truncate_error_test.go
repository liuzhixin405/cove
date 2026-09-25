package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/token"
)

// A failing tool can print as much as a succeeding one (a compiler dumping
// thousands of errors, a test run that panics in a loop). Error results used
// to skip truncation entirely and could fill the context in one call.
func TestErrorResultIsTruncatedAndKeepsErrorPrefix(t *testing.T) {
	huge := "Error: build failed\n" + strings.Repeat("undefined: foo at bar.go:12\n", 100000/28)
	if len(huge) < 99000 {
		t.Fatalf("fixture too small: %d", len(huge))
	}
	mt := &mockTool{name: "mytool", readOnly: true, err: errors.New(huge)}
	eng := newTestEngine(&mockProvider{}, mt)

	out := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "mytool", Input: map[string]any{}})
	limit := toolOutputLimit("mytool", eng.currentModel())
	if got := token.Estimate(out); got > limit {
		t.Fatalf("error result is %d tokens, limit %d", got, limit)
	}
	if !strings.HasPrefix(out, "Error") {
		t.Fatalf("truncated error lost its Error prefix: %q", out[:40])
	}
}

// An error result without the prefix gets one, and a short one is untouched.
func TestShortErrorResultUnchanged(t *testing.T) {
	mt := &mockTool{name: "mytool", readOnly: true, err: errors.New("permission denied")}
	eng := newTestEngine(&mockProvider{}, mt)
	out := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "mytool", Input: map[string]any{}})
	if out != "Error: permission denied" {
		t.Fatalf("out = %q", out)
	}
}

var nextOffsetRe = regexp.MustCompile(`\[next: offset=(\d+)\]$`)
var readLineRe = regexp.MustCompile(`(?m)^(\d+)[:\t]`)

// The read tool ends a partial read with "[next: offset=N]". When the engine
// cut the result further, the marker was cut with it and the model no longer
// learned where to continue; now it is rebuilt from the last line kept.
func TestTruncatedReadKeepsContinuationMarker(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("File: big.go (5000 lines total)\n\n")
	for i := 1; i <= 2000; i++ {
		fmt.Fprintf(&sb, "%d: %s\n", i, strings.Repeat("x", 60))
	}
	sb.WriteString("... [showing lines 1-2000 of 5000]\n[next: offset=2001]")
	mt := &mockTool{name: "read", readOnly: true, result: sb.String()}
	eng := newTestEngine(&mockProvider{}, mt)

	out := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "read", Input: map[string]any{}})
	if len(out) >= sb.Len() {
		t.Fatalf("fixture was not truncated (%d bytes)", len(out))
	}
	m := nextOffsetRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("truncated read does not end with [next: offset=N]:\n...%s", out[len(out)-200:])
	}
	lines := readLineRe.FindAllStringSubmatch(out, -1)
	last, _ := strconv.Atoi(lines[len(lines)-1][1])
	if want := strconv.Itoa(last + 1); m[1] != want {
		t.Fatalf("next offset = %s, want %s (last kept line %d + 1)", m[1], want, last)
	}
	if strings.Count(out, "[next: offset=") != 1 {
		t.Fatalf("stale marker kept alongside the new one")
	}
}

// A read that fits keeps the tool's own marker as is.
func TestUntruncatedReadMarkerUntouched(t *testing.T) {
	data := "File: a.go (30 lines total)\n\n1: package a\n2: \n... [showing lines 1-2 of 30]\n[next: offset=3]"
	mt := &mockTool{name: "read", readOnly: true, result: data}
	eng := newTestEngine(&mockProvider{}, mt)
	out := eng.executeTool(context.Background(), api.ToolCall{ID: "1", Name: "read", Input: map[string]any{}})
	if out != data {
		t.Fatalf("out = %q", out)
	}
}

func TestTruncateKeepTail(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("Error: head line\n")
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&sb, "noise line %d\n", i)
	}
	sb.WriteString("exit status 1")
	s := sb.String()
	out := token.TruncateKeepTail(s, 500, 2)
	if token.Estimate(out) > 500 {
		t.Fatalf("TruncateKeepTail result %d tokens > 500", token.Estimate(out))
	}
	if !strings.HasPrefix(out, "Error: head line") || !strings.HasSuffix(out, "noise line 4999\nexit status 1") {
		t.Fatalf("head or tail lost:\n%s", out)
	}
	if short := "a\nb"; token.TruncateKeepTail(short, 500, 2) != short {
		t.Fatal("short text changed")
	}
}
