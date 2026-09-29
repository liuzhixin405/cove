package diagnostic

import (
	"fmt"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

type persistingRuntime struct {
	*stubRuntime
	saved map[string]int
}

func (p *persistingRuntime) PersistModelContextWindow(model string, tokens int) error {
	p.saved[model] = tokens
	return nil
}

// A runtime that can persist the learned window is asked to, and the user
// is told it was written rather than told to edit config.json by hand: the
// same 16K wall was hit four times in one afternoon with the hint on screen.
func TestContextWindowRemedyPersistsWhenTheRuntimeCan(t *testing.T) {
	resetRuntimeEvents(t)
	ResetRemedyState()
	t.Cleanup(api.ClearModelContextWindows)
	rt := &persistingRuntime{stubRuntime: newStubRuntime(), saved: map[string]int{}}
	SetRuntime(rt)

	err := fmt.Errorf("api: %w", &api.StatusError{Status: 400, Msg: llamaOverflow})
	ReportError(err, Context{Provider: "openai-compatible", Model: "qwen3.6-27b"})

	if rt.saved["qwen3.6-27b"] != 16384 {
		t.Fatalf("window not persisted: %v", rt.saved)
	}
	if len(rt.notes) != 1 || !strings.Contains(rt.notes[0], "已写入") || strings.Contains(rt.notes[0], "在 config.json 加") {
		t.Errorf("note should say the window was written, got %v", rt.notes)
	}
}
