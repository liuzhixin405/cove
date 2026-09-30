package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
	ctxt "github.com/liuzhixin405/cove-agent/internal/context"
	"github.com/liuzhixin405/cove-agent/internal/tool"
)

// B. Two spellings of one file claim the same write key: a relative and an
// absolute path, and on Windows two letter cases.
func TestWriteClaimKeyNormalizesSpellings(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "proj")
	rel := writeClaimKey(filepath.Join("internal", "x.go"), cwd)
	abs := writeClaimKey(filepath.Join(cwd, "internal", "x.go"), cwd)
	if rel != abs {
		t.Fatalf("relative %q and absolute %q claim different keys", rel, abs)
	}
	if runtime.GOOS == "windows" && writeClaimKey("X.go", cwd) != writeClaimKey("x.go", cwd) {
		t.Fatal("X.go and x.go claim different keys on Windows")
	}
}

// B. Edits to one file spelled two ways do not run in parallel.
func TestEditsToOneFileSpelledTwoWaysAreSerialized(t *testing.T) {
	dir := t.TempDir()
	edit := &overlapTool{name: "edit", delay: 80 * time.Millisecond}
	second := filepath.Join(dir, "internal", "x.go")
	if runtime.GOOS == "windows" {
		second = filepath.Join(dir, "internal", "X.go")
	}
	prov := &mockProvider{responses: []mockResponse{
		{toolCalls: []api.ToolCall{
			{ID: "1", Name: "edit", Input: map[string]any{"filePath": "internal/x.go"}},
			{ID: "2", Name: "edit", Input: map[string]any{"filePath": second}},
		}},
		{content: "done"},
	}}
	eng := newTestEngine(prov, edit)
	eng.projCtx = &ctxt.ProjectContext{Cwd: dir}
	if _, err := eng.RunMessageWithStream(context.Background(), api.Message{Role: "user", Content: "edit x twice"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := edit.peakInFlight(); got != 1 {
		t.Fatalf("%d edits of one file were in flight at once", got)
	}
}

// otherSpanTool is a spanTool that is concurrency-safe but not read-only,
// like agent or an MCP tool.
type otherSpanTool struct{ *spanTool }

func (o otherSpanTool) Def() tool.Def {
	d := o.spanTool.Def()
	d.IsReadOnly, d.IsConcurrencySafe = false, true
	return d
}

// C. [agent "refactor P", read P] ran the read during the agent: both were
// "concurrency-safe observers". A reader now waits for the agents before it.
func TestReaderWaitsForConcurrentNonReadOnlyCall(t *testing.T) {
	agent := otherSpanTool{&spanTool{name: "agent", delay: 200 * time.Millisecond}}
	read := &spanTool{name: "read", safe: true}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "agent", Input: map[string]any{"filePath": "a"}},
		{ID: "2", Name: "read", Input: map[string]any{"filePath": "r"}},
	}, agent, read)
	a, r := agent.span("a"), read.span("r")
	if a[1].IsZero() || r[0].IsZero() {
		t.Fatalf("a call did not run: agent=%v read=%v", a, r)
	}
	if r[0].Before(a[1]) {
		t.Fatalf("read started %v before the agent finished", a[1].Sub(r[0]))
	}
}

// C. Agents among themselves still overlap.
func TestAgentsStillRunInParallel(t *testing.T) {
	agent := otherSpanTool{&spanTool{name: "agent", delay: 150 * time.Millisecond}}
	runBatch(t, []api.ToolCall{
		{ID: "1", Name: "agent", Input: map[string]any{"filePath": "a"}},
		{ID: "2", Name: "agent", Input: map[string]any{"filePath": "b"}},
	}, agent)
	a, b := agent.span("a"), agent.span("b")
	if a[1].IsZero() || b[1].IsZero() {
		t.Fatal("an agent did not run")
	}
	if !a[0].Before(b[1]) || !b[0].Before(a[1]) {
		t.Fatalf("agents ran serially: a=[%v,%v] b=[%v,%v]", a[0], a[1], b[0], b[1])
	}
}

