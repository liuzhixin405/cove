package engine

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/skills"
)

func anyMessageContains(reqs []api.ChatRequest, text string) bool {
	for _, r := range reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, text) {
				return true
			}
		}
	}
	return false
}

// deepseek-flash is a capable model, not a weak one. Routing a simple turn to
// it must not add extra "be disciplined, you are a lighter model" rules.
func TestFastTierTurnsGetNoWeakModelGuidance(t *testing.T) {
	prov := &seqProvider{}
	eng := newPatternEngine(t, prov, func(c *Config) { c.Model = "deepseek-v4-pro"; c.ModelFast = "deepseek-flash" })
	if _, err := run(t, eng, "hi"); err != nil {
		t.Fatal(err)
	}
	if prov.requests()[0].Model != "deepseek-flash" {
		t.Fatalf("setup: routed to %q", prov.requests()[0].Model)
	}
	if anyMessageContains(prov.requests(), "fast/mid-tier") {
		t.Fatal("weak-model guidance injected for the flash tier")
	}
}

// The "this looks like a multi-step task, plan first with todowrite" note fired
// on any message of 300+ bytes — about 100 Chinese characters — and pushed
// ordinary requests into planning ceremony. Planning is left to the model.
func TestLongRequestsAreNotPushedIntoPlanning(t *testing.T) {
	prov := &seqProvider{}
	eng := newPatternEngine(t, prov, nil)
	msg := strings.Repeat("请帮我看一下这个函数为什么返回空值，", 8) // ~150 characters, ~450 bytes
	if _, err := run(t, eng, msg); err != nil {
		t.Fatal(err)
	}
	if anyMessageContains(prov.requests(), "multi-step task") {
		t.Fatal("per-message planning guidance injected")
	}
}

// A file-type skill was appended to every matching tool result, so reading ten
// .go files put the same skill text into the context ten times.
func TestFileTypeSkillIsInjectedOncePerSession(t *testing.T) {
	mgr := skills.NewManager()
	mgr.Register(skills.Skill{Name: "go-style", Prompt: "GO-SKILL-BODY", Conditional: true, Paths: []string{"*.go"}})
	read := &mockTool{name: "read", readOnly: true, safe: true, result: "package x"}
	prov := &seqProvider{reply: func(ctx context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		switch n {
		case 0:
			return toolCallResp("r1", "read", map[string]any{"filePath": "a.go"}), nil
		case 1:
			return toolCallResp("r2", "read", map[string]any{"filePath": "b.go"}), nil
		default:
			return &api.ChatResponse{Content: "done"}, nil
		}
	}}
	eng := newPatternEngine(t, prov, func(c *Config) { c.SkillManager = mgr }, read)
	if _, err := run(t, eng, "read both"); err != nil {
		t.Fatal(err)
	}
	injected := 0
	for _, m := range eng.Messages() {
		if m.Role == "tool" && strings.Contains(m.Content, "GO-SKILL-BODY") {
			injected++
		}
	}
	if injected != 1 {
		t.Fatalf("skill injected into %d tool results, want exactly 1", injected)
	}
}

// The system_prompt option in config.json was never applied at startup. It is
// added to the built-in prompt as the user's own instructions rather than
// replacing it, which would drop the tool rules and the trust boundary.
func TestConfiguredInstructionsAreAddedToTheBuiltInPrompt(t *testing.T) {
	eng := newPatternEngine(t, &seqProvider{}, func(c *Config) { c.CustomInstructions = "提交信息一律用英文写。" })
	sp := eng.SystemPrompt()
	if !strings.Contains(sp, "提交信息一律用英文写。") {
		t.Fatal("configured instructions missing from the system prompt")
	}
	if !strings.Contains(sp, "Trust Boundary") {
		t.Fatal("configured instructions replaced the built-in prompt")
	}
}

func turnOutputLines(t *testing.T, debug bool) []string {
	t.Helper()
	read := &mockTool{name: "read", readOnly: true, safe: true, result: "x"}
	prov := &seqProvider{reply: func(ctx context.Context, n int, req api.ChatRequest) (*api.ChatResponse, error) {
		if n == 0 {
			return toolCallResp("r1", "read", map[string]any{"filePath": "a.go"}), nil
		}
		return &api.ChatResponse{Content: "done"}, nil
	}}
	eng := newPatternEngine(t, prov, func(c *Config) { c.Debug = debug }, read)
	var mu sync.Mutex
	var lines []string
	eng.OnEngineOutput = func(line string) {
		mu.Lock()
		lines = append(lines, line)
		mu.Unlock()
	}
	if _, err := run(t, eng, "read a.go"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), lines...)
}

// Bookkeeping lines ("session: +1 tool", learned memories) were printed into
// the conversation after every turn. They are diagnostics: debug mode only.
func TestTurnBookkeepingIsShownOnlyInDebugMode(t *testing.T) {
	for _, l := range turnOutputLines(t, false) {
		if strings.Contains(l, "session:") {
			t.Fatalf("bookkeeping printed outside debug mode: %q", l)
		}
	}
	var shown bool
	for _, l := range turnOutputLines(t, true) {
		if strings.Contains(l, "session:") {
			shown = true
		}
	}
	if !shown {
		t.Fatal("debug mode should still show the session summary")
	}
}

// Loop detection used stricter thresholds for any model named flash/mini/lite
// on the assumption that such models get stuck more; it interrupted them
// sooner for the same work.
func TestLoopDetectionIsTheSameForEveryTier(t *testing.T) {
	flash := newPatternEngine(t, &seqProvider{}, func(c *Config) { c.Model = "deepseek-flash" })
	pro := newPatternEngine(t, &seqProvider{}, func(c *Config) { c.Model = "deepseek-v4-pro" })
	if !reflect.DeepEqual(flash.loopDetector, pro.loopDetector) {
		t.Fatalf("loop detector differs by tier:\nflash=%+v\npro=%+v", flash.loopDetector, pro.loopDetector)
	}
}
