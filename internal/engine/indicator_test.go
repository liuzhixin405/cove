package engine

import "testing"

func TestShouldShowWalkingIndicator(t *testing.T) {
	eng := &Engine{}

	if eng.shouldShowWalkingIndicator(0) {
		t.Fatal("iter=0 should not show walking indicator")
	}

	if !eng.shouldShowWalkingIndicator(1) {
		t.Fatal("iter>0 without debug/output override should show walking indicator")
	}

	eng.config.Debug = true
	if eng.shouldShowWalkingIndicator(1) {
		t.Fatal("debug mode should suppress walking indicator")
	}

	eng.config.Debug = false
	eng.OnEngineOutput = func(string) {}
	if eng.shouldShowWalkingIndicator(1) {
		t.Fatal("external output handler should suppress walking indicator")
	}
}
