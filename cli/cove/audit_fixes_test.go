package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/command"
	"github.com/liuzhixin405/cove/internal/config"
	"github.com/liuzhixin405/cove/internal/engine"
	"github.com/liuzhixin405/cove/internal/state"
	"github.com/liuzhixin405/cove/internal/termui"
)

// An e-mail address or "user@host" is not an attachment. "@bar.com" used to
// be looked up as a file, and the whole prompt failed with 读取附件失败.
func TestBuildUserMessageIgnoresAtInsideWord(t *testing.T) {
	in := "把报告发给 foo@bar.com 并抄送 git@github.com"
	msg, _, err := buildUserMessage(in, t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("buildUserMessage: %v", err)
	}
	if msg.Content != in || len(msg.Parts) != 0 {
		t.Fatalf("content = %q parts = %d, want the prompt unchanged", msg.Content, len(msg.Parts))
	}
}

// Decorators and annotations (@Override, @dataclass) are everyday words in a
// coding prompt; when no such file exists they stay text instead of failing
// the prompt.
func TestBuildUserMessageKeepsBareAtWordWithoutFile(t *testing.T) {
	msg, _, err := buildUserMessage("为什么 @Override 报错", t.TempDir(), nil, "")
	if err != nil {
		t.Fatalf("buildUserMessage: %v", err)
	}
	if msg.Content != "为什么 @Override 报错" || len(msg.Parts) != 0 {
		t.Fatalf("content = %q parts = %d", msg.Content, len(msg.Parts))
	}
}

func TestBuildUserMessageStillAttachesBareNameThatExists(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"), []byte("all:\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, _, err := buildUserMessage("看看 @Makefile", dir, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("expected Makefile attached, got %d parts", len(msg.Parts))
	}
}

// The manual promises a switch to a vision model when an image is attached to
// a model that cannot see it, but the check looked for "fallback"/"vision" in
// a Chinese warning that contains neither, so it never fired.
func TestNonVisionImageWarningTriggersVisionSwitch(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "img.png")
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := buildUserMessage("看图", dir, []string{imgPath}, "deepseek-reasoner")
	if err != nil {
		t.Fatal(err)
	}
	if !shouldAutoSwitchToVision(warnings) {
		t.Fatalf("warnings %q did not trigger the vision switch", warnings)
	}
}

func TestTextAttachmentDoesNotTriggerVisionSwitch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, warnings, err := buildUserMessage("看 @a.txt", dir, nil, "deepseek-reasoner")
	if err != nil {
		t.Fatal(err)
	}
	if shouldAutoSwitchToVision(warnings) {
		t.Fatalf("a text attachment triggered the vision switch: %q", warnings)
	}
}

// newTestEngine builds a real engine against a HOME and working directory
// that belong to the test. It never sends a request.
func newTestEngine(t *testing.T) *engine.Engine {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("COVE_CONFIG_DIR", filepath.Join(home, ".cove"))
	seedCheckpointStore(t, home)
	origWD, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWD) })
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Config{
		Model:    "deepseek-v4-pro",
		Provider: api.ProviderConfig{Name: "deepseek", APIKey: "placeholder", BaseURL: "http://127.0.0.1:1"},
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	return eng
}

// /undo, /checkpoints and /ratelimit type-assert the engine they are handed;
// the adapter handleCommand passes did not forward those methods, so all
// three always answered "不可用" in the real program while their unit tests
// (using a fake engine) passed.
func TestEngineAdapterServesCheckpointAndRateLimitCommands(t *testing.T) {
	a := replEngineAdapter{eng: newTestEngine(t)}
	ctx := context.Background()
	for _, c := range []command.Command{command.NewCheckpointsCmd(), command.NewUndoCmd(), command.NewRateLimitCmd()} {
		out, err := c.Execute(ctx, command.Input{Engine: a})
		if err != nil {
			continue // a real error from the engine is fine; "unavailable" is not
		}
		if strings.Contains(out.Message, "不可用") {
			t.Errorf("/%s through the real adapter: %q", c.Name(), out.Message)
		}
	}
}

