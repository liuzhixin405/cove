package engine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
)

// Important 4: a command that runs out of time is reported as TimedOut, not
// passed, and Summary says it is not counted as a failure.
func TestVerifyGateTimeoutIsNotAFailure(t *testing.T) {
	g := NewVerifyGate([]string{"go build ./..."}, "")
	g.ledgerPath = ""
	g.SetTimeout(20 * time.Millisecond)
	g.runner = func(ctx context.Context, _, _ string) (string, int, error) {
		<-ctx.Done()
		return "", -1, ctx.Err()
	}
	results, passed := g.Run(context.Background(), nil)
	if passed || len(results) != 1 || !results[0].TimedOut || !TimedOut(results) {
		t.Fatalf("passed=%v results=%+v, want one timed-out result", passed, results)
	}
	if s := Summary(results); !strings.Contains(s, "did not finish within") || !strings.Contains(s, "not counted as a failure") {
		t.Fatalf("summary = %q", s)
	}
	// A real failure is still a failure, not a timeout.
	g.runner = func(context.Context, string, string) (string, int, error) { return "boom", 1, nil }
	results, _ = g.Run(context.Background(), nil)
	if TimedOut(results) || results[0].TimedOut {
		t.Fatalf("exit 1 reported as a timeout: %+v", results)
	}
}

// Important 4: dotnet and npm get 300 s by default, everything else 120 s;
// done_verify_timeout_seconds overrides both.
func TestVerifyGateTimeoutDefaults(t *testing.T) {
	g := NewVerifyGate([]string{"x"}, "")
	for cmd, want := range map[string]time.Duration{
		"dotnet build --nologo -v q": 300 * time.Second,
		"npm run build --if-present": 300 * time.Second,
		"go build ./...":             120 * time.Second,
		"cargo check":                120 * time.Second,
	} {
		if got := g.timeoutFor(cmd); got != want {
			t.Errorf("timeoutFor(%q) = %v, want %v", cmd, got, want)
		}
	}
	g.SetTimeout(45 * time.Second)
	if got := g.timeoutFor("dotnet build"); got != 45*time.Second {
		t.Fatalf("configured timeout ignored: %v", got)
	}
	eng := newPatternEngine(t, &seqProvider{}, func(c *Config) {
		c.DoneVerifyCommands = []string{"dotnet build"}
		c.DoneVerifyTimeout = 7 * time.Second
	})
	if got := eng.verifyGate.timeoutFor("dotnet build"); got != 7*time.Second {
		t.Fatalf("Config.DoneVerifyTimeout not applied: %v", got)
	}
}

// Important 4: a timed-out check neither rejects the completion nor
// escalates; the turn ends with the model's answer after one call.
func TestVerifyGateTimeoutEndsTurnNormally(t *testing.T) {
	const answer = "Everything is in place and the change is done as requested."
	prov := &seqProvider{reply: func(context.Context, int, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: answer}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.DoneVerifyCommands = []string{"go build ./..."}
		c.DoneCheck = "off"
	})
	eng.verifyGate.ledgerPath = ""
	eng.verifyGate.SetTimeout(20 * time.Millisecond)
	eng.verifyGate.runner = func(ctx context.Context, _, _ string) (string, int, error) {
		<-ctx.Done()
		return "", -1, ctx.Err()
	}
	var lines []string
	eng.OnEngineOutput = func(s string) { lines = append(lines, s) }
	got, err := run(t, eng, "do it")
	if err != nil {
		t.Fatal(err)
	}
	if got != answer || len(prov.requests()) != 1 || eng.verifyAttempts != 0 {
		t.Fatalf("got %q after %d calls, attempts %d: want the answer, no retry", got, len(prov.requests()), eng.verifyAttempts)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "not counted as a failure") {
		t.Fatalf("no timeout notice in %q", lines)
	}
}

// Important 5: a TypeScript project is checked by tsc only, not by tsc and
// then npm run build again; without tsc the build script is the check.
func TestDetectVerifyCommandsTypeScriptSkipsNpmBuild(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"tsconfig.json": "{}", "package.json": `{"scripts":{"build":"tsc && vite build"}}`})
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, filepath.Join(dir, "node_modules", ".bin"), map[string]string{"tsc": ""})
	if got, want := detectVerifyCommands(dir), []string{"npx --no-install tsc --noEmit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with tsc: %v, want %v", got, want)
	}
	if err := os.RemoveAll(filepath.Join(dir, "node_modules")); err != nil {
		t.Fatal(err)
	}
	if got, want := detectVerifyCommands(dir), []string{"npm run build --if-present"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("without tsc: %v, want %v", got, want)
	}
}
