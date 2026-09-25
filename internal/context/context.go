package context

import (
	stdctx "context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove/internal/repomap"
	"github.com/liuzhixin405/cove/internal/shell"
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

	// structCh is closed once Structure's background build is done;
	// structDone is then set. Both guarded by mu.
	structCh   chan struct{}
	structDone bool
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

// RefreshGitAll re-reads the per-turn git state — branch, status and recent
// commits — in parallel. It is what a turn needs for its <environment> note;
// unlike Collect it does not walk the file tree or rebuild the repo map.
//
// A directory that was not a repository when cove started is probed again
// (cheaply: git only runs once a .git entry exists above Cwd), so "git init"
// during a session gives the next turn its git state instead of none until
// restart.
func (c *ProjectContext) RefreshGitAll() {
	if c == nil {
		return
	}
	if c.GitRoot == "" {
		c.detectNewRepo()
	}
	if !c.IsGitRepo || c.GitRoot == "" {
		return
	}
	var branch, status, log string
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); branch = gitBranch(c.GitRoot) }()
	go func() { defer wg.Done(); status = gitStatus(c.GitRoot) }()
	go func() { defer wg.Done(); log = gitLog(c.GitRoot) }()
	wg.Wait()

	c.mu.Lock()
	c.GitBranch = branch
	c.GitStatus = status
	c.GitLog = log
	c.mu.Unlock()
}

// detectNewRepo looks for a repository that appeared around Cwd after
// Collect ran, and fills in the facts Collect would have (root, main branch,
// user).
func (c *ProjectContext) detectNewRepo() {
	if c.Cwd == "" || !hasGitMarkerAbove(c.Cwd) {
		return
	}
	root := findGitRoot(c.Cwd)
	if root == "" {
		return
	}
	main, user := detectMainBranch(root), gitUser(root)
	c.mu.Lock()
	c.GitRoot, c.IsGitRepo = root, true
	c.GitMain, c.GitUser = main, user
	c.mu.Unlock()
}

// hasGitMarkerAbove reports whether dir or one of its parents has a .git
// entry (a directory, or the file a worktree or submodule uses).
func hasGitMarkerAbove(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
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
		Shell:    shell.Default().Describe(),
	}
	c.Cwd, _ = os.Getwd()
	c.GitRoot = findGitRoot(c.Cwd)

	// Collect the git facts in parallel.
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

	// The file tree and repo map are no longer built here: the system prompt
	// carries a project outline instead, and walking and parsing the whole
	// repository held up startup. /context builds them on demand (Structure).
	wg.Wait()
	return c
}

// Builders behind Structure (swapped in tests).
var (
	buildFileTree = func(root string) string { return fileTree(root, 3) }
	buildRepoMap  = func(root string) string { return repomap.NewGenerator(root).Generate(50) }
)

// Structure returns the project file tree and the top-50 repo map, building
// them on first use in the background and caching them. It waits at most
// timeout; ok is false when they are still being built (a later call returns
// them once done). FileTree/RepoMap set by the caller count as built.
func (c *ProjectContext) Structure(timeout time.Duration) (tree, repoMap string, ok bool) {
	c.mu.Lock()
	if c.structDone || c.FileTree != "" || c.RepoMap != "" {
		tree, repoMap = c.FileTree, c.RepoMap
		c.mu.Unlock()
		return tree, repoMap, true
	}
	if c.structCh == nil {
		ch := make(chan struct{})
		c.structCh = ch
		cwd, scanDir := c.Cwd, c.GitRoot
		if scanDir == "" {
			scanDir = cwd
		}
		go func() {
			t, m := buildFileTree(cwd), buildRepoMap(scanDir)
			c.mu.Lock()
			c.FileTree, c.RepoMap, c.structDone = t, m, true
			c.mu.Unlock()
			close(ch)
		}()
	}
	ch := c.structCh
	c.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		c.mu.RLock()
		defer c.mu.RUnlock()
		return c.FileTree, c.RepoMap, true
	case <-timer.C:
		return "", "", false
	}
}

// gitTimeout bounds each git call made while collecting project context.
// Collect runs at startup and waits for all of them; with no bound, a git that
// never returned (status on a huge repo, a network filesystem, a stuck
// fsmonitor) kept cove from starting.
var gitTimeout = 5 * time.Second

// gitCommand builds a bounded git invocation in dir. The caller must call the
// returned cancel func.
//
// GIT_OPTIONAL_LOCKS=0: "git status" otherwise refreshes the index under
// .git/index.lock, so running it in the background made the user's own commit
// (or the model's bash call) fail with "index.lock: File exists".
// core.quotePath=false: git otherwise prints non-ASCII paths as octal escapes,
// and the prompt showed "ä¸­æ.go" for 中文.go.
func gitCommand(dir string, args ...string) (*exec.Cmd, stdctx.CancelFunc) {
	ctx, cancel := stdctx.WithTimeout(stdctx.Background(), gitTimeout)
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.quotePath=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(shell.Env(os.Environ()), "GIT_OPTIONAL_LOCKS=0")
	// A killed git can leave a child holding stdout open; don't wait on it.
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func findGitRoot(cwd string) string {
	cmd, cancel := gitCommand(cwd, "rev-parse", "--show-toplevel")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitBranch(root string) string {
	cmd, cancel := gitCommand(root, "rev-parse", "--abbrev-ref", "HEAD")
	defer cancel()
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func gitStatus(root string) string {
	cmd, cancel := gitCommand(root, "status", "--porcelain")
	defer cancel()
	out, err := cmd.Output()
	if err != nil {
		// Used to fall through to "(clean)": a failed or timed-out status
		// printed nothing, and the model was told a dirty tree was clean.
		return "(unknown: git status failed or timed out)"
	}
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

func gitLog(root string) string {
	cmd, cancel := gitCommand(root, "log", "--oneline", "--format=%h %an %s", "-5")
	defer cancel()
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
		cmd, cancel := gitCommand(root, "rev-parse", "--verify", "refs/heads/"+name)
		defer cancel()
		if err := cmd.Run(); err == nil {
			return name
		}
	}
	// Fallback: check remote HEAD
	cmd, cancel := gitCommand(root, "symbolic-ref", "refs/remotes/origin/HEAD")
	defer cancel()
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
	cmd, cancel := gitCommand(root, "config", "user.name")
	defer cancel()
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
