package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/render"
)

func TestCompleteAtPath(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"internal/engine/turn.go", "internal/repl/x.go", "README.md", ".hidden"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	got, ok := completeAtPath("看看 @int")
	if !ok || strings.Join(got, "|") != "看看 @internal/" {
		t.Fatalf("got %q, %v", got, ok)
	}
	got, _ = completeAtPath("看看 @internal/")
	if strings.Join(got, "|") != "看看 @internal/engine/|看看 @internal/repl/" {
		t.Fatalf("dir listing = %q", got)
	}
	if got, _ := completeAtPath("@"); strings.Contains(strings.Join(got, "|"), ".hidden") {
		t.Fatalf("dotfile offered: %q", got)
	}
	if _, ok := completeAtPath("/help"); ok {
		t.Fatal("a command line was taken as a path")
	}
}

func TestExpandCommand(t *testing.T) {
	sessionBlocks = &blockStore{}
	if got := expandCommand(nil); !strings.Contains(got, "还没有") {
		t.Fatalf("empty store: %q", got)
	}
	sessionBlocks.add(render.ToolBlock("41", "bash", "ls", "", "a\nb\nc", false, 0))
	sessionBlocks.add(render.ToolBlock("42", "grep", "x", "", "hit1\nhit2", false, 0))
	if got := expandCommand(nil); !strings.Contains(got, "hit2") {
		t.Fatalf("/x should expand the latest block: %q", got)
	}
	if got := expandCommand([]string{"#41"}); !strings.Contains(got, "c") || strings.Contains(got, "hit") {
		t.Fatalf("/x 41 = %q", got)
	}
	if got := expandCommand([]string{"99"}); !strings.Contains(got, "找不到 #99") {
		t.Fatalf("/x 99 = %q", got)
	}
}

func TestQuestionOptions(t *testing.T) {
	prompt := "[Q1] 选择\n怎么处理？\n  1. 保留: 不改\n  2. 删除: 去掉\n"
	opts, keys := questionOptions(prompt)
	if len(opts) != 2 || opts[1] != "2. 删除: 去掉" || keys != "12" {
		t.Fatalf("options %q keys %q", opts, keys)
	}
}

func TestTurnUsageFormatting(t *testing.T) {
	if humanTokens(950) != "950" || humanTokens(3200) != "3.2k" || humanTokens(1_500_000) != "1.5M" {
		t.Fatal("humanTokens")
	}
	if humanElapsed(65e9) != "1m05s" || humanElapsed(9e9) != "9s" {
		t.Fatal("humanElapsed")
	}
}
