package diagnostic

import (
	"context"
	"testing"

	"github.com/agentgo/internal/config"
)

func TestNewError(t *testing.T) {
	e := New(ErrConfigMissing, "/home/user/.agentgo/config.yaml")
	if e == nil {
		t.Fatal("expected non-nil error")
	}
	if e.Def.Code != ErrConfigMissing {
		t.Errorf("expected code %s, got %s", ErrConfigMissing, e.Def.Code)
	}
	if e.Def.Severity != SevFatal {
		t.Errorf("expected severity Fatal, got %v", e.Def.Severity)
	}
	if e.Detail == "" {
		t.Error("expected non-empty detail")
	}
}

func TestNewFixed(t *testing.T) {
	e := NewFixed(ErrSessionCorrupt, "2 files")
	if !e.Fixed {
		t.Error("expected Fixed=true")
	}
	formatted := e.Format()
	if formatted == "" {
		t.Error("expected non-empty formatted output")
	}
}

func TestLookupUnknown(t *testing.T) {
	def := Lookup("E9999")
	if def != nil {
		t.Error("expected nil for unknown code")
	}
}

func TestNewUnknownCode(t *testing.T) {
	e := New("E9999", "something went wrong")
	if e == nil {
		t.Fatal("expected non-nil error even for unknown code")
	}
	if e.Def.Message != "未知错误" {
		t.Errorf("expected generic message, got %s", e.Def.Message)
	}
}

func TestCheckerRunQuick(t *testing.T) {
	cfg := &config.Config{
		Model: "test-model",
		Provider: config.ProviderConfig{
			Name:   "deepseek",
			APIKey: "sk-test-key",
		},
	}
	checker := NewChecker(cfg)
	report := checker.RunQuick()

	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if len(report.Results) == 0 {
		t.Error("expected at least one check result")
	}
	// Summary should not be empty
	if report.Summary() == "" {
		t.Error("expected non-empty summary")
	}
}

func TestCheckerRunAll(t *testing.T) {
	cfg := &config.Config{
		Model: "test-model",
		Provider: config.ProviderConfig{
			Name:    "deepseek",
			APIKey:  "sk-test-key",
			BaseURL: "https://api.deepseek.com/v1",
		},
	}
	checker := NewChecker(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 10*1000*1000*1000) // 10s
	defer cancel()

	report := checker.RunAll(ctx)
	if report == nil {
		t.Fatal("expected non-nil report")
	}

	// Format should produce output without panicking
	formatted := report.Format()
	if formatted == "" {
		t.Error("expected non-empty formatted report")
	}
}

func TestQuickCheckNilConfig(t *testing.T) {
	// Should not panic with nil config
	issues := QuickCheck(nil)
	// With nil config, should report config invalid
	if len(issues) == 0 {
		t.Error("expected issues with nil config")
	}
}

func TestReportHasProblems(t *testing.T) {
	r := &Report{
		Results: []CheckResult{
			{Name: "test1", Status: SevInfo},
			{Name: "test2", Status: SevInfo},
		},
	}
	if r.HasProblems() {
		t.Error("expected no problems")
	}

	r.Results = append(r.Results, CheckResult{
		Name:   "test3",
		Status: SevError,
		Error:  New(ErrAPIAuth, "invalid key"),
	})
	if !r.HasProblems() {
		t.Error("expected problems")
	}
}
