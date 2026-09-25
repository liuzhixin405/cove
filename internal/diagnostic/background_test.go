package diagnostic

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/dream"
	"github.com/liuzhixin405/cove/internal/memory"
)

func findResult(r *Report, name string) (CheckResult, bool) {
	for _, res := range r.Results {
		if res.Name == name {
			return res, true
		}
	}
	return CheckResult{}, false
}

// The full report has a "后台学习" item: dream gates, memory count and the
// last extraction, taken from the injectable BackgroundStatusFn.
func TestRunAllReportsBackgroundLearning(t *testing.T) {
	// A nil config skips the network probe.
	c, _ := newTestChecker(t, nil)
	at := time.Date(2026, 9, 25, 9, 30, 0, 0, time.Local)
	c.background = func() BackgroundStatus {
		return BackgroundStatus{
			Dream:  dream.Status{Enabled: true, HoursSinceLast: -1, SessionsSinceLast: 1, MinHours: 12, MinSessions: 3},
			Memory: memory.Stats{FileCount: 4, TotalBytes: 2048, LastExtractedAt: at, LastExtractedCount: 2},
		}
	}
	report := c.RunAll(t.Context())
	res, ok := findResult(report, "background")
	if !ok {
		t.Fatal("no background item in the full report")
	}
	if res.Title != "后台学习" || problem(res) {
		t.Fatalf("background item = %+v", res)
	}
	text := report.Format()
	for _, want := range []string{"后台学习", "从未整理", "还差 2 个会话", "4 条", "09-25 09:30", "保存 2 条"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
}

// A failed consolidation is a warning, not a silent debug line.
func TestBackgroundLearningWarnsOnFailedDream(t *testing.T) {
	c, _ := newTestChecker(t, config.DefaultConfig())
	c.background = func() BackgroundStatus {
		return BackgroundStatus{Dream: dream.Status{Enabled: true, MinHours: 12, MinSessions: 3,
			LastRunAt: time.Now(), LastRunErr: errors.New("429 rate limited")}}
	}
	res := c.checkBackgroundLearning(t.Context())
	if res.Status != SevWarning || res.Error == nil || !strings.Contains(res.Error.Detail, "429") {
		t.Fatalf("result = %+v", res)
	}
}

// Without an injected function the item still works from disk (cove doctor
// runs in a process with no engine).
func TestBackgroundLearningDefaultReadsDisk(t *testing.T) {
	c, root := newTestChecker(t, config.DefaultConfig())
	memDir := filepath.Join(root, ".cove", "memory")
	if err := memory.NewStoreForDirs(memDir).Save("a.md", "alpha"); err != nil {
		t.Fatal(err)
	}
	memory.RecordExtractionIn(memDir, 1)
	res := c.checkBackgroundLearning(t.Context())
	if res.Skipped || problem(res) {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Detail, "保存 1 条") {
		t.Fatalf("detail = %q", res.Detail)
	}
}

// policies.json that does not parse means its deny rules silently stop
// applying; the report names the file and the error.
func TestPolicyFileItemReportsLoadError(t *testing.T) {
	c, _ := newTestChecker(t, config.DefaultConfig())
	c.policyErr = func() error { return errors.New("load C:/x/policies.json: invalid character") }
	res := c.checkPolicyFile(t.Context())
	if res.Title != "权限规则文件" || res.Status < SevError || res.Error == nil ||
		!strings.Contains(res.Error.Detail, "policies.json") {
		t.Fatalf("result = %+v", res)
	}

	c.policyErr = func() error { return nil }
	if res := c.checkPolicyFile(t.Context()); problem(res) {
		t.Fatalf("nil load error reported as a problem: %+v", res)
	}
}

// With no engine to ask, the check parses the file in the config dir itself.
func TestPolicyFileItemDefaultParsesFile(t *testing.T) {
	c, _ := newTestChecker(t, config.DefaultConfig())
	if res := c.checkPolicyFile(t.Context()); problem(res) {
		t.Fatalf("missing policies.json reported as a problem: %+v", res)
	}
	if err := os.MkdirAll(c.configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.configDir, "policies.json"), []byte("[{"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := c.checkPolicyFile(t.Context())
	if !problem(res) || !strings.Contains(res.Error.Detail, "policies.json") {
		t.Fatalf("broken policies.json not reported: %+v", res)
	}
}

// The package-level hooks are what the engine sets; NewChecker picks them up.
func TestNewCheckerUsesInjectedFunctions(t *testing.T) {
	called := 0
	PolicyLoadErrorFn = func() error { called++; return nil }
	BackgroundStatusFn = func() BackgroundStatus { called++; return BackgroundStatus{} }
	t.Cleanup(func() { PolicyLoadErrorFn, BackgroundStatusFn = nil, nil })
	c, _ := newTestChecker(t, config.DefaultConfig())
	c.checkPolicyFile(t.Context())
	c.checkBackgroundLearning(t.Context())
	if called != 2 {
		t.Fatalf("injected functions called %d times, want 2", called)
	}
}