// "/config budget 7" must reach the engine's cost tracker, which is the one
// that stops a run over budget.
func TestEngineAdapterAppliesConfigBudgetLive(t *testing.T) {
	eng := newTestEngine(t)
	cfg := config.DefaultConfig()
	_, err := command.NewConfigCmd().Execute(context.Background(), command.Input{
		Args: []string{"budget", "7"}, Config: cfg, SaveConfig: func(*config.Config) error { return nil },
		Engine: replEngineAdapter{eng: eng}, AppState: &state.AppState{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(eng.CostTracker().Summary(), "7.00") {
		t.Fatalf("budget not applied to the engine: %s", eng.CostTracker().Summary())
	}
}

func captureOut(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	termui.SetWriter(&buf)
	t.Cleanup(func() { termui.SetWriter(nil) })
	return &buf
}

// "/budget abc" and "/budget -1" used to print nothing at all.
func TestBudgetCommandRejectsInvalidAmount(t *testing.T) {
	for _, in := range []string{"/budget abc", "/budget -1", "/budget 0"} {
		buf := captureOut(t)
		cfg := config.DefaultConfig()
		cfg.MaxBudgetUsd = 5
		handleBudgetCommand(in, cfg, nil, &state.AppState{})
		if !strings.Contains(buf.String(), "用法") {
			t.Errorf("%s: expected usage, got %q", in, buf.String())
		}
		if cfg.MaxBudgetUsd != 5 {
			t.Errorf("%s: budget changed to %v", in, cfg.MaxBudgetUsd)
		}
	}
}

// Bare "/budget" fell through to "未找到命令 /budget".
func TestBareBudgetShowsCurrentBudget(t *testing.T) {
	buf := captureOut(t)
	cfg := config.DefaultConfig()
	cfg.MaxBudgetUsd = 2.5
	if !handleBuiltinConfigCommand("/budget", cfg, nil, nil, &state.AppState{}) {
		t.Fatal("/budget was not handled")
	}
	if !strings.Contains(buf.String(), "2.50") {
		t.Fatalf("expected the current budget, got %q", buf.String())
	}
}

// `cat app.log | cove -p "解释"` ignored the log entirely.
func TestReadPipedStdinReturnsData(t *testing.T) {
	got, truncated, err := readPipedStdin(strings.NewReader("line1\nline2\n"), time.Second, 1024)
	if err != nil || truncated {
		t.Fatalf("err=%v truncated=%v", err, truncated)
	}
	if got != "line1\nline2\n" {
		t.Fatalf("got %q", got)
	}
}

// An inherited stdin that is open but never written (some IDEs, CI runners)
// must not hang a -p run.
func TestReadPipedStdinGivesUpWhenNothingArrives(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	start := time.Now()
	got, _, err := readPipedStdin(r, 50*time.Millisecond, 1024)
	if err != nil || got != "" {
		t.Fatalf("got %q err=%v", got, err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("readPipedStdin waited far past its timeout")
	}
}

func TestReadPipedStdinCapsSize(t *testing.T) {
	got, truncated, err := readPipedStdin(strings.NewReader(strings.Repeat("x", 100)), time.Second, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(got) != 10 {
		t.Fatalf("len=%d truncated=%v", len(got), truncated)
	}
}

func TestCombinePromptAndStdin(t *testing.T) {
	cases := []struct{ prompt, stdin, want string }{
		{"解释", "", "解释"},
		{"", "日志内容\n", "日志内容"},
		{"解释", "日志内容\n", "解释\n\n日志内容"},
	}
	for _, c := range cases {
		if got := combinePromptAndStdin(c.prompt, c.stdin); got != c.want {
			t.Errorf("combine(%q, %q) = %q, want %q", c.prompt, c.stdin, got, c.want)
		}
	}
}
