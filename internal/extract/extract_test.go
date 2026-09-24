package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/fsatomic"
)

// fakeProvider is a counting api.Provider stand-in. Chat returns a canned
// response and records every request so tests can assert both how many times
// the model was consulted and what it was asked.
type fakeProvider struct {
	mu          sync.Mutex
	calls       int
	streamCalls int
	requests    []api.ChatRequest
	response    string
	err         error
}

func (f *fakeProvider) Name() string        { return "fake" }
func (f *fakeProvider) DisplayName() string { return "Fake" }
func (f *fakeProvider) Validate() error     { return nil }

func (f *fakeProvider) Chat(ctx context.Context, req api.ChatRequest) (*api.ChatResponse, error) {
	f.mu.Lock()
	f.calls++
	f.requests = append(f.requests, req)
	resp, err := f.response, f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &api.ChatResponse{Content: resp, Model: req.Model}, nil
}

func (f *fakeProvider) ChatStream(ctx context.Context, req api.ChatRequest, handler api.StreamHandler) (*api.ChatResponse, error) {
	f.mu.Lock()
	f.streamCalls++
	f.mu.Unlock()
	return nil, errors.New("extract must not use the streaming API")
}

func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeProvider) lastRequest(t *testing.T) api.ChatRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("provider was never called")
	}
	return f.requests[len(f.requests)-1]
}

// newTestRunner builds a Runner whose memory directory is a temp dir. HOME and
// USERPROFILE are redirected as well so NewRunner's own ~/.cove/memory lookup
// cannot touch the developer's real memories.
func newTestRunner(t *testing.T, p api.Provider) (*Runner, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	r := NewRunner(p, "extract-model")
	dir := filepath.Join(t.TempDir(), "memory")
	r.memoryDir = dir
	return r, dir
}

// conversation returns n messages, enough to clear Extract's minimum-length gate.
func conversation(n int) []api.Message {
	msgs := make([]api.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, api.Message{Role: role, Content: fmt.Sprintf("message %d", i)})
	}
	return msgs
}

func memoryBlock(file, mode, content string) string {
	return fmt.Sprintf("---MEMORY---\nFILE: %s\nMODE: %s\nCONTENT:\n%s\n---END---\n", file, mode, content)
}

func readMemory(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read memory %s: %v", name, err)
	}
	return string(data)
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	for _, e := range entries {
		if fsatomic.IsTempName(e.Name()) {
			t.Errorf("atomic-write temp file left behind in %s: %s", dir, e.Name())
		}
	}
}

func TestNewRunnerUsesHomeMemoryDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	r := NewRunner(&fakeProvider{}, "m")
	want := filepath.Join(home, ".cove", "memory")
	if r.memoryDir != want {
		t.Fatalf("memoryDir = %q, want %q (NewRunner must honour the redirected home)", r.memoryDir, want)
	}
}

func TestExtractThrottlesBackToBackCalls(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, _ := newTestRunner(t, p)
	msgs := conversation(6)

	r.Extract(context.Background(), msgs)
	r.Extract(context.Background(), msgs)
	r.Extract(context.Background(), msgs)

	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times for three back-to-back Extracts, want 1 (%v throttle)", got, minExtractInterval)
	}
}

func TestExtractConcurrentCallsClaimExactlyOneSlot(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, _ := newTestRunner(t, p)
	msgs := conversation(6)

	const goroutines = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // maximise the overlap on the throttle check
			r.Extract(context.Background(), msgs)
		}()
	}
	close(start)
	wg.Wait()

	if got := p.callCount(); got != 1 {
		t.Fatalf("provider called %d times for %d concurrent Extracts, want 1; the throttle claim is not atomic", got, goroutines)
	}
}

func TestExtractRunsAgainOnceIntervalElapsed(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, _ := newTestRunner(t, p)
	msgs := conversation(6)

	r.Extract(context.Background(), msgs)
	if got := p.callCount(); got != 1 {
		t.Fatalf("first Extract made %d calls, want 1", got)
	}

	// Rewind the claim instead of sleeping for two minutes.
	r.mu.Lock()
	r.lastExtract = time.Now().Add(-minExtractInterval - time.Second)
	r.mu.Unlock()

	r.Extract(context.Background(), msgs)
	if got := p.callCount(); got != 2 {
		t.Fatalf("Extract made %d calls after the interval elapsed, want 2", got)
	}
}

