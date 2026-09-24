package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// grepMaxLines caps one grep result; the rest is summarized as a count.
const grepMaxLines = 100

// lookRipgrep finds rg; tests replace it to exercise the fallback.
var lookRipgrep = func() (string, error) { return exec.LookPath("rg") }

type GrepTool struct{ baseTool }

func NewGrepTool() Tool {
	return &GrepTool{baseTool{def: Def{
		Name: "grep", Description: "Fast content search with regex. Uses ripgrep (rg) when available, otherwise a built-in search. Honors .gitignore inside git repositories.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"Regex pattern to search for"},
				"include":{"type":"string","description":"Glob pattern to filter files (e.g. *.go, *.ts)"},
				"path":{"type":"string","description":"Directory to search in (defaults to cwd)"}
			},
			"required":["pattern"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Grep",
	}}}
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

	var lines []string
	if rg, err := lookRipgrep(); err == nil {
		lines, err = ripgrep(ctx, rg, pattern, include, searchPath)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
	} else {
		lines, err = builtinGrep(ctx, pattern, include, searchPath)
		if err != nil {
			return Result{Data: "Error: " + err.Error(), IsError: true}, nil
		}
	}

	if len(lines) == 0 {
		return Result{Data: "No matches found"}, nil
	}
	if len(lines) > grepMaxLines {
		more := len(lines) - grepMaxLines
		return Result{Data: strings.Join(lines[:grepMaxLines], "\n") +
			fmt.Sprintf("\n... %d more matches (narrow the pattern, or use include/path)", more)}, nil
	}
	return Result{Data: strings.Join(lines, "\n")}, nil
}

func ripgrep(ctx context.Context, rg, pattern, include, searchPath string) ([]string, error) {
	args := []string{"-n", "--no-heading", "--color=never", "--max-columns=500", "--max-columns-preview"}
	if include != "" {
		args = append(args, "-g", include)
	}
	// -e keeps a pattern that starts with "-" from being read as a flag.
	args = append(args, "-e", pattern, "--", searchPath)
	cmd := exec.CommandContext(ctx, rg, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimSpace(stdout.String())
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
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
	return strings.Split(out, "\n"), nil
}

// builtinGrep is the search used when ripgrep is not installed, which is the
// usual case on Windows. It walks the same files rg would (projectFiles) and
// skips binaries.
func builtinGrep(ctx context.Context, pattern, include, searchPath string) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex: %v", err)
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
			if include == "" || matchGlob(include, rel) {
				files = append(files, filepath.Join(searchPath, filepath.FromSlash(rel)))
			}
		}
	} else {
		files = []string{searchPath}
	}

	var lines []string
	for _, path := range files {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lines = append(lines, grepFile(path, re)...)
	}
	return lines, nil
}

func grepFile(path string, re *regexp.Regexp) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReaderSize(f, 64*1024)
	if sample, _ := br.Peek(8192); sniffText(sample) == textBinary {
		return nil
	}
	var lines []string
	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		if re.MatchString(line) {
			line = textutil.ClipBytes(strings.TrimRight(line, "\r"), 500, " [... omitted]")
			lines = append(lines, fmt.Sprintf("%s:%d:%s", path, n, line))
		}
	}
	return lines
}

func (t *GrepTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("grep is read-only")
}
