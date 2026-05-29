package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	// Always use streaming read — avoids double memory allocation from ReadFile + Split
	return t.readFileStream(path, offset, limit)
}

func (t *ReadTool) readFileStream(path string, offset, limit int) (Result, error) {
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

	maxCollect := limit
	if maxCollect <= 0 {
		maxCollect = 2000
	}

	// Pre-size the builder to avoid repeated reallocation
	var sb strings.Builder
	sb.Grow(maxCollect * 80) // estimate ~80 chars per line
	sb.WriteString("File: ")
	sb.WriteString(path)
	sb.WriteByte('\n')
	sb.WriteByte('\n')

	lineNum := 0
	collected := 0
	totalLines := 0

	for scanner.Scan() {
		lineNum++
		totalLines = lineNum
		if lineNum <= offset {
			continue
		}
		if collected >= maxCollect {
			// Keep counting total lines
			continue
		}
		sb.WriteString(strconv.Itoa(lineNum))
		sb.WriteString(": ")
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
		collected++
	}
	// Count remaining lines if we stopped collecting early
	if collected >= maxCollect {
		for scanner.Scan() {
			totalLines++
		}
	}

	if collected == 0 {
		sb.WriteString("(no lines in range)\n")
	}

	// Prepend total line info
	header := fmt.Sprintf(" (%d lines total)", totalLines)
	// Insert after filename on first line
	result := sb.String()
	nlIdx := strings.IndexByte(result, '\n')
	if nlIdx > 0 {
		result = result[:nlIdx] + header + result[nlIdx:]
	}

	if totalLines > offset+maxCollect {
		result += fmt.Sprintf("\n... [truncated, showing %d/%d lines. Use offset/limit for more.]\n", collected, totalLines)
	}

	return Result{Data: strings.TrimRight(result, "\n")}, nil
}

func (t *ReadTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("read is read-only")
}
