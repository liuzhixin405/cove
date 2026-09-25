package command

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHooksCmdListsLoadedHooks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("COVE_CONFIG_DIR", dir)
	out, err := NewHooksCmd().Execute(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Message, "没有") {
		t.Fatalf("empty case: %q", out.Message)
	}
	cfg := `{"hooks":{"BeforeTool":[{"matcher":"bash","command":"echo pre","timeout":5}],"SessionEnd":[{"command":"echo bye","async":true}],"Bogus":[{"command":"x"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err = NewHooksCmd().Execute(context.Background(), Input{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BeforeTool", "bash", "echo pre", "5s", "SessionEnd", "echo bye", "异步", "Bogus"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("missing %q in:\n%s", want, out.Message)
		}
	}
}
