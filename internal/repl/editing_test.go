package repl

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditingKeys(t *testing.T) {
	tests := []struct {
		name, keys, want string
	}{
		{"ctrl+a and ctrl+e", "bc\x01a\x05d\r", "abcd"},
		{"ctrl+u deletes to the start", "abc\x1b[Dxy\x15\r", "c"},
		{"ctrl+k deletes to the end", "abcd\x1b[D\x1b[D\x0b\r", "ab"},
		{"ctrl+w deletes a word", "go test ./x\x17\x17y\r", "go y"},
		{"alt+b and alt+f", "ab cd\x1bb\x1bbX\x1bf\x1bfY\r", "Xab cdY"},
		{"alt+backspace deletes a word", "ab cd\x1b\x7f\r", "ab "},
		{"ctrl+j types a newline", "a\nb\r", "a\nb"},
		// Typed: nothing is buffered behind the Enter (a buffered one is a
		// paste); the text is then delivered at the end of input.
		{"trailing backslash continues the line", "a\\\r", "a\n"},
		{"alt+enter types a newline", "a\x1b\rb\r", "a\nb"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := editOnce(t, tc.keys)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("typed %q, got %q, want %q", tc.keys, got, tc.want)
			}
		})
	}
}

// A bare Esc (nothing behind it in the read) clears the line; on an empty
// line it is ErrEscape, which the front end turns into an interrupt.
func TestEscKey(t *testing.T) {
	if _, err := editOnce(t, "\x1b"); !errors.Is(err, ErrEscape) {
		t.Fatalf("bare Esc on an empty line: err = %v", err)
	}
	t.Setenv("COVE_ESC_INTERRUPT", "0")
	if _, err := editOnce(t, "\x1b"); errors.Is(err, ErrEscape) {
		t.Fatal("COVE_ESC_INTERRUPT=0 did not turn Esc off")
	}
}

func TestCtrlRSearchesHistory(t *testing.T) {
	restore := captureStdout(t)
	defer restore()
	lr := typed("\x12tes\r\r")
	lr.history = []string{"go build ./...", "go test ./internal/x", "git status"}
	lr.histIdx = len(lr.history)
	got, err := lr.editLine()
	if err != nil || got != "go test ./internal/x" {
		t.Fatalf("got %q, %v", got, err)
	}
}

// A prompt with one-key answers takes the key without Enter; Up/Down walk a
// question's options.
func TestPromptKeysAndOptions(t *testing.T) {
	ch := make(chan string, 1)
	SetPromptInput(ch, nil, "")
	consoleMu.Lock()
	permKeys = "yn"
	consoleMu.Unlock()
	if got, err := editOnce(t, "y"); err != nil || got != "y" {
		t.Fatalf("one-key answer: %q, %v", got, err)
	}
	if got, _ := editOnce(t, "yes\r"); got != "yes" {
		t.Fatalf("a typed word must not be cut at its first key: %q", got)
	}

	SetPromptInput(ch, nil, "")
	consoleMu.Lock()
	permOptions = []string{"1. 保留: 不改", "2. 删除: 去掉"}
	consoleMu.Unlock()
	if got, _ := editOnce(t, "\x1b[B\x1b[B\r"); got != "2. 删除: 去掉" {
		t.Fatalf("Down Down = %q", got)
	}
	ClearPermInputCh()
}

func TestHistoryFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	SetHistoryFile(path)
	defer SetHistoryFile("")
	restore := captureStdout(t)
	lr := typed("")
	lr.submit([]rune("first line"))
	lr.submit([]rune("multi\nline"))
	lr.submit([]rune("y")) // one-key answers are not kept
	restore()
	got := loadHistory()
	if strings.Join(got, "|") != "first line|multi\nline" {
		t.Fatalf("history = %q", got)
	}
}

// Every submitted line was recorded, so "/api-key sk-..." sat in the history
// file and came back with Up. Lines the filter refuses are kept nowhere, and
// ones already in the file are dropped when it is loaded.
func TestHistoryFilterKeepsSecretsOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	SetHistoryFile(path)
	defer SetHistoryFile("")
	restore := captureStdout(t)
	lr := typed("")
	lr.submit([]rune("/api-key sk-old")) // before the filter: in the file
	SetHistoryFilter(func(line string) bool { return !strings.HasPrefix(line, "/api-key") })
	defer SetHistoryFilter(nil)
	lr.submit([]rune("/api-key sk-secret"))
	lr.submit([]rune("hello there"))
	restore()
	for _, h := range lr.history {
		if strings.Contains(h, "sk-secret") {
			t.Fatalf("in-memory history kept %q", h)
		}
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-secret") {
		t.Fatalf("history file kept the key: %s", raw)
	}
	if got := loadHistory(); strings.Join(got, "|") != "hello there" {
		t.Fatalf("loaded history = %q, want the old key filtered out too", got)
	}
}

func TestLayoutInput(t *testing.T) {
	rows, curRow, curOff := layoutInput("❯ ", 2, []rune("ab\ncdefghijkl"), 5, 12)
	// avail = 12-1-2 = 9 → "ab" | "cdefghijk" | "l"
	if len(rows) != 3 || string(rows[1].text) != "cdefghijk" || string(rows[2].text) != "l" {
		t.Fatalf("rows = %+v", rows)
	}
	if curRow != 1 || curOff != 2 {
		t.Fatalf("cursor at row %d off %d, want 1,2", curRow, curOff)
	}
	if rows[1].prefix != "  " {
		t.Fatalf("continuation prefix %q", rows[1].prefix)
	}
}
