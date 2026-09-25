package hooks

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
)

func writeHooksJSON(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".cove")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A BeforeTool hook configured in ~/.cove/hooks.json runs its command when a
// bash tool call fires the event. The command writes a file, so the test sees
// that it really ran through the user's shell.
func TestUserConfigHookRunsConfiguredCommand(t *testing.T) {
	home := t.TempDir()
	marker := filepath.ToSlash(filepath.Join(t.TempDir(), "hook-marker.txt"))
	writeHooksJSON(t, home, `{"hooks":{"BeforeTool":[{"matcher":"bash","command":"echo hooked > \"`+marker+`\""}]}}`)

	defs, err := LoadUserConfig(home)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if len(defs) != 1 || defs[0].Event != BeforeTool || defs[0].Matcher != "bash" {
		t.Fatalf("defs = %+v, want one BeforeTool/bash hook", defs)
	}

	m := NewManager()
	m.RegisterDefs(defs)
	out := m.Fire(context.Background(), BeforeTool, "bash", HookInput{Event: BeforeTool, ToolName: "bash"})
	if !out.Continue {
		t.Fatalf("a hook that prints nothing blocked the tool: %+v", out)
	}
	data, err := os.ReadFile(filepath.FromSlash(marker))
	if err != nil {
		t.Fatalf("configured hook command did not run (no marker file): %v", err)
	}
	// PowerShell 5.1 redirects as UTF-16; compare without the NULs.
	if !strings.Contains(strings.ReplaceAll(string(data), "\x00", ""), "hooked") {
		t.Errorf("marker file = %q, want it to contain \"hooked\"", data)
	}

	// The matcher still filters: another tool does not run it.
	_ = os.Remove(filepath.FromSlash(marker))
	m.Fire(context.Background(), BeforeTool, "read", HookInput{Event: BeforeTool, ToolName: "read"})
	if _, err := os.Stat(filepath.FromSlash(marker)); err == nil {
		t.Error("the bash-only hook ran for the read tool")
	}
}

func TestLoadUserConfigMissingFileIsNotAnError(t *testing.T) {
	defs, err := LoadUserConfig(t.TempDir())
	if err != nil || len(defs) != 0 {
		t.Fatalf("LoadUserConfig without hooks.json = %v, %v; want nothing and no error", defs, err)
	}
}

func TestLoadUserConfigAcceptsAliasesAndDefaults(t *testing.T) {
	home := t.TempDir()
	// A UTF-8 BOM is what PowerShell 5.1's Set-Content -Encoding utf8 writes.
	writeHooksJSON(t, home, "\xef\xbb\xbf"+`{"hooks":{
		"PreToolUse":[{"command":"a"}],
		"PostToolUse":[{"matcher":"^write$","command":"b","async":true,"timeout":5}],
		"SessionStart":[{"command":"c"}]
	}}`)
	defs, err := LoadUserConfig(home)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	got := map[HookEvent]HookDef{}
	for _, d := range defs {
		got[d.Event] = d
	}
	if len(got) != 3 || got[BeforeTool].Command != "a" || got[AfterTool].Command != "b" || got[SessionStart].Command != "c" {
		t.Fatalf("defs = %+v", defs)
	}
	after := got[AfterTool].Config()
	if after.Sequential || after.Timeout != 5*time.Second || after.Type != HookCommand {
		t.Errorf("async hook config = %+v", after)
	}
	before := got[BeforeTool].Config()
	if !before.Sequential || before.Timeout != defaultConfigHookTimeout {
		t.Errorf("default hook config = %+v, want sequential with the default timeout", before)
	}
}

func TestLoadUserConfigReportsBadEntriesButKeepsGoodOnes(t *testing.T) {
	home := t.TempDir()
	writeHooksJSON(t, home, `{"hooks":{
		"BeforeTool":[{"matcher":"(","command":"x"},{"command":""},{"command":"ok"}],
		"NoSuchEvent":[{"command":"y"}]
	}}`)
	defs, err := LoadUserConfig(home)
	if err == nil {
		t.Fatal("bad entries were accepted silently")
	}
	for _, want := range []string{"NoSuchEvent", "matcher", "command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(defs) != 1 || defs[0].Command != "ok" {
		t.Fatalf("defs = %+v, want only the valid hook", defs)
	}
}

func TestLoadUserConfigRejectsMalformedJSON(t *testing.T) {
	home := t.TempDir()
	writeHooksJSON(t, home, `{"hooks":`)
	if _, err := LoadUserConfig(home); err == nil || !strings.Contains(err.Error(), "hooks.json") {
		t.Fatalf("err = %v, want a parse error naming hooks.json", err)
	}
}

// A configured matcher names tools, so it matches the whole tool name: "bash"
// used to match "bash_output" too. A user who writes ^ or $ gets the regexp
// as written, and "*" means every tool.
func TestConfigMatcherIsAnchored(t *testing.T) {
	cases := []struct {
		matcher, tool string
		want          bool
	}{
		{"bash", "bash", true},
		{"bash", "bash_output", false},
		{"bash", "mybash", false},
		{"read|write", "write", true},
		{"read|write", "readme", false},
		{"^bash", "bash_output", true},
		{"output$", "bash_output", true},
		{"*", "anything", true},
		{"", "anything", true},
	}
	m := NewManager()
	for _, c := range cases {
		h := HookDef{Event: BeforeTool, Matcher: c.matcher, Command: "x"}.Config()
		if got := m.matches(h, c.tool); got != c.want {
			t.Errorf("matcher %q vs %q = %v, want %v (compiled %q)", c.matcher, c.tool, got, c.want, h.Matcher)
		}
	}
}

// SessionEnd is fired on exit now (cli/cove fireSessionEnd), so the loader no
// longer warns that such a hook will never run.
func TestLoadUserConfigAcceptsSessionEndSilently(t *testing.T) {
	var logBuf strings.Builder
	log.SetWriter(&logBuf)
	t.Cleanup(func() { log.SetWriter(io.Discard) })

	home := t.TempDir()
	writeHooksJSON(t, home, `{"hooks":{"SessionEnd":[{"command":"x"}]}}`)
	defs, err := LoadUserConfig(home)
	if err != nil || len(defs) != 1 {
		t.Fatalf("LoadUserConfig = %v, %v", defs, err)
	}
	if strings.Contains(logBuf.String(), "not fired") {
		t.Errorf("SessionEnd is still reported as never fired; log = %q", logBuf.String())
	}
	if defs[0].Event != SessionEnd {
		t.Errorf("event = %q, want SessionEnd", defs[0].Event)
	}
}
