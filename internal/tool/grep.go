package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// grepMaxLines caps one grep result; the rest is summarized as a note.
const grepMaxLines = 100

// grepMaxContext caps the context parameter: more than a few lines either
// side turns a search into reading whole files, which read does better.
const grepMaxContext = 10

// lookRipgrep finds rg; tests replace it to exercise the fallback.
var lookRipgrep = func() (string, error) { return exec.LookPath("rg") }

type GrepTool struct{ baseTool }

func NewGrepTool() Tool {
	return &GrepTool{baseTool{def: Def{
		Name: "grep", Description: "Fast content search with regex. Uses ripgrep (rg) when available, otherwise a built-in search. Honors .gitignore inside git repositories. Paths are shown relative to the working directory; output is capped at 100 lines.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"Regex pattern to search for"},
				"include":{"type":"string","description":"Glob pattern to filter files (e.g. *.go, *.ts)"},
				"path":{"type":"string","description":"Directory or file to search in (defaults to cwd)"},
				"ignore_case":{"type":"boolean","description":"Case-insensitive match (like grep -i)"},
				"context":{"type":"integer","description":"Lines of context to show before and after each match (0-10, like grep -C)"},
				"files_only":{"type":"boolean","description":"List only the paths of matching files (like grep -l)"}
			},
			"required":["pattern"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Grep",
	}}}
}

// grepOptions are the search parameters shared by both implementations.
type grepOptions struct {
	pattern    string
	include    string
	ignoreCase bool
	filesOnly  bool
	context    int
	// base is the directory paths are shown relative to (the cwd).
	base string
}

// display shows path relative to the working directory when it is inside it.
func (o grepOptions) display(path string) string {
	if o.base == "" {
		return path
	}
	rel, err := filepath.Rel(o.base, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return path
	}
	return rel
}

func (t *GrepTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	pattern, _ := input["pattern"].(string)
	include, _ := input["include"].(string)
	searchPath, _ := input["path"].(string)
	if pattern == "" {
		return Result{Data: "Error: pattern is required", IsError: true}, nil
	}
	if searchPath == "" {
		searchPath = tctx.Cwd
	}
	if searchPath == "" {
		searchPath = "."
	}
	// Same boundary as the read tool: grep is a way to read files too.
	searchPath, err := resolvePathInCwd(searchPath, tctx, false)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	opts := grepOptions{pattern: pattern, include: include, base: tctx.Cwd}
	if opts.base == "" {
		opts.base, _ = os.Getwd()
	}
	if opts.base != "" {
		if abs, err := filepath.Abs(opts.base); err == nil {
			opts.base = abs
		}
	}
	if abs, err := filepath.Abs(searchPath); err == nil {
		searchPath = abs
	}
	opts.ignoreCase = inputBool(input, "ignore_case") || inputBool(input, "-i")
	opts.filesOnly = inputBool(input, "files_only")
	opts.context = min(max(inputInt(input, "context"), 0), grepMaxContext)

	var lines []string
	counted := false // lines holds every result (rg), not just the first grepMaxLines+1
	if rg, err := lookRipgrep(); err == nil {
		lines, err = ripgrep(ctx, rg, opts, searchPath)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
		counted = true
	} else {
		lines, err = builtinGrep(ctx, opts, searchPath)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
	}

	return Result{Data: grepResult(lines, counted, opts)}, nil
}

// grepResult joins the output lines, cutting at grepMaxLines. counted means
// lines is what rg returned — everything, except that rg stops each file at
// --max-count — so the note gives a lower bound of how many were left out,
// and says so when a file did hit that cap. The built-in search stops
// collecting one line past the cap and can only say that there were more.
func grepResult(lines []string, counted bool, opts grepOptions) string {
	if len(lines) == 0 {
		return "No matches found"
	}
	if len(lines) <= grepMaxLines {
		return strings.Join(lines, "\n")
	}
	const hint = " not shown (narrow the pattern, or use include/path)"
	note := "\n... more matches" + hint
	if counted {
		unit := "matches"
		switch {
		case opts.filesOnly:
			unit = "files"
		case opts.context > 0:
			unit = "lines" // context lines and "--" separators are counted too
		}
		capNote := ""
		if !opts.filesOnly && hitPerFileCap(lines) {
			capNote = "; some files hit the per-file cap"
		}
		note = fmt.Sprintf("\n... at least %d more %s not shown (narrow the pattern, or use include/path%s)",
			len(lines)-grepMaxLines, unit, capNote)
	}
	return strings.Join(lines[:grepMaxLines], "\n") + note
}

// grepMatchLine splits an rg match line "path:N:text" (context lines use "-"
// and are not matched) into its path.
var grepMatchLine = regexp.MustCompile(`^(.*?):\d+:`)

// hitPerFileCap reports whether some file reached rg's --max-count
// (grepMaxLines+1 matches), after which rg stops reading that file.
func hitPerFileCap(lines []string) bool {
	perFile := map[string]int{}
	for _, l := range lines {
		if m := grepMatchLine.FindStringSubmatch(l); m != nil {
			perFile[m[1]]++
			if perFile[m[1]] >= grepMaxLines+1 {
				return true
			}
		}
	}
	return false
}