// D. Path-qualified and quoted git is git.
func TestGitCommandRePathQualified(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/usr/bin/git push": true,
		`"C:\Program Files\Git\bin\git.exe" commit -m x`: true,
		`& 'C:\Program Files\Git\cmd\git.exe' push`:      true,
		`C:\Git\cmd\git.exe status`:                      true,
		"cd sub && /opt/git/bin/git add -A":              true,
		"cat ./internal/gitdir/x":                        false,
		`"C:\Program Files\Git\bin\gitk.exe" --all`:      false,
		"echo digit":                                         false,
		"ls /usr/share/git-core/templates":                   false,
		`"C:\tools\legit.exe" run`:                           false,
		"python -c 'print(1)' ; git -C sub commit -m 'a; b'": true,
	} {
		if got := gitCommandRe.MatchString(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

// D. A git push that fails (or times out) still leaves the status line: it
// was recorded only once the shell call had succeeded.
func TestFailedGitPushIsRecorded(t *testing.T) {
	bash := &mockTool{name: "bash", readOnly: true, err: errors.New("exit status 128: timed out")}
	prov := &seqProvider{reply: func(_ context.Context, n int, _ api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("1", "bash", map[string]any{"command": "/usr/bin/git push origin main"}), nil
		}
		return &api.ChatResponse{Content: "push failed"}, nil
	}}
	eng := newPatternEngine(t, prov, nil, bash)
	eng.projCtx = &ctxt.ProjectContext{Cwd: t.TempDir()}
	if _, err := run(t, eng, "push it"); err != nil {
		t.Fatal(err)
	}
	eng.fileMu.Lock()
	ran := eng.turnRanGit
	eng.fileMu.Unlock()
	if !ran {
		t.Fatal("a failed git push was not recorded")
	}
}

// E. Only git commands that can change the repository count.
func TestGitChangesRepo(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git log --oneline -5":              false,
		"git diff HEAD~1":                   false,
		"git status -s":                     false,
		"git show HEAD":                     false,
		"git fetch origin":                  false,
		"git branch -a":                     false,
		"git branch --contains abc":         false,
		"git tag -l 'v*'":                   false,
		"git stash list":                    false,
		"git -C sub log":                    false,
		"git -c core.pager=cat diff":        false,
		"git commit -m 'x'":                 true,
		"git push":                          true,
		"git -C sub add -A":                 true,
		"git --no-pager log; git push":      true,
		"git status && git commit -am x":    true,
		"git branch -D old":                 true,
		"git branch feature":                true,
		"git tag v1.2":                      true,
		"git stash":                         true,
		"git stash pop":                     true,
		"git checkout -- a.go":              true,
		"git restore --staged a.go":         true,
		"git cherry-pick abc":               true,
		`"C:\Git\bin\git.exe" reset --hard`: true,
		"echo git commit":                   true, // a limitation: echo is not parsed
		"go test ./...":                     false,
	} {
		if got := gitChangesRepo(cmd); got != want {
			t.Errorf("%q: %v, want %v", cmd, got, want)
		}
	}
}

// E. A read-only git command does not produce the status line.
func TestReadOnlyGitCommandRecordsNothing(t *testing.T) {
	eng := newTestEngine(&mockProvider{})
	eng.projCtx = &ctxt.ProjectContext{Cwd: t.TempDir()}
	eng.noteGitInvocation(api.ToolCall{Name: "bash", Input: map[string]any{"command": "git log --oneline"}})
	if eng.turnRanGit {
		t.Fatal("git log counted as a repository-changing command")
	}
	eng.noteGitInvocation(api.ToolCall{Name: "powershell", Input: map[string]any{"command": "git commit -m x"}})
	if !eng.turnRanGit {
		t.Fatal("git commit not recorded")
	}
}

// F. Text between a block cut short by truncation and a later </todo_list>
// is not deleted; only the block todoAfterCompaction appended is replaced.
func TestTodoAfterCompactionReplacesOnlyItsOwnBlock(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.runtime.SetTodos(sampleTodos())
	summary := "<compress>summary <todo_list>\nold cut short... IMPORTANT FACT kept here\n" +
		todoBlock(compactionTodoIntro, "[ ] todo-9. stale\n") + "</compress>"
	eng.messages = []api.Message{{Role: "user", Content: summary}, {Role: "assistant", Content: "ok"}}
	eng.todoAfterCompaction(true)
	c := eng.messages[0].Content
	if !strings.Contains(c, "IMPORTANT FACT kept here") {
		t.Fatalf("text before the old block was deleted:\n%s", c)
	}
	if strings.Contains(c, "todo-9. stale") || strings.Count(c, compactionTodoIntro) != 1 || !strings.Contains(c, "fix the tokenizer") {
		t.Fatalf("own block not replaced by the current list:\n%s", c)
	}
	// A <todo_list> the function did not write is not treated as its own.
	eng.messages = []api.Message{{Role: "user", Content: "<compress>x <todo_list>\nmodel text</todo_list> tail</compress>"}, {Role: "assistant", Content: "ok"}}
	eng.todoAfterCompaction(true)
	if !strings.Contains(eng.messages[0].Content, "model text</todo_list> tail") {
		t.Fatalf("foreign block changed:\n%s", eng.messages[0].Content)
	}
}

