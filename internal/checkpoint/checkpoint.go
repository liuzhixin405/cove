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

	"github.com/liuzhixin405/cove/internal/log"
)

// Manager handles file system checkpoints using a git shadow store.
type Manager struct {
	mu       sync.Mutex
	storeDir string // ~/.cove/checkpoints/store (shared git repo)
	refName  string // refs/cove/<hash(workdir)>
	workDir  string
	count    int
	// created counts checkpoints created by this manager, for the periodic
	// trim and gc; the rest is the background maintenance state (all under mu
	// except maintWG).
	created     int
	pendingTrim bool
	pendingGC   bool
	maintaining bool
	maintWG     sync.WaitGroup
}

// New creates a checkpoint manager for the given working directory.
func New(workDir string) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	storeDir := filepath.Join(home, ".cove", "checkpoints", "store")

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

	// Install the exclude rules into the shadow store. See excludePatterns.
	if err := writeStoreExcludes(storeDir); err != nil {
		return nil, err
	}

	// Compute ref name from workdir hash
	h := sha256.Sum256([]byte(workDir))
	refName := "refs/cove/" + hex.EncodeToString(h[:8])

	return &Manager{
		storeDir: storeDir,
		refName:  refName,
		workDir:  workDir,
	}, nil
}

// excludePatterns lists what a checkpoint must never capture.
//
// These are applied through the shadow store's own info/exclude file, not on
// the `git add` command line. `git add --all --exclude=<p>` is not valid git:
// --exclude belongs to `git ls-files`, so every Create failed outright and silently
// fell back to the unfiltered `git add -A` in the error branch — which is why
// node_modules/ and target/ ended up inside the checkpoints all along.
var excludePatterns = []string{
	"node_modules", ".git", ".venv", "__pycache__",
	"*.exe", "*.dll", "*.so", "*.dylib",
	"target/", "dist/", "build/", ".next/",
}

// writeStoreExcludes renders excludePatterns into <storeDir>/info/exclude,
// which git applies to every add in this repository. The store is dedicated to
// checkpoints, so owning that file outright is safe.
func writeStoreExcludes(storeDir string) error {
	dir := filepath.Join(storeDir, "info")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("checkpoint store info dir: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("# Managed by cove — regenerated on startup.\n")
	for _, p := range excludePatterns {
		sb.WriteString(p)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "exclude"), []byte(sb.String()), 0600); err != nil {
		return fmt.Errorf("checkpoint store excludes: %w", err)
	}
	return nil
}

// Create snapshots the working tree and returns the checkpoint's commit hash.
// When nothing changed since the last checkpoint it returns that one instead
// of adding an identical entry.
func (m *Manager) Create(label string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if label == "" {
		label = fmt.Sprintf("checkpoint-%d", m.count+1)
	}
	hash, created, err := m.snapshot(m.refName, label)
	if err != nil {
		return "", err
	}
	if created {
		m.count++
		// Trim and gc run in the background (retention.go); a trim rewrites
		// the kept commits, so this hash may be replaced by then.
		m.scheduleMaintenanceLocked()
	}
	return hash, nil
}

