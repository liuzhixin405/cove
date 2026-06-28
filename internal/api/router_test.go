package api

import (
	"context"
	"testing"
)

func TestModelRouter_OverrideWins(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	mr.SetOverride("user-pick")
	d := mr.Route(context.Background(), "anything")
	if d.Model != "user-pick" || d.Source != "override" {
		t.Fatalf("override should win: got %+v", d)
	}
	mr.ClearOverride()
	if mr.Route(context.Background(), "hi").Source == "override" {
		t.Fatal("override not cleared")
	}
}

func TestModelRouter_ComplexityClassifier(t *testing.T) {
	mr := NewModelRouter("premium", "fast")
	if d := mr.Route(context.Background(), "请重构整个模块"); d.Model != "premium" {
		t.Fatalf("complex task should route to premium, got %+v", d)
	}
	if d := mr.Route(context.Background(), "hi"); d.Model != "fast" {
		t.Fatalf("simple task should route to fast, got %+v", d)
	}
}

// Regression: after /model or /provider changes the active model, the router
// must track it instead of routing to the construction-time model.
func TestModelRouter_SetModelsSync(t *testing.T) {
	mr := NewModelRouter("old-premium", "old-fast")
	mr.SetModels("new-premium", "new-fast")

	if d := mr.Route(context.Background(), "重构架构"); d.Model != "new-premium" {
		t.Fatalf("complex task should use updated premium model, got %+v", d)
	}
	if d := mr.Route(context.Background(), "hi"); d.Model != "new-fast" {
		t.Fatalf("simple task should use updated fast model, got %+v", d)
	}

	// Empty args leave existing models untouched.
	mr.SetModels("", "")
	if mr.defaultModel != "new-premium" || mr.fastModel != "new-fast" {
		t.Fatalf("empty SetModels should be a no-op, got %q/%q", mr.defaultModel, mr.fastModel)
	}
}
