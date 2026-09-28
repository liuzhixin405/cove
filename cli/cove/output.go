package main

import (
	"fmt"
	"io"

	"github.com/liuzhixin405/cove/internal/termui"
)

// User-facing stdout output for the CLI.
//
// Every fmt.Print/Printf/Println in this package goes through one of these so
// there is a single place a front end can redirect. That matters because a
// front end with a pinned input box keeps the prompt in place by tracking how
// many rows it drew: a direct fmt.Println lands outside that bookkeeping and
// leaves the prompt drifting or ghosted.
//
// The destination follows termui.Writer(), so ONE termui.SetWriter call
// redirects both this package and the ~140 termui.PrintSafe call sites
// elsewhere. It resolves late (termui's default is "whatever os.Stdout is at
// write time"), so behaviour is unchanged for the headless and classic paths,
// including tests that capture output by reassigning os.Stdout.
//
// Diagnostics and fatal errors deliberately do NOT come through here: they go
// to os.Stderr directly. On the headless path there is no program to corrupt
// and stderr is the correct channel — it keeps stdout clean for piping — and a
// startup failure happens before any front end exists.
func outw() io.Writer { return termui.Writer() }

// outf, outln and outp are fmt.Printf, Println and Print through
// termui.Text: above the input line while the REPL's editor is on screen,
// verbatim otherwise. They used to write to termui.Writer() directly, past
// the editor, so a command's output typed while a task ran could land on
// the pinned input row or glue itself to the model's unfinished line.
func outf(format string, args ...any) { termui.Text(fmt.Sprintf(format, args...)) }

func outln(args ...any) { termui.Text(fmt.Sprintln(args...)) }

func outp(args ...any) { termui.Text(fmt.Sprint(args...)) }
