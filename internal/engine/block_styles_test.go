package engine

import (
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/render"
)

// The glyph set used to be chosen during package initialization, which runs
// before cli/cove's init switches the Windows console to UTF-8 (code page
// 65001). On a Chinese Windows console (code page 936 at that moment) every
// user got the ASCII fallback for the whole session. It has to be decided at
// first use, after startup has configured the console.
//
// Both directions are checked, because the value computed at package init
// depends on the console this test binary happens to run in.
func TestBlockStylesAreChosenAtFirstUse(t *testing.T) {
	for _, ascii := range []bool{true, false} {
		blockStylesOnce = sync.Once{}
		env := "0"
		if ascii {
			env = "1"
		}
		t.Setenv("COVE_TUI_ASCII", env) // what the console reports after startup
		if got := currentBlockStyles().Glyphs == render.ASCIIGlyphs(); got != ascii {
			t.Errorf("COVE_TUI_ASCII=%s at first use: ASCII glyphs = %v, want %v", env, got, ascii)
		}
	}
	blockStylesOnce = sync.Once{}
}
