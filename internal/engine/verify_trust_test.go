package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/config"
	"github.com/liuzhixin405/cove-agent/internal/permission"
)

// trustGateEngine is an engine in default mode whose detected verify gate
// records the commands it runs instead of executing them, and whose output
// lines are collected.
func trustGateEngine(t *testing.T, dir string) (eng *Engine, ran func() []string, lines func() []string) {
	t.Helper()
	t.Setenv("COVE_CONFIG_DIR", t.TempDir()) // a fresh, empty trust store
	write := &mockTool{name: "write", result: "written"}
	prov := &seqProvider{reply: func(ctx context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		// Every odd request is the model finishing after its one write.
		if n%2 == 0 {
			return toolCallResp("w", "write", map[string]any{"filePath": "a.go"}), nil
		}
		return &api.ChatResponse{Content: "done"}, nil
	}}
	eng = newPatternEngine(t, prov, nil, write)
	var mu sync.Mutex
	var cmds, out []string
	g := newAutoVerifyGate([]string{"npm run build --if-present"}, dir)
	g.ledgerPath = ""
	g.runner = func(ctx context.Context, cmd, workDir string) (string, int, error) {
		mu.Lock()
		cmds = append(cmds, cmd)
		mu.Unlock()
		return "", 0, nil
	}
	eng.verifyGate = g
	eng.SetOutput(LineSink(func(s string) { mu.Lock(); out = append(out, s); mu.Unlock() }))
	// Default mode, but the write itself is allowed so the turn reaches the gate.
	eng.perm.SetMode(permission.Default)
	eng.PermissionPrompt = func(string, map[string]any, string) bool { return true }
	return eng,
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), cmds...) },
		func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), out...) }
}

func countNotice(lines []string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "未信任的项目不自动运行构建/测试校验") {
			n++
		}
	}
	return n
}

// A cloned repository without any .cove.json used to have its detected
// build (npm run build = a package.json script) executed at the end of the
// first turn that edited a file, in default mode, with no prompt. In an
// untrusted directory the gate is skipped, said once, and not a failure.
func TestDetectedVerifyGateSkippedInUntrustedProject(t *testing.T) {
	dir := t.TempDir()
	eng, ran, lines := trustGateEngine(t, dir)
	for i := 0; i < 2; i++ {
		got, err := run(t, eng, "edit a.go")
		if err != nil || got != "done" {
			t.Fatalf("turn %d: %q, %v", i, got, err)
		}
	}
	if c := ran(); len(c) != 0 {
		t.Fatalf("untrusted project ran %v", c)
	}
	if n := countNotice(lines()); n != 1 {
		t.Fatalf("notice shown %d times, want 1: %q", n, lines())
	}
	if eng.verifyAttempts != 0 {
		t.Fatalf("a skipped gate counted as a failure (%d attempts)", eng.verifyAttempts)
	}

	if err := config.TrustProjectDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, eng, "edit a.go"); err != nil {
		t.Fatal(err)
	}
	if c := ran(); len(c) != 1 || c[0] != "npm run build --if-present" {
		t.Fatalf("trusted project ran %v, want the detected build", c)
	}
}

// In auto and bypass mode build and test commands already run unasked, so the
// gate runs without the project being trusted.
func TestDetectedVerifyGateRunsInAutoModeWithoutTrust(t *testing.T) {
	for _, mode := range []permission.Mode{permission.Auto, permission.Bypass} {
		eng, ran, lines := trustGateEngine(t, t.TempDir())
		eng.perm.SetMode(mode)
		if _, err := run(t, eng, "edit a.go"); err != nil {
			t.Fatal(err)
		}
		if len(ran()) != 1 || countNotice(lines()) != 0 {
			t.Fatalf("%s: ran %v, notices %d", mode, ran(), countNotice(lines()))
		}
	}
}

// Commands the user configured (done_verify_commands) keep running in an
// untrusted directory: the user wrote them.
func TestConfiguredVerifyCommandsNeedNoTrust(t *testing.T) {
	eng, ran, _ := trustGateEngine(t, t.TempDir())
	eng.verifyGate.needsTrust = false // what NewVerifyGate builds for done_verify_commands
	if _, err := run(t, eng, "edit a.go"); err != nil {
		t.Fatal(err)
	}
	if len(ran()) != 1 {
		t.Fatalf("configured commands did not run: %v", ran())
	}
	if g := NewVerifyGate([]string{"make check"}, t.TempDir()); g.needsTrust {
		t.Fatal("a configured gate requires trust")
	}
}
