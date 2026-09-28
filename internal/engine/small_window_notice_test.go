package engine

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
)

// The notice said a 16384 window "装不下" a 6536 overhead. It now names the
// overhead, the reply's reserve and what is left, which add up to the window.
func TestSmallWindowNoticeAddsUp(t *testing.T) {
	t.Cleanup(api.ClearModelContextWindows)
	api.SetModelContextWindow("tiny-local", 8192)
	got := smallWindowNotice("tiny-local", 6536)
	for _, want := range []string{"8192", "6536", "4000", "留给对话的只剩约 0 token"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "装不下") {
		t.Errorf("notice still claims the window cannot hold the overhead: %s", got)
	}
}
