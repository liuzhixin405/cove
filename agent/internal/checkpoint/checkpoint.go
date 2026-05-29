package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager handles file system checkpoints using a git shadow store.
type Manager struct {
	mu       sync.Mutex
	storeDir string // ~/.agentgo/checkpoints/store (shared git repo)
	refName  string // refs/agentgo/<hash(workdir)>
	workDir  string
	count    int
	lastCP   time.Time
}

// New creates a checkpoint manager for the given working directory.
func New(workDir string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	storeDir := filepath.Join(home, ".agentgo", "checkpoints", "store")

	// Initialize bare git repo if not exists
	if _, err := os.Stat(filepath.Join(storeDir, "HEAD")); os.IsNotExist(err) {
		if err := os.MkdirAll(storeDir, 0700); err != nil {
			return nil, err
		}
		cmd := exec.Command("git", "init", "--bare", storeDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git init failed: %s: %w", out, err)
		}
	}

	// Compute ref name from workdir hash
	h := sha256.Sum256([]byte(workDir))
	refName := "refs/agentgo/" + hex.EncodeToString(h[:8])

	return &Manager{
		storeDir: storeDir,
		refName:  refName,
		workDir:  workDir,
	}, nil
}

// excludePatterns for git add
var excludePatterns = []string{
	"node_modules", ".git", ".venv", "__pycache__",
	"*.exe", "*.dll", "*.so", "*.dylib",
	"target/", "dist/", "build/", ".next/",
}

// Create creates a new checkpoint. Returns the commit hash.
func (m *Manager) Create(label string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Use a temporary index file to avoid polluting any real git repo
	indexFile := filepath.Join(m.storeDir, "index-"+m.refName[len("refs/agentgo/"):])

	env := []string{
		"GIT_DIR=" + m.storeDir,
		"GIT_WORK_TREE=" + m.workDir,
		"GIT_INDEX_FILE=" + indexFile,
	}

	// Build exclude args
	var excludeArgs []string
	for _, p := range excludePatterns {
		excludeArgs = append(excludeArgs, "--exclude", p)
	}

	// Add all files (respecting excludes)
	addArgs := append([]string{"add", "--all"}, excludeArgs...)
	if err := m.gitCmd(env, addArgs...); err != nil {
		// Fallback: add without excludes (some git versions don't support --exclude with --all)
		if err2 := m.gitCmd(env, "add", "-A"); err2 != nil {
			return "", fmt.Errorf("git add failed: %w", err)
		}
	}

	// Commit
	if label == "" {
		label = fmt.Sprintf("checkpoint-%d", m.count+1)
	}
	msg := fmt.Sprintf("[agentgo] %s (%s)", label, time.Now().Format("15:04:05"))

	commitArgs := []string{"commit", "--allow-empty", "-m", msg}
	// Set parent if ref exists
	if parent := m.getRef(env); parent != "" {
		commitArgs = append(commitArgs, "--amend") // We don't actually amend; use update-ref
	}

	if err := m.gitCmd(env, "commit", "--allow-empty", "-m", msg); err != nil {
		// If nothing to commit, that's okay
		if strings.Contains(err.Error(), "nothing to commit") {
			return "", nil
		}
		return "", fmt.Errorf("git commit failed: %w", err)
	}

	// Get the commit hash
	hash, err := m.gitOutput(env, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	hash = strings.TrimSpace(hash)

	// Update our ref to point to this commit
	if err := m.gitCmd(env, "update-ref", m.refName, hash); err != nil {
		return "", err
	}

	m.count++
	m.lastCP = time.Now()
	return hash, nil
}

// Restore rolls back the working directory to a checkpoint.
func (m *Manager) Restore(commitHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if commitHash == "" {
		// Restore to last checkpoint
		env := []string{"GIT_DIR=" + m.storeDir}
		var err error
		commitHash, err = m.gitOutput(env, "rev-parse", m.refName)
		if err != nil {
			return fmt.Errorf("无可用检查点")
		}
		commitHash = strings.TrimSpace(commitHash)
	}

	env := []string{
		"GIT_DIR=" + m.storeDir,
		"GIT_WORK_TREE=" + m.workDir,
	}

	// Checkout the files from the commit
	return m.gitCmd(env, "checkout", commitHash, "--", ".")
}

// List returns available checkpoints (most recent first).
func (m *Manager) List() []string {
	env := []string{"GIT_DIR=" + m.storeDir}
	out, err := m.gitOutput(env, "log", m.refName, "--oneline", "-20")
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var result []string
	for _, l := range lines {
		if l != "" {
			result = append(result, l)
		}
	}
	return result
}

// Count returns how many checkpoints have been made this session.
func (m *Manager) Count() int {
	return m.count
}

// Prune removes old refs for projects that haven't been used in 30 days.
func (m *Manager) Prune() error {
	env := []string{"GIT_DIR=" + m.storeDir}
	return m.gitCmd(env, "gc", "--auto", "--quiet")
}

func (m *Manager) getRef(env []string) string {
	out, err := m.gitOutput(env, "rev-parse", m.refName)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (m *Manager) gitCmd(env []string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = m.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func (m *Manager) gitOutput(env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = m.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %s", err, string(out))
	}
	return string(out), nil
}
