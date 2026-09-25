package onboarding

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// InitSystemPrompt is the system prompt for the one-shot model call that
// drafts CLAUDE.md in /init.
const InitSystemPrompt = `You write CLAUDE.md, the instruction file a coding agent reads at the start of every session in this repository.
Write concise Markdown (at most about 80 lines): what the project is, how to build, test and lint it (exact commands), the layout of the important directories, and conventions a contributor must follow.
Only state what the repository material shows; do not invent commands or tools. Do not repeat what an existing AGENTS.md already says; refer to it instead.
Reply with the file content only, no preamble and no code fence around the whole file.`

// manifestFiles are read (head only) into the /init prompt.
var manifestFiles = []string{
	"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "requirements.txt",
	"pom.xml", "build.gradle", "Makefile", "Gemfile", "mix.exs", ".golangci.yml",
}

// skipDirs are never listed in the repository digest.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, ".venv": true, "__pycache__": true, ".next": true, ".idea": true, ".vscode": true,
}

// InitPrompt is the user prompt for drafting CLAUDE.md: the detected facts
// plus a digest of the repository (top two directory levels, README and
// manifest heads, an existing AGENTS.md).
func (s *State) InitPrompt() string {
	var sb strings.Builder
	sb.WriteString("Draft CLAUDE.md for this repository.\n\n")
	fmt.Fprintf(&sb, "Detected: language %s", s.Language)
	if s.HasGit {
		sb.WriteString(", git repository")
	}
	sb.WriteString("\n\n## Directory layout (two levels)\n")
	sb.WriteString(dirDigest(s.ProjectDir))
	for _, name := range append([]string{"README.md"}, manifestFiles...) {
		if head := fileHead(filepath.Join(s.ProjectDir, name), 3000); head != "" {
			fmt.Fprintf(&sb, "\n## %s\n%s\n", name, head)
		}
	}
	if s.HasAgentsMD {
		if head := fileHead(filepath.Join(s.ProjectDir, "AGENTS.md"), 6000); head != "" {
			fmt.Fprintf(&sb, "\n## Existing AGENTS.md (also loaded by the agent; do not repeat it)\n%s\n", head)
		}
	}
	return sb.String()
}

func fileHead(path string, limit int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return textutil.ClipBytes(strings.TrimSpace(string(data)), limit, "\n...")
}

func dirDigest(root string) string {
	var sb strings.Builder
	entries, err := os.ReadDir(root)
	if err != nil {
		return "(unreadable)\n"
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	shown := 0
	for _, e := range entries {
		if shown >= 60 {
			sb.WriteString("...\n")
			break
		}
		name := e.Name()
		if e.IsDir() {
			if skipDirs[name] {
				continue
			}
			fmt.Fprintf(&sb, "%s/\n", name)
			shown++
			sub, _ := os.ReadDir(filepath.Join(root, name))
			n := 0
			for _, c := range sub {
				if skipDirs[c.Name()] {
					continue
				}
				if n >= 15 {
					sb.WriteString("  ...\n")
					break
				}
				suffix := ""
				if c.IsDir() {
					suffix = "/"
				}
				fmt.Fprintf(&sb, "  %s%s\n", c.Name(), suffix)
				n++
			}
			continue
		}
		fmt.Fprintf(&sb, "%s\n", name)
		shown++
	}
	return sb.String()
}

// GuideDiff renders the change from existing to draft as a line diff for the
// user to review ("+ " added, "- " removed, "  " kept). A new file is all "+".
func GuideDiff(existing, draft string) string {
	a := splitLines(existing)
	b := splitLines(draft)
	// LCS table; guides are small (hundreds of lines at most).
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var sb strings.Builder
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			sb.WriteString("  " + a[i] + "\n")
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			sb.WriteString("+ " + b[j] + "\n")
			j++
		default:
			sb.WriteString("- " + a[i] + "\n")
			i++
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	s = strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
