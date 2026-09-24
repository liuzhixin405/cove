package dream

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove/internal/textutil"
)

const (
	grepMaxMatches   = 200     // cap total result rows
	grepMaxFileBytes = 2 << 20 // skip files larger than 2MB
	grepMaxLineWidth = 400     // clip very long matched lines
)

// grepFiles searches for pattern under root (a file or directory), recursively,
// WITHOUT invoking any shell. This replaces the former shell-based implementation
// (`sh -c "grep ..."`), which passed AI-generated input to a shell and was thus
// vulnerable to command injection via $(...), backticks, ';', '&', etc.
//
// pattern is compiled as a regular expression; if it is not a valid regexp it
// falls back to a literal substring search. Either way it is only ever used as
// data for matching — never executed.
func grepFiles(pattern, root string) string {
	if pattern == "" {
		return "Error: pattern is required"
	}

	var matches func(string) bool
	if re, err := regexp.Compile(pattern); err == nil {
		matches = re.MatchString
	} else {
		matches = func(line string) bool { return strings.Contains(line, pattern) }
	}

	info, err := os.Stat(root)
	if err != nil {
		return fmt.Sprintf("Error: %v", err)
	}

	var out []string
	truncated := false

	// searchFile appends "path:line:text" rows for each matching line. It returns
	// false once the global match cap is reached, signalling the walk to stop.
	searchFile := func(path string, size int64) bool {
		if size > grepMaxFileBytes {
			return true
		}
		f, err := os.Open(path)
		if err != nil {
			return true
		}
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		lineNo := 0
		for sc.Scan() {
			lineNo++
			line := sc.Text()
			if !matches(line) {
				continue
			}
			// Clip on a rune boundary: a byte slice cut Chinese text mid-rune
			// and put invalid UTF-8 into the next request body.
			line = textutil.ClipBytes(line, grepMaxLineWidth, "…")
			out = append(out, fmt.Sprintf("%s:%d:%s", path, lineNo, line))
			if len(out) >= grepMaxMatches {
				truncated = true
				return false
			}
		}
		return true
	}

	if info.IsDir() {
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			if !searchFile(path, fi.Size()) {
				return filepath.SkipAll
			}
			return nil
		})
	} else {
		searchFile(root, info.Size())
	}

	if len(out) == 0 {
		return "No matches found"
	}
	result := strings.Join(out, "\n")
	if truncated {
		result += fmt.Sprintf("\n... [truncated at %d matches]", grepMaxMatches)
	}
	return result
}