// G. Three compactions: the latest request is kept, blocks never nest, and
// compaction messages are engine text.
func TestRepeatedCompactionKeepsLatestRequestWithoutNesting(t *testing.T) {
	cc := NewChatCompressor()
	pad := strings.Repeat("x", 400)
	summary := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "Summary: the assistant read a.go several times while working through the tasks; nothing failed."}, nil
	}
	appendTask := func(msgs []api.Message, req string) []api.Message {
		msgs = append(msgs, api.Message{Role: "user", Content: req})
		for i := 0; i < 7; i++ {
			id := fmt.Sprintf("%s-%d", req[:6], i)
			msgs = append(msgs,
				api.Message{Role: "assistant", Content: "step " + pad, ToolCalls: []api.ToolCall{{ID: id, Name: "read", Input: map[string]any{"filePath": "a.go"}}}},
				api.Message{Role: "tool", ToolCallID: id, Name: "read", Content: "contents " + pad},
			)
		}
		return msgs
	}
	msgs := appendTask(nil, "task one: refactor the auth module")
	for n, req := range []string{"task two: add rate limiting", "task three: fix the login bug", "task four: write the docs"} {
		msgs = appendTask(msgs, req)
		res, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 1, summary)
		if !res.Summarized {
			t.Fatalf("compaction %d did not summarize: %+v", n+1, res)
		}
		assertValidSequence(t, out)
		head := out[0]
		if !head.Synthetic || !looksSynthetic(head) {
			t.Fatalf("compaction %d message is not marked as engine text", n+1)
		}
		if c := strings.Count(head.Content, "<original_request>"); c != 1 {
			t.Fatalf("compaction %d: %d <original_request> blocks (nested):\n%s", n+1, c, head.Content)
		}
		if !strings.Contains(head.Content, "<original_request>\n"+req) {
			t.Fatalf("compaction %d kept the wrong request:\n%s", n+1, head.Content)
		}
		msgs = out
	}
	// A history whose only request was summarized away keeps the carried one.
	only := []api.Message{{Role: "user", Content: "<compress summary=\"x\">\n<original_request>\nthe request\n</original_request>\n\ns\n</compress>"}}
	if got := originalRequest(only); got != "the request" {
		t.Fatalf("carried request = %q", got)
	}
	// Old sessions: an unmarked compaction message is recognized by its prefix.
	if !looksSynthetic(api.Message{Role: "user", Content: "<compress summary=\"conversation-history\">..."}) {
		t.Fatal("unmarked compaction message taken for a user request")
	}
}

// H. Ctrl+C during the verify gate is a cancellation, not a failure.
func TestCancelDuringVerifyGateIsNotAFailure(t *testing.T) {
	prov := &seqProvider{reply: func(context.Context, int, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "All done: the change is in place as requested."}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.DoneVerifyCommands = []string{"go build ./..."}
		c.DoneCheck = "off"
		c.ModelFast = "test-fast"
	})
	eng.verifyGate.ledgerPath = filepath.Join(t.TempDir(), "ledger.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng.verifyGate.runner = func(runCtx context.Context, _, _ string) (string, int, error) {
		cancel()
		<-runCtx.Done()
		return "killed", -1, runCtx.Err()
	}
	_, err := eng.RunMessageWithStream(ctx, api.Message{Role: "user", Content: "hi"}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the turn cancelled", err)
	}
	if eng.verifyAttempts != 0 || len(prov.requests()) != 1 {
		t.Fatalf("attempts %d, %d model calls: the cancel was treated as a failed check", eng.verifyAttempts, len(prov.requests()))
	}
	for _, m := range eng.messages {
		if strings.Contains(m.Content, "[verify_gate]") {
			t.Fatalf("verify failure handed to the model: %q", m.Content)
		}
	}
	if eng.lastRoutedModel != "test-fast" {
		t.Fatalf("escalated to %q on a cancel", eng.lastRoutedModel)
	}
	if !eng.HasInterruptedTurn() {
		t.Fatal("the cancelled turn cannot be resumed")
	}
	if n := eng.fastOutcomes.RecentFastModelFailureRate(); n != 0 {
		t.Fatalf("cancel recorded as a fast-model failure (%v)", n)
	}
	if b, _ := readFileIfExists(eng.verifyGate.ledgerPath); b != "" {
		t.Fatalf("cancelled check written to the ledger: %q", b)
	}
}