func TestExtractIgnoresShortConversations(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, _ := newTestRunner(t, p)

	r.Extract(context.Background(), conversation(3))

	if got := p.callCount(); got != 0 {
		t.Fatalf("provider called %d times for a 3-message conversation, want 0", got)
	}
}

func TestExtractWritesMemoryAndReportsCount(t *testing.T) {
	p := &fakeProvider{response: "Found something.\n" +
		memoryBlock("project-architecture.md", "write", "Cove is a Go CLI agent.\nState lives under ~/.cove.") +
		memoryBlock("api-conventions.md", "write", "All state writes go through fsatomic.")}
	r, dir := newTestRunner(t, p)

	var savedCh = make(chan int, 1)
	r.OnSave = func(n int) { savedCh <- n }

	r.Extract(context.Background(), conversation(6))

	if got := p.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	if p.streamCalls != 0 {
		t.Errorf("Extract used the streaming API %d times, want 0", p.streamCalls)
	}

	select {
	case n := <-savedCh:
		if n != 2 {
			t.Errorf("OnSave got count %d, want 2", n)
		}
	default:
		t.Error("OnSave was never invoked although two memories were written")
	}

	if got := readMemory(t, dir, "project-architecture.md"); got != "Cove is a Go CLI agent.\nState lives under ~/.cove." {
		t.Errorf("project-architecture.md = %q", got)
	}
	if got := readMemory(t, dir, "api-conventions.md"); got != "All state writes go through fsatomic." {
		t.Errorf("api-conventions.md = %q", got)
	}
	assertNoTempFiles(t, dir)

	req := p.lastRequest(t)
	if req.Model != "extract-model" {
		t.Errorf("request model = %q, want %q", req.Model, "extract-model")
	}
	if req.SystemBase != extractSystemPrompt {
		t.Error("request did not carry the extraction system prompt")
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Fatalf("request messages = %+v, want a single user message", req.Messages)
	}
	if !strings.Contains(req.Messages[0].Content, "## Recent conversation:") {
		t.Error("request prompt does not include the conversation section")
	}
}

