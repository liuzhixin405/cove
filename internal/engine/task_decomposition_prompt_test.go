package engine

import (
	"strings"
	"testing"
)

func TestSuggestsComplexTask_ShortSimpleMessage(t *testing.T) {
	if suggestsComplexTask("fix the typo in README") {
		t.Fatal("expected a short, simple message to not be flagged as complex")
	}
}

func TestSuggestsComplexTask_LongMessage(t *testing.T) {
	msg := strings.Repeat("please handle this carefully ", 12) // > 300 chars
	if !suggestsComplexTask(msg) {
		t.Fatal("expected a long message to be flagged as complex")
	}
}

func TestSuggestsComplexTask_Keyword(t *testing.T) {
	if !suggestsComplexTask("please refactor the auth module") {
		t.Fatal("expected a keyword match to be flagged as complex")
	}
	if !suggestsComplexTask("请重构一下权限模块") {
		t.Fatal("expected a Chinese keyword match to be flagged as complex")
	}
}

func TestSuggestsComplexTask_MultipleFiles(t *testing.T) {
	if !suggestsComplexTask("update main.go, handler.go and util.go together") {
		t.Fatal("expected 3+ file mentions to be flagged as complex")
	}
	if suggestsComplexTask("update main.go") {
		t.Fatal("a single short file mention alone should not be flagged as complex")
	}
}

func TestTaskDecompositionGuidance_EmptyForSimpleMessage(t *testing.T) {
	if g := taskDecompositionGuidance("fix the typo in README"); g != "" {
		t.Fatalf("expected no guidance for a simple message, got: %q", g)
	}
}

func TestTaskDecompositionGuidance_NonEmptyForComplexMessage(t *testing.T) {
	g := taskDecompositionGuidance("please refactor the entire auth module")
	if g == "" {
		t.Fatal("expected guidance for a complex-looking message")
	}
	if !strings.Contains(g, "3-5 concrete steps") {
		t.Fatalf("expected guidance to mention step decomposition, got: %q", g)
	}
}
