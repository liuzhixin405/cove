package context

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type ProjectContext struct {
	Cwd       string
	GitBranch string
	GitRoot   string
	GitStatus string
	GitLog    string
	FileTree  string
	Platform  string
	Shell     string
	IsGitRepo bool
}

func Collect() *ProjectContext {
	c := &ProjectContext{
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Shell:    detectShell(),
	}
	c.Cwd, _ = os.Getwd()
	c.GitRoot = findGitRoot(c.Cwd)
	if c.GitRoot != "" {
		c.IsGitRepo = true
		c.GitBranch = gitBranch(c.GitRoot)
		c.GitStatus = gitStatus(c.GitRoot)
		c.GitLog = gitLog(c.GitRoot)
	}
	c.FileTree = fileTree(c.Cwd, 3)
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
		return strings.Join(strings.Split(s, "\n")[:15], "\n") + "\n... " + itoa(lines) + " files changed"
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
	cmd := exec.Command("git", "log", "--oneline", "-10")
	cmd.Dir = root
	out, _ := cmd.Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return s
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
		return r[:strings.LastIndex(r[:min(len(r), 2000)], "\n")] + "\n... " + itoa(lines) + " total entries"
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
func min(a, b int) int { if a < b { return a }; return b }

func itoa(n int) string {
	if n == 0 { return "0" }
	d := []byte{}
	for n > 0 { d = append([]byte{byte('0'+n%10)}, d...); n /= 10 }
	return string(d)
}