// I. After a Layer-2 warning the output counts start over: every further
// identical output used to fire again and reach the hard stop.
func TestLayer2ResetClearsOutputCounts(t *testing.T) {
	ld := NewLoopDetector()
	out := strings.Repeat("same output ", 20)
	warned := false
	for i := 0; i < ld.outThresh; i++ {
		if r := ld.RecordOutput(out); r.Detected {
			warned = true
			ld.ResetFingerprintHistory()
		}
	}
	if !warned {
		t.Fatal("setup: no Layer-2 warning")
	}
	for i := 0; i < ld.outThresh-1; i++ {
		if r := ld.RecordOutput(out); r.Detected {
			t.Fatalf("output %d after the reset fired again (fatal=%v)", i+1, r.Fatal)
		}
	}
}

// J. A dedupe stub whose original is summarized away gets the content back.
func TestCompactionRestoresDedupedContent(t *testing.T) {
	cc := NewChatCompressor()
	pad := strings.Repeat("x", 400)
	big := "BIG RESULT " + strings.Repeat("y", 800)
	msgs := []api.Message{{Role: "user", Content: "look at a.go"}}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("c%d", i)
		content := "other " + pad
		if i == 0 {
			content = big
		}
		if i == 8 || i == 9 {
			content = fmt.Sprintf("[identical to earlier tool result for call c0 (%d bytes); content omitted]", len(big))
		}
		msgs = append(msgs,
			api.Message{Role: "assistant", Content: "step " + pad, ToolCalls: []api.ToolCall{{ID: id, Name: "read", Input: map[string]any{"filePath": "a.go"}}}},
			api.Message{Role: "tool", ToolCallID: id, Name: "read", Content: content},
		)
	}
	summary := func(context.Context, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "Summary: the assistant read a.go repeatedly and compared the outputs; no errors."}, nil
	}
	_, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 1, summary)
	byID := map[string]string{}
	for _, m := range out {
		if m.Role == "tool" {
			byID[m.ToolCallID] = m.Content
		}
	}
	if _, kept := byID["c0"]; kept {
		t.Skip("setup: the original survived the compaction")
	}
	if byID["c8"] != big {
		t.Fatalf("first surviving stub not restored: %q", byID["c8"])
	}
	if !strings.Contains(byID["c9"], "call c8 ") {
		t.Fatalf("later stub not pointed at the restored one: %q", byID["c9"])
	}
}

// K. Tier words are whole tokens: gemini is not "mini".
func TestIsFastModelNameTokens(t *testing.T) {
	for name, want := range map[string]bool{
		"gemini-2.5-pro":        false,
		"gemini-2.5-flash":      true,
		"gemini-2.0-flash-lite": true,
		"gpt-4o-mini":           true,
		"o4-mini":               true,
		"claude-3-5-haiku":      true,
		"claude-opus-4":         false,
		"gpt-5-nano":            true,
		"deepseek-chat":         false,
		"vendor/fast:latest":    true,
		"test-model":            false,
	} {
		if got := isFastModelName(name); got != want {
			t.Errorf("isFastModelName(%q) = %v, want %v", name, got, want)
		}
	}
}

// K. Verify retries exhausted count against the fast model the turn started
// on, although escalation moved the turn to the premium model.
func TestExhaustedVerifyRetriesCountAgainstTheStartModel(t *testing.T) {
	prov := &seqProvider{reply: func(context.Context, int, api.ChatRequest) (*api.ChatResponse, error) {
		return &api.ChatResponse{Content: "All done: the change is in place as requested."}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) {
		c.DoneVerifyCommands = []string{"go build ./..."}
		c.DoneCheck = "off"
		c.ModelFast = "test-fast"
	})
	eng.verifyGate.ledgerPath = ""
	eng.verifyGate.runner = func(context.Context, string, string) (string, int, error) { return "boom", 1, nil }
	if _, err := run(t, eng, "hi"); err != nil {
		t.Fatal(err)
	}
	reqs := prov.requests()
	if reqs[0].Model != "test-fast" || reqs[len(reqs)-1].Model != "test-model" {
		t.Fatalf("setup: models %q → %q", reqs[0].Model, reqs[len(reqs)-1].Model)
	}
	if r := eng.fastOutcomes.RecentFastModelFailureRate(); r != 1 {
		t.Fatalf("fast-model failure rate = %v, want the exhausted retries recorded", r)
	}
}