func inputBool(input Input, key string) bool {
	switch v := input[key].(type) {
	case bool:
		return v
	case string:
		b, _ := strconv.ParseBool(v)
		return b
	}
	return false
}

func inputInt(input Input, key string) int {
	switch v := input[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

func ripgrep(ctx context.Context, rg string, opts grepOptions, searchPath string) ([]string, error) {
	args := []string{"-n", "--no-heading", "--color=never", "--max-columns=500", "--max-columns-preview",
		// Per file: enough to fill one result, without rg walking a huge
		// generated file to the end.
		"--max-count=" + strconv.Itoa(grepMaxLines+1)}
	if opts.ignoreCase {
		args = append(args, "-i")
	}
	if opts.filesOnly {
		args = append(args, "-l")
	} else if opts.context > 0 {
		args = append(args, "-C", strconv.Itoa(opts.context))
	}
	if opts.include != "" {
		args = append(args, "-g", opts.include)
	}
	// rg prints paths as it was given them, so it runs in the cwd with a
	// relative path; "." comes back as a "./" prefix, which is removed below.
	target := opts.display(searchPath)
	// -e keeps a pattern that starts with "-" from being read as a flag.
	args = append(args, "-e", opts.pattern, "--", target)
	cmd := exec.CommandContext(ctx, rg, args...)
	if target != searchPath {
		cmd.Dir = opts.base
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		var exitErr *exec.ExitError
		ok := errors.As(err, &exitErr)
		switch {
		case ok && exitErr.ExitCode() == 1:
			return nil, nil
		case ok && out != "":
			// Exit 2 with output: some files could not be read. The matches
			// that were found are still the answer.
		default:
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return nil, fmt.Errorf("rg: %s", msg)
		}
	}
	if out == "" {
		return nil, nil
	}
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if strings.HasPrefix(l, "./") || strings.HasPrefix(l, `.\`) {
			l = l[2:]
		}
		lines[i] = l
	}
	return lines, nil
}

// builtinGrep is the search used when ripgrep is not installed, which is the
// usual case on Windows. It walks the same files rg would (projectFiles),
// skips binaries, and stops once it has more lines than a result shows.
func builtinGrep(ctx context.Context, opts grepOptions, searchPath string) ([]string, error) {
	expr := opts.pattern
	if opts.ignoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %w", err)
	}
	info, err := os.Stat(searchPath)
	if err != nil {
		return nil, err
	}
	var files []string
	if info.IsDir() {
		rels, err := projectFiles(ctx, searchPath)
		if err != nil {
			return nil, err
		}
		for _, rel := range rels {
			if opts.include == "" || matchGlob(opts.include, rel) {
				files = append(files, filepath.Join(searchPath, filepath.FromSlash(rel)))
			}
		}
	} else {
		files = []string{searchPath}
	}

	g := grepCollector{opts: opts, re: re, limit: grepMaxLines + 1}
	for _, path := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if g.full() {
			break
		}
		g.file(path)
	}
	return g.lines, nil
}

// grepCollector gathers output lines in rg's format: "path:N:text" for a
// match, "path-N-text" for context, "--" between separate groups.
type grepCollector struct {
	opts    grepOptions
	re      *regexp.Regexp
	limit   int
	lines   []string
	printed bool // any group emitted yet, for the "--" separator
}

func (g *grepCollector) full() bool { return len(g.lines) >= g.limit }

type grepHeldLine struct {
	n    int
	text string
}

func (g *grepCollector) file(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 64*1024)
	if sample, _ := br.Peek(8192); sniffText(sample) == textBinary {
		return
	}
	shown := g.opts.display(path)
	n := g.opts.context
	var before []grepHeldLine
	lastPrinted, afterLeft := 0, 0
	emit := func(sep string, ln int, text string) {
		text = textutil.ClipBytes(strings.TrimRight(text, "\r"), 500, " [... omitted]")
		g.lines = append(g.lines, shown+sep+strconv.Itoa(ln)+sep+text)
		lastPrinted = ln
	}

	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for ln := 1; scanner.Scan(); ln++ {
		if g.full() {
			return
		}
		line := scanner.Text()
		switch {
		case g.re.MatchString(line):
			if g.opts.filesOnly {
				g.lines = append(g.lines, shown)
				return
			}
			if n > 0 {
				// A group that does not touch the previous one (or is in
				// another file) is separated by "--", as rg and grep do.
				first := ln - len(before)
				if g.printed && (lastPrinted == 0 || first > lastPrinted+1) {
					g.lines = append(g.lines, "--")
				}
				for _, h := range before {
					emit("-", h.n, h.text)
				}
				before = before[:0]
				afterLeft = n
			}
			emit(":", ln, line)
			g.printed = true
		case afterLeft > 0:
			emit("-", ln, line)
			afterLeft--
		case n > 0:
			if len(before) == n {
				before = append(before[:0], before[1:]...)
			}
			before = append(before, grepHeldLine{ln, line})
		}
	}
}

func (t *GrepTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("grep is read-only")
}
