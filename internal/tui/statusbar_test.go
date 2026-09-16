package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestStatusBarWidthAndNoDuplication(t *testing.T) {
	for _, w := range []int{40, 60, 80, 100, 120, 137, 138, 139, 140, 200} {
		m := newSmokeModel(t, w, 24, nil, nil)
		m.status = StatusInfo{
			Version: "5.0.0", Model: "deepseek-v4-pro",
			Provider: "deepseek", PermMode: "default",
		}
		got := m.renderStatusBar()
		plain := ansi.Strip(got)

		if n := strings.Count(plain, "\n"); n != 0 {
			t.Errorf("w=%d: status bar wrapped onto %d extra rows: %q", w, n, plain)
		}
		if gw := ansi.StringWidth(strings.Split(plain, "\n")[0]); gw != w {
			t.Errorf("w=%d: status bar width = %d, want %d: %q", w, gw, w, plain)
		}
		// The state indicator must survive every width: it is the only
		// busy/idle signal. Narrow terminals may clip it, but never drop it.
		if c := strings.Count(plain, "Ready"); c != 1 {
			if w >= 20 && !strings.Contains(plain, "Read") {
				t.Errorf("w=%d: state indicator lost entirely: %q", w, plain)
			} else if c > 1 {
				t.Errorf("w=%d: %q contains \"Ready\" %d times, want 1", w, plain, c)
			}
		}
		if strings.Contains(plain, "Readyy") {
			t.Errorf("w=%d: duplicated character: %q", w, plain)
		}
	}
}