func TestExtractAppendModeKeepsExistingContent(t *testing.T) {
	p := &fakeProvider{response: memoryBlock("notes.md", "append", "New fact: the throttle is two minutes.")}
	r, dir := newTestRunner(t, p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Deliberately dissimilar so the dedup path is not what triggers the append.
	existing := "Old fact: zzzz qqqq wwww unrelated text 1234567890"
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(existing), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r.Extract(context.Background(), conversation(6))

	want := existing + "\nNew fact: the throttle is two minutes."
	if got := readMemory(t, dir, "notes.md"); got != want {
		t.Errorf("notes.md =\n%q\nwant\n%q", got, want)
	}
}

func TestExtractWriteModeReplacesDissimilarContent(t *testing.T) {
	p := &fakeProvider{response: memoryBlock("notes.md", "write", "Brand new unrelated memory body.")}
	r, dir := newTestRunner(t, p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("zzzz qqqq 1234567890 nothing alike"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r.Extract(context.Background(), conversation(6))

	if got := readMemory(t, dir, "notes.md"); got != "Brand new unrelated memory body." {
		t.Errorf("notes.md = %q, want the replacement content only", got)
	}
}

func TestExtractDedupTurnsSimilarWriteIntoAppend(t *testing.T) {
	// The existing memory and the new one are >80% similar, so the dedup guard
	// must merge them even though the model asked for MODE: write.
	existing := "Cove stores its state under ~/.cove and writes it atomically."
	incoming := "Cove stores its state under ~/.cove and writes it atomically!!"
	if s := similarity(existing, incoming); s <= 0.8 {
		t.Fatalf("fixtures are only %.2f similar; the test would not reach the dedup branch", s)
	}

	p := &fakeProvider{response: memoryBlock("state.md", "write", incoming)}
	r, dir := newTestRunner(t, p)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.md"), []byte(existing), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	r.Extract(context.Background(), conversation(6))

	want := existing + "\n" + incoming
	if got := readMemory(t, dir, "state.md"); got != want {
		t.Errorf("state.md =\n%q\nwant\n%q", got, want)
	}
}

func TestExtractProviderErrorWritesNothing(t *testing.T) {
	p := &fakeProvider{err: errors.New("boom")}
	r, dir := newTestRunner(t, p)
	saved := 0
	r.OnSave = func(n int) { saved = n }

	r.Extract(context.Background(), conversation(6))

	if got := p.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1", got)
	}
	if saved != 0 {
		t.Errorf("OnSave called with %d after an API error", saved)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("memory dir was created despite the API error (stat err = %v)", err)
	}
}

func TestExtractNoneResponseWritesNothing(t *testing.T) {
	p := &fakeProvider{response: "NONE"}
	r, dir := newTestRunner(t, p)
	r.Extract(context.Background(), conversation(6))

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a NONE response created the memory dir (stat err = %v)", err)
	}
}

func TestExtractClipsEntryAt5KBOnRuneBoundary(t *testing.T) {
	const maxBytes = 5000
	const suffix = "\n... [truncated]"

	// Shift the start of the Chinese run so byte 5000 lands on each of the three
	// possible offsets inside a 3-byte rune.
	for pad := 0; pad < 3; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			content := strings.Repeat("x", pad) + strings.Repeat("记忆内容", 600) // ~7200 bytes
			p := &fakeProvider{response: memoryBlock("big.md", "write", content)}
			r, dir := newTestRunner(t, p)

			r.Extract(context.Background(), conversation(6))

			data, err := os.ReadFile(filepath.Join(dir, "big.md"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !utf8.Valid(data) {
				t.Fatalf("memory file is not valid UTF-8 after the 5KB entry clip (len=%d); the cap split a rune", len(data))
			}
			if !strings.HasSuffix(string(data), suffix) {
				t.Fatalf("clipped file does not end with the truncation marker; tail = %q", lastRunes(string(data), 30))
			}
			body := len(data) - len(suffix)
			if body > maxBytes || body < maxBytes-2 {
				t.Fatalf("clipped body is %d bytes, want %d..%d", body, maxBytes-2, maxBytes)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

func TestExtractClipsFileAt10KBOnRuneBoundary(t *testing.T) {
	const maxBytes = 10240
	const suffix = "\n... [truncated to 10KB]"

	for pad := 0; pad < 3; pad++ {
		t.Run(fmt.Sprintf("pad=%d", pad), func(t *testing.T) {
			// Existing file is ~9KB of Chinese; appending another clipped entry
			// pushes the total past the 10KB per-file cap.
			existing := strings.Repeat("x", pad) + strings.Repeat("已有记忆", 750) // pad + 9000 bytes
			incoming := strings.Repeat("新的记忆内容", 400)                          // 7200 bytes, clipped to 5KB first

			p := &fakeProvider{response: memoryBlock("grow.md", "append", incoming)}
			r, dir := newTestRunner(t, p)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "grow.md"), []byte(existing), 0644); err != nil {
				t.Fatalf("seed: %v", err)
			}

			r.Extract(context.Background(), conversation(6))

			data, err := os.ReadFile(filepath.Join(dir, "grow.md"))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !utf8.Valid(data) {
				t.Fatalf("memory file is not valid UTF-8 after the 10KB file clip (len=%d); the cap split a rune", len(data))
			}
			if !strings.HasSuffix(string(data), suffix) {
				t.Fatalf("clipped file does not end with the 10KB marker; tail = %q", lastRunes(string(data), 30))
			}
			body := len(data) - len(suffix)
			if body > maxBytes || body < maxBytes-2 {
				t.Fatalf("clipped body is %d bytes, want %d..%d", body, maxBytes-2, maxBytes)
			}
		})
	}
}

func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func TestParseExtractResponseNone(t *testing.T) {
	for _, in := range []string{"NONE", "  NONE  ", "\nNONE\n"} {
		if got := parseExtractResponse(in); got != nil {
			t.Errorf("parseExtractResponse(%q) = %+v, want nil", in, got)
		}
	}
	// "NONE" is only honoured as the whole reply; a NONE prefix followed by a
	// real block must still yield the block.
	mixed := "NONE\n" + memoryBlock("a.md", "write", "still a fact")
	got := parseExtractResponse(mixed)
	if len(got) != 1 || got[0].Name != "a.md" || got[0].Content != "still a fact" {
		t.Errorf("parseExtractResponse(mixed) = %+v, want one a.md entry", got)
	}
}

func TestParseExtractResponseMultipleBlocks(t *testing.T) {
	resp := "Here is what I found.\n" +
		"---MEMORY---\n" +
		"FILE: project-architecture.md\n" +
		"MODE: write\n" +
		"CONTENT:\n" +
		"Cove is a Go CLI.\n" +
		"Second line.\n" +
		"---END---\n" +
		"some chatter between blocks\n" +
		"---MEMORY---\n" +
		"FILE:   api-conventions.md  \n" +
		"MODE:append\n" +
		"CONTENT:\n" +
		"Use fsatomic for every state write.\n" +
		"---END---\n"

	got := parseExtractResponse(resp)
	want := []memoryEntry{
		{Name: "project-architecture.md", Content: "Cove is a Go CLI.\nSecond line.", Append: false},
		{Name: "api-conventions.md", Content: "Use fsatomic for every state write.", Append: true},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseExtractResponseModeHandling(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		wantAppend bool
	}{
		{"append", true},
		{"write", false},
		{"", false},
		// The comparison is an exact match on "append", so anything else — a
		// different case included — means overwrite.
		{"APPEND", false},
		{"append the content", false},
	} {
		got := parseExtractResponse(memoryBlock("f.md", tc.mode, "body"))
		if len(got) != 1 {
			t.Fatalf("mode %q: parsed %d entries, want 1", tc.mode, len(got))
		}
		if got[0].Append != tc.wantAppend {
			t.Errorf("mode %q: Append = %v, want %v", tc.mode, got[0].Append, tc.wantAppend)
		}
	}
}

func TestParseExtractResponseIgnoresMalformedBlocks(t *testing.T) {
	for name, resp := range map[string]string{
		"no markers at all":  "I did not find anything worth saving.",
		"no FILE line":       "---MEMORY---\nMODE: write\nCONTENT:\nbody\n---END---",
		"no CONTENT line":    "---MEMORY---\nFILE: x.md\nMODE: write\n---END---",
		"empty content":      "---MEMORY---\nFILE: x.md\nCONTENT:\n---END---",
		"whitespace content": "---MEMORY---\nFILE: x.md\nCONTENT:\n   \n\t\n---END---",
		"empty FILE value":   "---MEMORY---\nFILE:\nCONTENT:\nbody\n---END---",
		// The scan stops at CONTENT:, so a FILE line after it is never seen.
		"FILE after CONTENT": "---MEMORY---\nCONTENT:\nbody\nFILE: x.md\n---END---",
		"only end marker":    "---END---\nFILE: x.md\nCONTENT:\nbody",
	} {
		if got := parseExtractResponse(resp); len(got) != 0 {
			t.Errorf("%s: parsed %+v, want no entries", name, got)
		}
	}
}

func TestParseExtractResponseToleratesMissingEndMarker(t *testing.T) {
	resp := "---MEMORY---\nFILE: unterminated.md\nMODE: append\nCONTENT:\nthe model forgot the end marker"
	got := parseExtractResponse(resp)
	if len(got) != 1 {
		t.Fatalf("parsed %d entries, want 1", len(got))
	}
	if got[0].Name != "unterminated.md" || got[0].Content != "the model forgot the end marker" || !got[0].Append {
		t.Errorf("entry = %+v", got[0])
	}
}

func TestParseExtractResponseKeepsMultiByteContentIntact(t *testing.T) {
	body := "项目使用 Go 1.25,状态写入走 fsatomic。\n第二行说明。"
	got := parseExtractResponse(memoryBlock("cn.md", "write", body))
	if len(got) != 1 {
		t.Fatalf("parsed %d entries, want 1", len(got))
	}
	if got[0].Content != body {
		t.Errorf("Content = %q, want %q", got[0].Content, body)
	}
}

func TestSanitizeFilenameExactCases(t *testing.T) {
	// Cases with no path separators and no Windows volume syntax, so
	// filepath.Base behaves identically on every platform.
	for _, tc := range []struct{ in, want string }{
		{"", "memory.md"},
		{".", "memory.md"},
		{"..", "memory.md"},
		{"plain", "plain.md"},
		{"project-notes.md", "project-notes.md"},
		{"a<b>c*d?e|f\"g.md", "a-b-c-d-e-f-g.md"},
		{"has spaces.md", "has spaces.md"},
		{"中文记忆.md", "中文记忆.md"},
		{"no-extension-here", "no-extension-here.md"},
	} {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Path traversal: filepath.Base strips the directory part on every platform
	// when "/" is the separator.
	for _, tc := range []struct{ in, want string }{
		{"../../etc/passwd", "passwd.md"},
		{"/etc/hosts", "hosts.md"},
		{"a/b/c.md", "c.md"},
		{"dir/..", "memory.md"},
	} {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeFilenameNeverEscapesOrReturnsDots(t *testing.T) {
	nasty := []string{
		"", ".", "..", "...", "/", "//", "./", "../", "/../../",
		"..\\..\\windows\\system32\\config",
		"C:\\Windows\\system.ini",
		"a/b\\c:d*e?f\"g<h>i|j",
		"\\\\server\\share\\file.md",
		"con", "nul.md", strings.Repeat("a", 300),
		"中文/../记忆.md", ":", "|", "*", "?",
	}
	for _, in := range nasty {
		got := sanitizeFilename(in)
		if got == "" || got == "." || got == ".." {
			t.Errorf("sanitizeFilename(%q) = %q, which is not a usable filename", in, got)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("sanitizeFilename(%q) = %q, which still contains a path separator", in, got)
		}
		if !strings.Contains(got, ".") {
			t.Errorf("sanitizeFilename(%q) = %q, which has no extension", in, got)
		}
		if filepath.Base(got) != got {
			t.Errorf("sanitizeFilename(%q) = %q, which is not a bare file name", in, got)
		}
		// Joining it onto a directory must not climb out of that directory.
		joined := filepath.Clean(filepath.Join("/root/memory", got))
		if !strings.HasPrefix(filepath.ToSlash(joined), "/root/memory/") {
			t.Errorf("sanitizeFilename(%q) = %q escapes its directory: %q", in, got, joined)
		}
	}
}

func TestSanitizedNameIsActuallyWritable(t *testing.T) {
	// A sanitized name has to survive a real atomic write, which is where a
	// stray separator or an empty name would blow up.
	p := &fakeProvider{response: memoryBlock("../../escape/../../../secret", "write", "should stay inside the memory dir")}
	r, dir := newTestRunner(t, p)

	r.Extract(context.Background(), conversation(6))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("memory dir holds %d entries, want 1", len(entries))
	}
	if entries[0].Name() != "secret.md" {
		t.Errorf("wrote %q, want %q", entries[0].Name(), "secret.md")
	}
	if got := readMemory(t, dir, "secret.md"); got != "should stay inside the memory dir" {
		t.Errorf("content = %q", got)
	}
}

func TestBuildExtractionPromptListsNoneWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	prompt := buildExtractionPrompt(dir, conversation(2))

	if !strings.Contains(prompt, "## Existing memories:\n(none yet)\n") {
		t.Errorf("prompt does not report an empty memory dir:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## Recent conversation:") {
		t.Error("prompt is missing the conversation section")
	}
	if !strings.HasSuffix(prompt, "If nothing worth saving, reply with just: NONE") {
		t.Error("prompt does not end with the NONE instruction")
	}
	// Non-existent dir behaves like an empty one rather than failing.
	missing := buildExtractionPrompt(filepath.Join(dir, "nope"), conversation(2))
	if !strings.Contains(missing, "(none yet)") {
		t.Errorf("prompt for a missing memory dir:\n%s", missing)
	}
}

func TestBuildExtractionPromptSkipsTempFilesAndDirs(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{"project-architecture.md", "api-conventions.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// A leftover in-progress atomic write must not be advertised as a memory:
	// the model would try to update a file that is about to vanish.
	tempName := ".cove-tmp-project-architecture.md.987654321"
	if !fsatomic.IsTempName(tempName) {
		t.Fatalf("fixture %q is not recognised as a temp name", tempName)
	}
	if err := os.WriteFile(filepath.Join(dir, tempName), []byte("half written"), 0644); err != nil {
		t.Fatalf("seed temp: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	prompt := buildExtractionPrompt(dir, conversation(2))

	for _, name := range []string{"- project-architecture.md\n", "- api-conventions.md\n"} {
		if !strings.Contains(prompt, name) {
			t.Errorf("prompt does not list %q:\n%s", name, prompt)
		}
	}
	if strings.Contains(prompt, tempName) {
		t.Errorf("prompt lists the atomic-write temp file %q:\n%s", tempName, prompt)
	}
	if strings.Contains(prompt, ".cove-tmp-") {
		t.Errorf("prompt leaks a temp-file prefix:\n%s", prompt)
	}
	if strings.Contains(prompt, "- subdir\n") {
		t.Errorf("prompt lists the subdirectory as a memory:\n%s", prompt)
	}
}

func TestBuildExtractionPromptSkipsTempFileWhenItIsTheOnlyEntry(t *testing.T) {
	dir := t.TempDir()
	tempName := ".cove-tmp-only.md.123"
	if err := os.WriteFile(filepath.Join(dir, tempName), []byte("half"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	prompt := buildExtractionPrompt(dir, conversation(2))
	if strings.Contains(prompt, tempName) || strings.Contains(prompt, ".cove-tmp-") {
		t.Errorf("prompt lists the only entry even though it is a temp file:\n%s", prompt)
	}
}

func TestBuildExtractionPromptClipsMessages(t *testing.T) {
	dir := t.TempDir()
	msgs := []api.Message{
		{Role: "user", Content: strings.Repeat("u", 600)},
		{Role: "assistant", Content: "short answer"},
		{Role: "tool", Content: strings.Repeat("t", 600)},
	}
	prompt := buildExtractionPrompt(dir, msgs)

	if !strings.Contains(prompt, "[user] "+strings.Repeat("u", 500)+"...") {
		t.Error("user message was not clipped to 500 bytes")
	}
	if strings.Contains(prompt, strings.Repeat("u", 501)) {
		t.Error("user message exceeded the 500-byte clip")
	}
	// Tool output is untrusted and is never shown to the extractor at all
	// (see TestExtractionPromptLeavesOutToolOutput).
	if strings.Contains(prompt, "ttt") {
		t.Error("tool output reached the extraction prompt")
	}
	if !strings.Contains(prompt, "[assistant] short answer") {
		t.Error("short message was altered")
	}
}

func TestBuildExtractionPromptKeepsValidUTF8WhenClipping(t *testing.T) {
	dir := t.TempDir()
	// Shift the clip point across all three offsets inside a 3-byte rune.
	for pad := 0; pad < 3; pad++ {
		msgs := []api.Message{
			{Role: "user", Content: strings.Repeat("x", pad) + strings.Repeat("会话内容", 200)},
			{Role: "tool", Content: strings.Repeat("x", pad) + strings.Repeat("工具输出", 200)},
		}
		prompt := buildExtractionPrompt(dir, msgs)
		if !utf8.ValidString(prompt) {
			t.Fatalf("pad=%d: prompt is not valid UTF-8; a message clip split a rune", pad)
		}
	}
}

func TestSimilarity(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		lo   float64
		hi   float64
	}{
		{"identical", "abc", "abc", 1, 1},
		{"identical ignoring case", "ABC Def", "abc def", 1, 1},
		{"empty left", "", "x", 0, 0},
		{"empty right", "x", "", 0, 0},
		{"both empty", "", "", 0, 0},
		{"near duplicate crosses dedup threshold", "project uses Go 1.25", "project uses Go 1.24", 0.9, 1},
		{"containment", "hello world", "hello", 0.45, 0.46},
		{"unrelated stays under dedup threshold", "完全不同的中文内容", "totally unrelated ascii", 0, 0.3},
	} {
		got := similarity(tc.a, tc.b)
		if got < tc.lo || got > tc.hi {
			t.Errorf("%s: similarity(%q, %q) = %v, want in [%v, %v]", tc.name, tc.a, tc.b, got, tc.lo, tc.hi)
		}
	}

	// Symmetry and bounds.
	pairs := [][2]string{{"abc", "abd"}, {"a", "abcdefg"}, {"记忆", "记忆内容"}, {"x", "y"}}
	for _, p := range pairs {
		ab, ba := similarity(p[0], p[1]), similarity(p[1], p[0])
		if ab != ba {
			t.Errorf("similarity is asymmetric for %q/%q: %v vs %v", p[0], p[1], ab, ba)
		}
		if ab < 0 || ab > 1 {
			t.Errorf("similarity(%q, %q) = %v, out of [0,1]", p[0], p[1], ab)
		}
	}
}

func TestLevenshteinCountsRunesNotBytes(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
		{"abc", "abc", 0},
		// Byte-based edit distance would report 6 here; the implementation
		// works on runes, so deleting two Chinese characters costs 2.
		{"你好世界", "你好", 2},
		{"记忆", "笔记", 2},
	} {
		if got := levenshtein([]rune(tc.a), []rune(tc.b)); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMinHelper(t *testing.T) {
	if got := min(3, 7); got != 3 {
		t.Errorf("min(3, 7) = %d, want 3", got)
	}
	if got := min(-2, -9); got != -9 {
		t.Errorf("min(-2, -9) = %d, want -9", got)
	}
}
