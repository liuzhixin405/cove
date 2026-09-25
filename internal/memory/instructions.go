package memory

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/liuzhixin405/cove/internal/textutil"
)

// MaxInstructionBytes caps the combined size of project instruction files
// (CLAUDE.md, AGENTS.md, ...) loaded into the prompt. Past it the remaining
// content is clipped and InstructionFilesTruncated reports true so the caller
// can tell the user.
const MaxInstructionBytes = 32 * 1024

// instructionFileNames are loaded in this order from every directory between
// the git root and the working directory.
var instructionFileNames = []string{
	"CLAUDE.md",
	filepath.Join(".claude", "CLAUDE.md"),
	"AGENTS.md",
	".cove.md",
}

// instructionDirs returns the directories from the git root (the nearest
// ancestor holding .git) down to cwd. Without a git root only cwd is used, so
// a stray AGENTS.md in the home directory is not picked up.
func instructionDirs(cwd string) []string {
	cwd = filepath.Clean(cwd)
	var chain []string
	for dir := cwd; ; {
		chain = append(chain, dir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			// Reverse: root first, most specific last.
			for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
				chain[i], chain[j] = chain[j], chain[i]
			}
			return chain
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return []string{cwd}
		}
		dir = parent
	}
}

func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// loadInstructionFiles appends the project instruction files for cwd to
// entries, deduplicated by path and by content, and reports whether the
// combined size had to be clipped to MaxInstructionBytes.
func loadInstructionFiles(cwd string, entries *[]Entry, seen map[string]bool) (truncated bool) {
	dirs := instructionDirs(cwd)
	base := dirs[0]
	contentSeen := map[[32]byte]bool{}
	total := 0
	for _, dir := range dirs {
		for _, name := range instructionFileNames {
			path := filepath.Join(dir, name)
			key := pathKey(path)
			if seen[key] {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			seen[key] = true
			sum := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
			if contentSeen[sum] {
				continue
			}
			contentSeen[sum] = true
			if truncated {
				continue
			}
			content := string(data)
			if total+len(content) > MaxInstructionBytes {
				remain := MaxInstructionBytes - total
				if remain < 0 {
					remain = 0
				}
				content = textutil.ClipBytes(content, remain, "\n... [truncated: instruction files exceed 32KB]")
				truncated = true
			}
			total += len(content)
			display := name
			if rel, err := filepath.Rel(base, path); err == nil {
				display = filepath.ToSlash(rel)
			}
			*entries = append(*entries, Entry{
				Name:    display,
				Path:    path,
				Content: content,
				Project: true,
				Source:  SourceInstructions,
			})
		}
	}
	return truncated
}

// InstructionFilesTruncated reports whether the last load had to clip the
// project instruction files (see MaxInstructionBytes). The engine shows a
// one-line notice when it is true.
func (s *Store) InstructionFilesTruncated() bool {
	s.All()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.instrTruncated
}

// ProjectRoot is the git root above cwd (the nearest ancestor holding .git),
// or cwd itself outside a repository. Per-project data is keyed by it, so
// every subdirectory of a repository shares one memory directory.
func ProjectRoot(cwd string) string {
	return instructionDirs(cwd)[0]
}

// NewProjectStore is NewStoreForProject over <projectDataDir>/memory and the
// global ~/.cove/memory (projectDataDir is config.ProjectDataDir's result).
func NewProjectStore(projectDataDir string) *Store {
	home, _ := os.UserHomeDir()
	return NewStoreForProject(filepath.Join(projectDataDir, "memory"), filepath.Join(home, ".cove", "memory"))
}
