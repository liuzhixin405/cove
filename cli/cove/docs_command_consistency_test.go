package main

import (
	"bufio"
	"encoding/binary"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestRegisteredCommandsAreDocumented(t *testing.T) {
	root := repoRootFromThisFile(t)
	readmeCmds := extractDocCommands(t, filepath.Join(root, "README.md"))
	manualCmds := extractDocCommands(t, filepath.Join(root, "docs", "USER_MANUAL.md"))

	reg := registerAllCommands()
	var missingInReadme []string
	var missingInManual []string
	seen := map[string]bool{}
	for _, c := range reg.All() {
		name := c.Name()
		if seen[name] {
			continue
		}
		seen[name] = true
		if !readmeCmds[name] {
			missingInReadme = append(missingInReadme, "/"+name)
		}
		if !manualCmds[name] {
			missingInManual = append(missingInManual, "/"+name)
		}
	}
	if len(missingInReadme) > 0 {
		sort.Strings(missingInReadme)
		t.Fatalf("README.md missing registered commands: %s", strings.Join(missingInReadme, ", "))
	}
	if len(missingInManual) > 0 {
		sort.Strings(missingInManual)
		t.Fatalf("docs/USER_MANUAL.md missing registered commands: %s", strings.Join(missingInManual, ", "))
	}
}

func TestDocumentedCommandsAreImplemented(t *testing.T) {
	root := repoRootFromThisFile(t)
	paths := []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "docs", "USER_MANUAL.md"),
	}

	implemented := map[string]bool{}
	reg := registerAllCommands()
	for _, c := range reg.All() {
		implemented[c.Name()] = true
	}
	for _, name := range []string{
		"help", "exit", "quit",
		"attach",
		"model", "provider", "api-key", "base-url", "mode", "budget",
		"tasks", "stop", "cancel",
		"skill",
	} {
		implemented[name] = true
	}

	for _, p := range paths {
		docCmds := extractDocCommands(t, p)
		var unknown []string
		for name := range docCmds {
			if !implemented[name] {
				unknown = append(unknown, "/"+name)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			t.Fatalf("%s contains undocumented/unsupported commands: %s", filepath.Base(p), strings.Join(unknown, ", "))
		}
	}
}

func TestUserManualMemoryAddSyntaxAndTaskModeNotes(t *testing.T) {
	root := repoRootFromThisFile(t)
	manual := readTextFile(t, filepath.Join(root, "docs", "USER_MANUAL.md"))

	if !strings.Contains(manual, "/memory add <名称> <内容>") {
		t.Fatalf("docs/USER_MANUAL.md must document '/memory add <名称> <内容>'")
	}
	if !strings.Contains(manual, "`/tasks` | 查看运行中/排队任务（TUI）；headless 显示同步执行状态") {
		t.Fatalf("docs/USER_MANUAL.md must describe /tasks TUI/headless mode difference")
	}
	if !strings.Contains(manual, "`/stop` 或 `/cancel` | 取消当前任务（TUI）；headless 无后台任务可取消") {
		t.Fatalf("docs/USER_MANUAL.md must describe /stop /cancel TUI/headless mode difference")
	}
}

func repoRootFromThisFile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve current test file path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

func extractDocCommands(t *testing.T, path string) map[string]bool {
	t.Helper()
	text := readTextFile(t, path)
	cmds := map[string]bool{}
	re := regexp.MustCompile(`/([a-z][a-z0-9\-]*)`)

	s := bufio.NewScanner(strings.NewReader(text))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		firstCol := strings.TrimSpace(cells[1])
		if !strings.HasPrefix(firstCol, "`/") {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(firstCol, -1) {
			if len(m) > 1 {
				cmds[m[1]] = true
			}
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return cmds
}

func readTextFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(b) >= 2 {
		// UTF-16 LE BOM
		if b[0] == 0xFF && b[1] == 0xFE {
			return decodeUTF16(t, b[2:], binary.LittleEndian)
		}
		// UTF-16 BE BOM
		if b[0] == 0xFE && b[1] == 0xFF {
			return decodeUTF16(t, b[2:], binary.BigEndian)
		}
	}
	// UTF-8 BOM
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	return string(b)
}

func decodeUTF16(t *testing.T, b []byte, order binary.ByteOrder) string {
	t.Helper()
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, order.Uint16(b[i:i+2]))
	}
	r := utf16.Decode(u16)
	return string(r)
}
