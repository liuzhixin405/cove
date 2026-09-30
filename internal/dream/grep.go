package dream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/textutil"
)

const (
	grepMaxMatches    = 200 // cap total result rows
	grepMaxLineWidth  = 400 // clip very long matched lines
	grepMaxIncomplete = 5   // files named in the "not fully searched" note

	// grepReadBuf is the read buffer, and grepWindow the most of one line
	// held in memory at a time. A longer line is matched window by window,
	// each window starting with the last grepOverlap bytes of the one
	// before, so a match up to that long that straddles two windows is
	// still found. (A ^ anchor can then match at a window start inside the
	// line: a false positive on a >64KB line is the price of bounded memory.)
	grepReadBuf = 64 << 10
	grepWindow  = 64 << 10
	grepOverlap = 1 << 10
	// grepSniff is how much of a file is checked for a NUL byte: text files
	// have none, binaries (.vhdx, zip, a .git pack, images) nearly always do
	// in their first bytes.
	grepSniff = 8 << 10
	// grepCtxEvery is how many bytes are scanned between context checks.
	grepCtxEvery = 1 << 20
)

// grepMaxTotalBytes bounds the bytes one search reads, over all files; a
// variable so tests can lower it.
var grepMaxTotalBytes int64 = 256 << 20

// grepSkipDirs are not descended into while walking a tree (they are
// searched when they are the path asked for): they are large, mostly
// binary, and never hold memories or transcripts.
var grepSkipDirs = map[string]bool{".git": true, "node_modules": true}

// grepOpen opens a file to search; a variable so tests can make it fail.
var grepOpen = os.Open

// errGrepBudget stops a search that has read grepMaxTotalBytes.
var errGrepBudget = errors.New("search byte budget reached")

