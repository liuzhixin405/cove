package context

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/liuzhixin405/cove/internal/repomap"
)

type ProjectContext struct {
	mu        sync.RWMutex
	Cwd       string
	GitBranch string
	GitRoot   string
	GitStatus string
	GitLog    string
	GitMain   string // main/master branch name
	GitUser   string // git user.name
	FileTree  string
	RepoMap   string // AST-based lightweight global schema index
	Platform  string
	Shell     string
	IsGitRepo bool // 是否在 git 仓库内
}

func (c *ProjectContext) RefreshGit() {
	if c == nil || !c.IsGitRepo || c.GitRoot == "" {
		return
	}
	branch := gitBranch(c.GitRoot)
	status := gitStatus(c.GitRoot)

	c.mu.Lock()
	c.GitBranch = branch
	c.GitStatus = status
	c.mu.Unlock()
}

func (c *ProjectContext) GetGitInfo() (branch string, status string) {
	if c == nil {
		return "", ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.GitBranch, c.GitStatus
}

func Collect() *ProjectContext {
	c := &ProjectContext{
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Shell:    detectShell(),
	}
	c.Cwd, _ = os.Getwd()
	c.GitRoot = findGitRoot(c.Cwd)

	// Run git info collection, file tree, and RepoMap in parallel
	var wg sync.WaitGroup
	if c.GitRoot != "" {
		c.IsGitRepo = true
		wg.Add(5)
		go func() { defer wg.Done(); c.GitBranch = gitBranch(c.GitRoot) }()
		go func() { defer wg.Done(); c.GitStatus = gitStatus(c.GitRoot) }()
		go func() { defer wg.Done(); c.GitLog = gitLog(c.GitRoot) }()
		go func() { defer wg.Done(); c.GitMain = detectMainBranch(c.GitRoot) }()
		go func() { defer wg.Done(); c.GitUser = gitUser(c.GitRoot) }()
	}

	// File tree in parallel with git
	var treeResult string
	wg.Add(1)
	go func() { defer wg.Done(); treeResult = fileTree(c.Cwd, 3) }()

	// Repo Map generation in parallel
	var repoMapResult string
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanDir := c.GitRoot
		if scanDir == "" {
			scanDir = c.Cwd
		}
		gen := repomap.NewGenerator(scanDir)
		repoMapResult = gen.Generate(50) // Extract top 50 ranked files
	}()

	wg.Wait()
	c.FileTree = treeResult
	c.RepoMap = repoMapResult
	return c
}

func findGitRoot(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitBranch(root string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = root
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func gitStatus(root string) string {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = root
	out, _ := cmd.Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "(clean)"
	}
	lines := strings.Count(s, "\n") + 1
	if lines > 15 {
		return strings.Join(strings.Split(s, "\n")[:15], "\n") + "\n... " + strconv.Itoa(lines) + " files changed"
	}
	return s
}

func detectShell() string {
	if s := os.Getenv("SHELL"); s != "" {
		return s
	}
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "/bin/sh"
}

func gitLog(root string) string {
	cmd := exec.Command("git", "log", "--oneline", "--format=%h %an %s", "-5")
	cmd.Dir = root
	out, _ := cmd.Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return s
}

func detectMainBranch(root string) string {
	// Try common remote main branch names
	for _, name := range []string{"main", "master"} {
		cmd := exec.Command("git", "rev-parse", "--verify", "refs/heads/"+name)
		cmd.Dir = root
		if err := cmd.Run(); err == nil {
			return name
		}
	}
	// Fallback: check remote HEAD
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err == nil {
		ref := strings.TrimSpace(string(out))
		parts := strings.Split(ref, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1]
		}
	}
	return "main" // default assumption
}

func gitUser(root string) string {
	cmd := exec.Command("git", "config", "user.name")
	cmd.Dir = root
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func fileTree(root string, depth int) string {
	var sb strings.Builder
	walkDir(root, root, depth, 0, &sb)
	r := sb.String()
	if r == "" {
		return ""
	}
	lines := strings.Count(r, "\n") + 1
	if lines > 40 {
		return r[:strings.LastIndex(r[:min(len(r), 2000)], "\n")] + "\n... " + strconv.Itoa(lines) + " total entries"
	}
	return r
}

func walkDir(root, current string, maxDepth, currentDepth int, sb *strings.Builder) {
	if currentDepth > maxDepth {
		return
	}
	entries, err := os.ReadDir(current)
	if err != nil {
		return
	}
	prefix := strings.Repeat("  ", currentDepth)
	for _, e := range entries {
		if e.IsDir() && (e.Name() == ".git" || e.Name() == "node_modules" || strings.HasPrefix(e.Name(), ".")) {
			continue
		}
		rel, _ := filepath.Rel(root, filepath.Join(current, e.Name()))
		if e.IsDir() {
			sb.WriteString(prefix + rel + "/\n")
			walkDir(root, filepath.Join(current, e.Name()), maxDepth, currentDepth+1, sb)
		} else {
			sb.WriteString(prefix + rel + "\n")
		}
	}
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
