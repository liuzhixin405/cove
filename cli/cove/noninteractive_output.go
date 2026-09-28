package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/render"
)

// wireNonInteractiveOutput gives a -p or headless run the engine's own
// lines — compaction, blocked tools, stall warnings, model switches — on
// stderr, so stdout carries the answer alone as the manual promises. Without
// this the lines were dropped (no sink).
//
// termui's writer is left alone on purpose: the answer itself is printed
// through it (outln → termui.Writer()), and redirecting it to stderr sent
// the answer there too — the -p end-to-end scenario caught exactly that.
func wireNonInteractiveOutput(eng *engine.Engine) {
	if eng == nil {
		return
	}
	eng.SetOutput(engine.LineSink(func(line string) {
		line = render.StripControls(strings.TrimRight(line, "\r\n"))
		if strings.TrimSpace(line) != "" {
			fmt.Fprintln(os.Stderr, line)
		}
	}))
}
