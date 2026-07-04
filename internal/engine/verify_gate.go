package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// VerifyResult captures the outcome of running one configured verification
// command against the current workspace.
type VerifyResult struct {
	Command  string
	Passed   bool
	ExitCode int
	Output   string
	Duration time.Duration
}

// VerifyGate runs a small, user-configured list of shell commands (e.g.
// "go build ./...", "go test ./...") before the engine accepts a model's
// "I'm done" (a response with no further tool calls) as actually done.
//
// This is the minimal version of the "完成合同 + 验证门禁" item from
// docs/核心优化项清单.md's EDCL proposal: no evidence-ledger UI, no scoring —
// just "run the commands, and if any fails, hand the failure back to the
// model instead of ending the turn." Every check is still appended to a
// JSONL file on disk so results are at least inspectable later, which is
// enough to satisfy the underlying goal (completion claims are backed by
// something checkable) without building the full ledger UI up front.
//
// It is opt-in and off by default (zero commands configured = no-op), and
// bounded: RunIfDue caps how many times it will re-reject a single user
// turn (maxRetries) so a flaky/always-failing command can't turn into an
// unbounded retry loop and blow through the cost budget it's supposed to
// protect.
type VerifyGate struct {
	commands   []string
	workDir    string
	maxRetries int
	timeout    time.Duration
	ledgerPath string
}

// NewVerifyGate creates a gate for the given commands. An empty commands
// slice makes every method a no-op, which is the default: nothing changes
// for users who don't set done_verify_commands in config.json.
func NewVerifyGate(commands []string, workDir string) *VerifyGate {
	var ledger string
	if home, err := os.UserHomeDir(); err == nil {
		ledger = filepath.Join(home, ".cove", "verify_ledger.jsonl")
	}
	return &VerifyGate{
		commands:   commands,
		workDir:    workDir,
		maxRetries: 2,
		timeout:    120 * time.Second,
		ledgerPath: ledger,
	}
}

// Enabled reports whether any verification commands are configured.
func (g *VerifyGate) Enabled() bool { return g != nil && len(g.commands) > 0 }

// MaxRetries returns how many times the gate will reject a single turn's
// completion before giving up and letting it through anyway (to bound cost).
func (g *VerifyGate) MaxRetries() int {
	if g == nil {
		return 0
	}
	return g.maxRetries
}

// Run executes every configured command in order and stops at the first
// failure (fail-fast: no point running the test suite if the build itself
// is broken). It never returns an error itself — a command that can't even
// start is recorded as a failed result, not a Go-level error, since from the
// gate's point of view that's just as much "not verified" as a nonzero exit.
func (g *VerifyGate) Run(ctx context.Context) (results []VerifyResult, allPassed bool) {
	if !g.Enabled() {
		return nil, true
	}
	allPassed = true
	for _, cmdStr := range g.commands {
		start := time.Now()
		runCtx, cancel := context.WithTimeout(ctx, g.timeout)
		out, exitCode, runErr := runVerifyCommand(runCtx, cmdStr, g.workDir)
		cancel()
		if runErr != nil {
			out = out + "\n[verify_gate] failed to run command: " + runErr.Error()
			exitCode = -1
		}
		passed := exitCode == 0
		res := VerifyResult{
			Command: cmdStr, Passed: passed, ExitCode: exitCode,
			Output: out, Duration: time.Since(start),
		}
		results = append(results, res)
		g.appendLedger(res)
		if !passed {
			allPassed = false
			break // fail-fast; remaining commands are skipped, not just unreported
		}
	}
	return results, allPassed
}

// Summary renders the check results as guidance to hand back to the model:
// which command(s) ran, which one failed, its exit code, and a truncated
// tail of its output (the part most likely to contain the actual error).
func Summary(results []VerifyResult) string {
	var sb strings.Builder
	sb.WriteString("[verify_gate] Your completion was not accepted because a verification command failed. Results:\n")
	for _, r := range results {
		if r.Passed {
			sb.WriteString(fmt.Sprintf("  OK   %s (%s)\n", r.Command, r.Duration.Round(time.Millisecond)))
			continue
		}
		sb.WriteString(fmt.Sprintf("  FAIL %s (exit %d, %s)\n", r.Command, r.ExitCode, r.Duration.Round(time.Millisecond)))
		sb.WriteString("---\n")
		sb.WriteString(truncateTail(r.Output, 2000))
		sb.WriteString("\n---\n")
	}
	sb.WriteString("Fix the issue above before declaring the task complete again. Do not repeat the same fix if it already failed once — diagnose the actual error output first.")
	return sb.String()
}

func (g *VerifyGate) appendLedger(r VerifyResult) {
	if g.ledgerPath == "" {
		return
	}
	if dir := filepath.Dir(g.ledgerPath); dir != "" {
		_ = os.MkdirAll(dir, 0755)
	}
	f, err := os.OpenFile(g.ledgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	entry := map[string]any{
		"time":        time.Now().Format(time.RFC3339),
		"command":     r.Command,
		"passed":      r.Passed,
		"exit_code":   r.ExitCode,
		"duration_ms": r.Duration.Milliseconds(),
		"output_tail": truncateTail(r.Output, 500),
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
}

// runVerifyCommand executes a single shell command, mirroring the same
// cross-platform shell detection used by the bash tool (kept as a small,
// self-contained duplicate here rather than importing internal/tool, to
// avoid coupling the engine's completion gate to the tool package's
// unexported helpers).
func runVerifyCommand(ctx context.Context, cmdStr string, workDir string) (output string, exitCode int, err error) {
	shell, flag := verifyShell()
	cmd := exec.CommandContext(ctx, shell, flag, cmdStr)
	if workDir != "" {
		cmd.Dir = workDir
	}
	cmd.Env = os.Environ()

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	out := buf.String()
	if len(out) > 20000 {
		out = out[len(out)-20000:] // keep the tail: errors are usually at the end
	}
	if runErr == nil {
		return out, 0, nil
	}
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		return out, exitErr.ExitCode(), nil
	}
	// Could not even start the command (bad shell, timeout before start, etc).
	return out, -1, runErr
}

func verifyShell() (string, string) {
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("cmd"); err == nil {
			return "cmd", "/C"
		}
		if _, err := exec.LookPath("pwsh"); err == nil {
			return "pwsh", "-Command"
		}
		if _, err := exec.LookPath("powershell"); err == nil {
			return "powershell", "-Command"
		}
		if _, err := exec.LookPath("bash"); err == nil {
			return "bash", "-c"
		}
	}
	if _, err := exec.LookPath("bash"); err == nil {
		return "bash", "-c"
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh", "-Command"
	}
	if _, err := exec.LookPath("sh"); err == nil {
		return "sh", "-c"
	}
	return "cmd", "/C"
}

func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "... (truncated)\n" + s[len(s)-max:]
}
