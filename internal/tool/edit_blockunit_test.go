package tool

import "testing"

// Final fix (Minor 3): blockUnit compared only the first line's indentation,
// so a block starting at column 0 matched any unit ("" is level 0 in every
// unit). It now compares the first line that is indented, on both sides.
func TestBlockUnitSkipsZeroIndentFirstLine(t *testing.T) {
	file := []string{"", "    "}
	// The file hint says 2 spaces, so the block's second line is level 2;
	// oldString has it at level 1 (2 spaces in a 2-space model) — the hint
	// does not map the block onto oldString's depth, so the block keeps its
	// own unit.
	if got := blockUnit(file, []string{"", "  "}, 2, 2); got != 4 {
		t.Fatalf("blockUnit = %d, want the block's own 4", got)
	}
	// When the levels agree the file-wide hint is used.
	if got := blockUnit(file, []string{"", "    "}, 2, 2); got != 2 {
		t.Fatalf("blockUnit = %d, want the hint 2", got)
	}
}
