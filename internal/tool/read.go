package tool

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

	path, err := resolvePathInCwd(path, tctx, false)
	if err != nil {
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}

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
	return t.readFile(path, input, fileTracker(tctx))
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

func (t *ReadTool) readFile(path string, input Input, files *FileTracker) (Result, error) {
	offset := 0
	limit := 0
	if o, ok := input["offset"].(float64); ok && o > 0 {
		offset = int(o) - 1 // convert to 0-indexed
	}
	if l, ok := input["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	// Always use streaming read — avoids double memory allocation from ReadFile + Split
	return t.readFileStream(path, offset, limit, files)
}

func (t *ReadTool) readFileStream(path string, offset, limit int, files *FileTracker) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return Result{Data: "Error: permission denied", IsError: true}, nil
		}
		return Result{Data: "Error: " + err.Error(), IsError: true}, nil
	}
	defer func() { _ = f.Close() }()

	// Every byte read also goes through snap, so the tracker gets a snapshot
	// of the whole file without a second pass over it.
	snap := newSnapshotWriter()
	br := bufio.NewReaderSize(io.TeeReader(f, snap), 64*1024)
	recordSeen := func() error {
		if _, err := io.Copy(io.Discard, br); err != nil {
			return err
		}
		files.record(path, snap.snapshot())
		return nil
	}
	sample, _ := br.Peek(8192)
	kind := sniffText(sample)
	switch kind {
	case textBinary:
		return Result{Data: fmt.Sprintf("Error: %s is a binary file; read only shows text. Use bash (file, xxd, or the format's own tool) to inspect it.", path), IsError: true}, nil
	case textUTF16:
		// The model was told what the file is and how to convert it; writing
		// it again as UTF-8 is one way, which must not then fail with "read it
		// first". A binary file is not recorded: write only produces text.
		_ = recordSeen()
		return Result{Data: fmt.Sprintf("Error: %s is UTF-16 text (often written by PowerShell's > or Out-File). Convert it first, e.g. iconv -f UTF-16 -t UTF-8, or Get-Content | Set-Content -Encoding utf8.", path), IsError: true}, nil
	}

	var src io.Reader = br
	var legacy *legacyText
	if kind == textNotUTF8 {
		// Whether a file is GBK takes all of it (see decodeGBK), so this one
		// case reads the file into memory; the tee still sees every byte.
		data, err := io.ReadAll(br)
		if err != nil {
			return Result{Data: fmt.Sprintf("Error: reading %s failed: %v", path, err), IsError: true}, nil
		}
		src = bytes.NewReader(data)
		if lt, ok := decodeGBK(data); ok {
			legacy, src = &lt, strings.NewReader(lt.text)
		}
	}

	scanner := bufio.NewScanner(src)
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

	// Scanner errors were previously dropped, so a line longer than the 1MB
	// buffer (minified JS, a single-line JSON blob) or a mid-read IO failure
	// silently returned a partial file that looked complete. Surfacing it is
	// essential: a model that edits based on truncated content corrupts the file.
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			// The model goes on to inspect the file with bash; recording it
			// keeps edit usable on it while still catching later changes.
			_ = recordSeen()
			return Result{Data: fmt.Sprintf(
				"Error: %s contains a line longer than the 1MB read limit (stopped at line %d). "+
					"Use bash with head/cut, or grep, to inspect it.", path, totalLines+1),
				IsError: true}, nil
		}
		return Result{Data: fmt.Sprintf("Error: reading %s failed after line %d: %v", path, totalLines, err), IsError: true}, nil
	}

	if err := recordSeen(); err != nil {
		return Result{Data: fmt.Sprintf("Error: reading %s failed after line %d: %v", path, totalLines, err), IsError: true}, nil
	}

	if collected == 0 {
		sb.WriteString("(no lines in range)\n")
	}

	// Prepend total line info
	header := fmt.Sprintf(" (%d lines total)", totalLines)
	switch {
	case legacy != nil:
		header += fmt.Sprintf("\n[note: file is %s-encoded; shown decoded here, and edit/write keep it in %s]", legacy.name, legacy.name)
	case kind == textNotUTF8:
		header += "\n[note: file is not valid UTF-8, and not GBK either (probably another ANSI code page); non-ASCII text below is garbled, and the edit tool will refuse this file]"
	}
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