// L. A compaction that shrinks the history below the review throttle's
// count rebases it, so skill review is not off for the rest of the session.
func TestCompactionRebasesReviewThrottle(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.bgMu.Lock()
	eng.lastReviewMsgCount = 40
	gen := eng.conversationGen
	eng.bgMu.Unlock()
	eng.rebaseReviewThrottle(50, 12)
	eng.bgMu.Lock()
	defer eng.bgMu.Unlock()
	if eng.lastReviewMsgCount != 2 {
		t.Fatalf("lastReviewMsgCount = %d, want 2 (10 unreviewed messages of the 12 kept)", eng.lastReviewMsgCount)
	}
	if eng.conversationGen == gen {
		t.Fatal("a review still running on the old history would move the throttle")
	}
}

// L. Through compact: the throttle follows the rewrite.
func TestCompactRebasesReviewThrottle(t *testing.T) {
	isolatedHome(t)
	prov := &mockProvider{responses: []mockResponse{{content: "Summary: the assistant read a.go several times; nothing failed along the way."}}}
	eng := newTestEngine(prov)
	eng.messages = buildConversation()
	n := len(eng.messages)
	eng.bgMu.Lock()
	eng.lastReviewMsgCount = n
	eng.bgMu.Unlock()
	eng.updateTokenCount()
	if res := eng.compact(context.Background(), 0); res == nil || !res.Summarized {
		t.Fatalf("setup: compaction did not summarize: %+v", res)
	}
	eng.bgMu.Lock()
	defer eng.bgMu.Unlock()
	if eng.lastReviewMsgCount > len(eng.messages) {
		t.Fatalf("throttle count %d above the %d messages left", eng.lastReviewMsgCount, len(eng.messages))
	}
}

// M. A compaction that trimmed tool results but could not summarize says
// so, and reports the rewrite so the cleanup runs.
func TestTrimOnlyCompactionIsReported(t *testing.T) {
	cc := NewChatCompressor()
	big := strings.Repeat("z", 1000)
	// One assistant turn with ten tool calls: the only boundary is at 1, so
	// the history before it is too short to summarize.
	calls := make([]api.ToolCall, 10)
	msgs := []api.Message{{Role: "user", Content: "go"}, {Role: "assistant"}}
	for i := range calls {
		calls[i] = api.ToolCall{ID: fmt.Sprint(i), Name: "read"}
		msgs = append(msgs, api.Message{Role: "tool", ToolCallID: fmt.Sprint(i), Name: "read", Content: big})
	}
	msgs[1].ToolCalls = calls
	res, out := cc.Compress(context.Background(), msgs, countTokens(msgs), 1, nil)
	if out[2].Content == big {
		t.Skip("setup: nothing was trimmed")
	}
	if !res.Compressed || res.Summarized || !strings.Contains(res.Reason, "仅裁剪了旧工具输出") {
		t.Fatalf("trim-only compaction reported as %+v", res)
	}
}

// N. The last resume marker keeps its own place; earlier ones go.
func TestShrinkKeepsTheLastMarkerInPlace(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	m1 := newSyntheticUserMsg("[system: 上一次执行被中断（a）。first]")
	m2 := newSyntheticUserMsg("[system: 上一次执行被中断（b）。second]")
	eng.messages = []api.Message{
		{Role: "user", Content: "req"}, m1,
		{Role: "assistant", Content: "work"},
		{Role: "user", Content: "req"}, m2,
	}
	eng.updateTokenCount()
	if !eng.shrinkForWindow(1 << 30) {
		t.Fatal("a marker was removed, yet shrinkForWindow reported no change")
	}
	want := []string{"req", "work", "req", m2.Content}
	if len(eng.messages) != len(want) {
		t.Fatalf("messages = %+v", eng.messages)
	}
	for i, w := range want {
		if eng.messages[i].Content != w {
			t.Fatalf("message %d = %q, want %q", i, eng.messages[i].Content, w)
		}
	}
}

// N. Nothing to remove: no change is reported, so the single retry is not
// spent on an identical request.
func TestShrinkReportsNoChangeWhenNothingRemoved(t *testing.T) {
	isolatedHome(t)
	eng := newTestEngine(&mockProvider{})
	eng.messages = []api.Message{{Role: "user", Content: "req"}, {Role: "assistant", Content: "ok"}}
	// A provider figure larger than the estimate: re-estimating alone
	// used to make the count drop and read as a change.
	eng.recordUsage(50000, len(eng.messages))
	eng.updateTokenCount()
	if eng.shrinkForWindow(1) {
		t.Fatal("shrinkForWindow reported a change with nothing removed")
	}
}

// readFileIfExists returns the file's content, "" when it does not exist.
func readFileIfExists(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}