// snapshot commits the current working tree onto ref and reports whether a new
// commit was needed.
//
// The commit is built with write-tree/commit-tree and chained on this
// project's own ref. `git commit` would have advanced the shared store's HEAD,
// so every project's checkpoint became the parent of the next one in any
// project: /checkpoints listed other projects' snapshots and /undo <hash>
// would check one of them out into this directory.
func (m *Manager) snapshot(ref, label string) (string, bool, error) {
	env := m.env()
	// The exclusions live in the store's info/exclude (written by
	// writeStoreExcludes), and git also honors the project's own .gitignore.
	if err := m.gitCmd(env, "add", "--all"); err != nil {
		return "", false, fmt.Errorf("git add failed: %w", err)
	}
	tree, err := m.gitOutput(env, "write-tree")
	if err != nil {
		return "", false, fmt.Errorf("git write-tree failed: %w", err)
	}
	tree = strings.TrimSpace(tree)

	parent := m.getRef(env, ref)
	if parent != "" && m.treeOf(parent) == tree {
		return parent, false, nil
	}

	msg := fmt.Sprintf("[cove] %s (%s)", label, time.Now().Format("15:04:05"))
	// The store is cove's own; commits need an identity even on a machine where
	// the user never configured one, or no checkpoint is ever created.
	args := []string{"-c", "user.name=cove", "-c", "user.email=cove@localhost", "commit-tree", tree, "-m", msg}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	hash, err := m.gitOutput(env, args...)
	if err != nil {
		return "", false, fmt.Errorf("git commit-tree failed: %w", err)
	}
	hash = strings.TrimSpace(hash)
	if err := m.gitCmd(env, "update-ref", ref, hash); err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// Restore rolls the working directory back to a checkpoint and returns the
// hash of a backup taken just before, so the rollback itself can be undone
// with Restore(backup).
//
// With an empty hash it goes to the newest checkpoint that differs from the
// current state, so repeated calls keep stepping back instead of restoring the
// same snapshot again.
func (m *Manager) Restore(commitHash string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	env := m.env()
	backupRef := m.refName + "-undo"
	backup, backedUp, err := m.snapshot(backupRef, "before undo")
	if err != nil {
		return "", fmt.Errorf("备份当前状态失败: %w", err)
	}
	if backedUp {
		// The undo-backup chain is kept to the same length as checkpoints.
		if trimmed, err := m.trimLocked(backupRef); err != nil {
			log.Warnf("[checkpoint] trim undo backups: %v", err)
		} else if trimmed != "" {
			backup = trimmed
		}
	}
	current := m.treeOf(backup)

	target := ""
	if commitHash == "" {
		out, err := m.gitOutput(env, "rev-list", m.refName)
		if err != nil {
			return "", fmt.Errorf("无可用检查点")
		}
		for _, h := range strings.Fields(out) {
			if m.treeOf(h) != current {
				target = h
				break
			}
		}
		if target == "" {
			return "", fmt.Errorf("没有与当前状态不同的检查点")
		}
	} else {
		full, err := m.gitOutput(env, "rev-parse", "--verify", "--quiet", commitHash+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("检查点 %s 不存在", commitHash)
		}
		target = strings.TrimSpace(full)
		if !m.isAncestor(target, m.refName) && !m.isAncestor(target, backupRef) {
			return "", fmt.Errorf("检查点 %s 不属于当前项目", commitHash)
		}
	}

	// Paths that exist now but not in the target have to be deleted, not left
	// in place: `checkout <hash> -- .` only writes out what the commit
	// contains. The backup holds every one of them, so this is reversible.
	out, err := m.gitOutput(env, "diff", "--name-only", "--no-renames", "--diff-filter=A", target, backup)
	if err != nil {
		return "", err
	}
	if err := m.gitCmd(env, "checkout", target, "--", "."); err != nil {
		return "", err
	}
	for _, rel := range strings.Split(strings.TrimSpace(out), "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		// Guard against a path escaping the working directory (a maliciously
		// crafted commit, or a stray absolute path in the diff output).
		full := filepath.Join(m.workDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(full, filepath.Clean(m.workDir)+string(os.PathSeparator)) {
			continue
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return backup, fmt.Errorf("restore: removing %s: %w", rel, err)
		}
	}
	return backup, nil
}

// List returns available checkpoints (most recent first).
func (m *Manager) List() []string {
	out, err := m.gitOutput(m.env(), "log", m.refName, "--oneline", "-20")
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
	// count is incremented under m.mu by Create, which runs on background
	// turn-end goroutines while the UI reads this for its status line.
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
}

// env points git at the shadow store, this project's work tree and this
// project's own index file, so no real repository and no other project is
// touched.
func (m *Manager) env() []string {
	return []string{
		"GIT_DIR=" + m.storeDir,
		"GIT_WORK_TREE=" + m.workDir,
		"GIT_INDEX_FILE=" + filepath.Join(m.storeDir, "index-"+m.refName[len("refs/cove/"):]),
	}
}

func (m *Manager) getRef(env []string, ref string) string {
	out, err := m.gitOutput(env, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (m *Manager) treeOf(commit string) string {
	out, err := m.gitOutput(m.env(), "rev-parse", commit+"^{tree}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (m *Manager) isAncestor(commit, ref string) bool {
	if m.getRef(m.env(), ref) == "" {
		return false
	}
	return m.gitCmd(m.env(), "merge-base", "--is-ancestor", commit, ref) == nil
}

func (m *Manager) gitCmd(env []string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = m.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func (m *Manager) gitOutput(env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = m.workDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, string(out))
	}
	return string(out), nil
}
