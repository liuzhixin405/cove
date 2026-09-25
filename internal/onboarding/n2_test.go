package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckDetectsAgentsMD(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "# rules\n")
	s := Check(dir)
	if !s.HasAgentsMD {
		t.Fatal("AGENTS.md not detected")
	}
	if strings.Contains(s.Summary(), "no CLAUDE.md") {
		t.Fatalf("summary still says CLAUDE.md is missing: %q", s.Summary())
	}
	if !strings.Contains(s.Summary(), "AGENTS.md") {
		t.Fatalf("summary should mention AGENTS.md: %q", s.Summary())
	}
}

func TestWriteGuideCreatesExclusively(t *testing.T) {
	dir := t.TempDir()
	s := Check(dir)
	path, err := s.WriteGuide("# generated\n")
	if err != nil || path != filepath.Join(dir, "CLAUDE.md") {
		t.Fatalf("WriteGuide = %q, %v", path, err)
	}
	if path, err := s.WriteGuide("# other\n"); err != nil || path != "" {
		t.Fatalf("second WriteGuide = %q, %v; want no-op", path, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if string(data) != "# generated\n" {
		t.Fatalf("content = %q", data)
	}
}

func TestInitPromptCarriesRepoDigest(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/demo\n\ngo 1.25\n")
	writeFile(t, filepath.Join(dir, "README.md"), "# Demo\nA demo service.\n")
	writeFile(t, filepath.Join(dir, "AGENTS.md"), "Always run gofmt.\n")
	writeFile(t, filepath.Join(dir, "cmd", "demo", "main.go"), "package main\n")
	writeFile(t, filepath.Join(dir, "node_modules", "x", "y.js"), "junk\n")
	p := Check(dir).InitPrompt()
	for _, want := range []string{"module example.com/demo", "A demo service.", "Always run gofmt.", "cmd/", "CLAUDE.md"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(p, "node_modules") {
		t.Error("prompt lists node_modules")
	}
}

func TestGuideDiff(t *testing.T) {
	d := GuideDiff("", "a\nb\n")
	if !strings.Contains(d, "+ a") || !strings.Contains(d, "+ b") {
		t.Fatalf("new-file diff: %q", d)
	}
}