// grepFiles searches for pattern under root (a file or directory), recursively,
// WITHOUT invoking any shell. This replaces the former shell-based implementation
// (`sh -c "grep ..."`), which passed AI-generated input to a shell and was thus
// vulnerable to command injection via $(...), backticks, ';', '&', etc.
//
// pattern is compiled as a regular expression; if it is not a valid regexp it
// falls back to a literal substring search. Either way it is only ever used as
// data for matching — never executed.
//
// Files are streamed with bounded memory. Files over 2MB used to be skipped
// outright and a bufio.Scanner stopped at the first line over 1MB with an
// unchecked ErrTooLong, so the rest of the file was silently dropped; the fix
// for that read each line whole with ReadString('\n'), which built a
// newline-less multi-GB file (.vhdx, zip, a .git pack, a minified bundle) in
// memory and crashed the process running dream. Now a line is held at most
// grepWindow bytes at a time, binary files are skipped, .git and
// node_modules are skipped while walking, the total read is bounded by
// grepMaxTotalBytes, and ctx (the run's dreamRunTimeout) is checked while
// reading: the walk used to run on over a huge tree past the timeout while
// the worker held the consolidation lock. The output is capped (match count,
// matched-line width), and every file skipped or not read to the end is
// named in the result.
func grepFiles(ctx context.Context, pattern, root string) string {
	if pattern == "" {
		return "Error: pattern is required"
	}

	var matches func([]byte) bool
	if re, err := regexp.Compile(pattern); err == nil {
		matches = re.Match
	} else {
		lit := []byte(pattern)
		matches = func(b []byte) bool { return bytes.Contains(b, lit) }
	}

	info, err := os.Stat(root)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	var out []string
	var incomplete []string
	truncated := false
	var stopErr error // set once the search must end: budget, cancellation
	var total, sinceCheck int64
	buf := make([]byte, 0, grepWindow+grepReadBuf)

	// account counts n bytes read and reports whether the search may go on.
	account := func(n int) bool {
		total += int64(n)
		sinceCheck += int64(n)
		if sinceCheck >= grepCtxEvery {
			sinceCheck = 0
			if err := ctx.Err(); err != nil {
				stopErr = fmt.Errorf("search cancelled: %w", err)
				return false
			}
		}
		if total >= grepMaxTotalBytes {
			stopErr = fmt.Errorf("%w (%d MB); narrow the path", errGrepBudget, grepMaxTotalBytes>>20)
			return false
		}
		return true
	}

	// searchFile appends "path:line:text" rows for each matching line. It returns
	// false once the search must stop (match cap, budget, cancellation).
	searchFile := func(path string) bool {
		if err := ctx.Err(); err != nil {
			stopErr = fmt.Errorf("search cancelled: %w", err)
			return false
		}
		f, err := grepOpen(path)
		if err != nil {
			incomplete = append(incomplete, fmt.Sprintf("%s (%v)", path, err))
			return true
		}
		defer func() { _ = f.Close() }()

		rd := bufio.NewReaderSize(f, grepReadBuf)
		if head, _ := rd.Peek(grepSniff); bytes.IndexByte(head, 0) >= 0 {
			incomplete = append(incomplete, fmt.Sprintf("%s (binary file, skipped)", path))
			return true
		}

		lineNo := 0 // completed lines
		buf = buf[:0]
		matched := false // the current line already produced its row
		for {
			chunk, rerr := rd.ReadSlice('\n')
			buf = append(buf, chunk...)
			if !account(len(chunk)) {
				incomplete = append(incomplete, fmt.Sprintf("%s (stopped at line %d: %v)", path, lineNo+1, stopErr))
				return false
			}
			eol := rerr == nil
			last := rerr != nil && !errors.Is(rerr, bufio.ErrBufferFull)
			if !eol && !last && len(buf) < grepWindow {
				continue // more of this line fits the window
			}
			window := buf
			if eol {
				window = bytes.TrimRight(window, "\r\n")
			}
			if !matched && len(window) > 0 && matches(window) {
				matched = true
				// Clip on a rune boundary: a byte slice cut Chinese text mid-rune
				// and put invalid UTF-8 into the next request body.
				line := textutil.ClipBytes(string(window[:min(len(window), grepMaxLineWidth*4)]), grepMaxLineWidth, "…")
				out = append(out, fmt.Sprintf("%s:%d:%s", path, lineNo+1, line))
				if len(out) >= grepMaxMatches {
					truncated = true
					return false
				}
			}
			switch {
			case eol || last:
				if len(buf) > 0 {
					lineNo++
				}
				buf, matched = buf[:0], false
			default:
				// A window of an over-long line: keep its tail so a match across
				// the boundary is still seen, and drop the rest.
				n := copy(buf, buf[len(buf)-grepOverlap:])
				buf = buf[:n]
			}
			if last {
				if !errors.Is(rerr, io.EOF) {
					incomplete = append(incomplete, fmt.Sprintf("%s (stopped at line %d: %v)", path, lineNo, rerr))
				}
				return true
			}
		}
	}

	if info.IsDir() {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if path != root {
					incomplete = append(incomplete, fmt.Sprintf("%s (%v)", path, err))
				}
				return nil
			}
			if d.IsDir() {
				if path != root && grepSkipDirs[d.Name()] {
					incomplete = append(incomplete, fmt.Sprintf("%s (directory skipped; search it by passing it as the path)", path))
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if !searchFile(path) {
				return filepath.SkipAll
			}
			return nil
		})
	} else {
		searchFile(root)
	}
	if stopErr != nil && !strings.Contains(strings.Join(incomplete, "\n"), stopErr.Error()) {
		// A stop between files (cancelled before opening the next one) is
		// carried by no file's entry: name it, or the result would read as
		// a complete search.
		incomplete = append(incomplete, stopErr.Error())
	}

	result := strings.Join(out, "\n")
	if len(out) == 0 {
		result = "No matches found"
	}
	if truncated {
		result += fmt.Sprintf("\n... [truncated at %d matches]", grepMaxMatches)
	}
	if len(incomplete) > 0 {
		shown := incomplete
		if len(shown) > grepMaxIncomplete {
			shown = shown[:grepMaxIncomplete]
		}
		result += fmt.Sprintf("\n... [%d file(s) could not be fully searched: %s", len(incomplete), strings.Join(shown, "; "))
		if len(incomplete) > len(shown) {
			result += fmt.Sprintf("; and %d more", len(incomplete)-len(shown))
		}
		result += "]"
	}
	return result
}
