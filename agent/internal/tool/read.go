package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type ReadTool struct{ baseTool }

func NewReadTool() Tool {
	return &ReadTool{baseTool{def: Def{
		Name: "read", Aliases: []string{"Read"},
		Description: "Read a file or directory from the filesystem. Returns contents with line numbers for files, or directory listing.",
		InputSchema: json.RawMessage(`{
			"type":"object",
			"properties":{
				"filePath":{"type":"string","description":"Absolute path to the file or directory to read"},
				"offset":{"type":"integer","description":"Line number to start reading from (1-indexed)"},
				"limit":{"type":"integer","description":"Maximum number of lines to read"}
			},
			"required":["filePath"]
		}`),
		IsReadOnly: true, IsConcurrencySafe: true, UserFacingName: "Read",
	}}}
}

func (t *ReadTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	path, _ := input["filePath"].(string)
	if path == "" {
		return Result{Data: "Error: filePath is required", IsError: true}, nil
	}

	if !filepath.IsAbs(path) && tctx.Cwd != "" {
		path = filepath.Join(tctx.Cwd, path)
	}
	path = filepath.Clean(path)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{Data: "Error: file not found: " + path, IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	if info.IsDir() {
		return t.readDir(path)
	}
	return t.readFile(path, input)
}

func (t *ReadTool) readDir(path string) (Result, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	var sb strings.Builder
	sb.WriteString("Directory: " + path + "\n\n")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		sb.WriteString(name + "\n")
	}
	return Result{Data: strings.TrimRight(sb.String(), "\n")}, nil
}

func (t *ReadTool) readFile(path string, input Input) (Result, error) {
	offset := 0
	limit := 0
	if o, ok := input["offset"].(float64); ok && o > 0 {
		offset = int(o) - 1 // convert to 0-indexed
	}
	if l, ok := input["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	// If offset/limit specified, use streaming read to avoid loading entire file
	if offset > 0 || limit > 0 {
		return t.readFileRange(path, offset, limit)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return Result{Data: "Error: permission denied", IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

	content := string(data)
	lines := strings.Split(content, "\n")

	// Cap output to avoid sending huge files to the model
	maxLines := 2000
	end := len(lines)
	if end > maxLines {
		end = maxLines
	}

	var sb strings.Builder
	sb.WriteString("File: " + path)
	sb.WriteString(" (" + itoa(len(lines)) + " lines total)\n\n")

	for i := 0; i < end && i < len(lines); i++ {
		sb.WriteString(itoa(i+1) + ": " + lines[i] + "\n")
	}
	if len(lines) > maxLines {
		sb.WriteString(fmt.Sprintf("\n... [truncated, showing %d/%d lines. Use offset/limit for more.]\n", maxLines, len(lines)))
	}

	return Result{Data: strings.TrimRight(sb.String(), "\n")}, nil
}

func (t *ReadTool) readFileRange(path string, offset, limit int) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return Result{Data: "Error: permission denied", IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 1024*1024) // support long lines

	var sb strings.Builder
	sb.WriteString("File: " + path + "\n\n")

	lineNum := 0
	collected := 0
	maxCollect := limit
	if maxCollect <= 0 {
		maxCollect = 2000
	}

	for scanner.Scan() {
		lineNum++
		if lineNum <= offset {
			continue
		}
		sb.WriteString(itoa(lineNum) + ": " + scanner.Text() + "\n")
		collected++
		if collected >= maxCollect {
			break
		}
	}

	if collected == 0 {
		sb.WriteString("(no lines in range)\n")
	}

	return Result{Data: strings.TrimRight(sb.String(), "\n")}, nil
}

func (t *ReadTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("read is read-only")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
