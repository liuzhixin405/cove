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
	storeDir string // ~/.cove/checkpoints/store (shared git repo)
	refName  string // refs/cove/<hash(workdir)>
	workDir  string
	count    int
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

// Create creates a new checkpoint. Returns the commit hash.
func (m *Manager) Create(label string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Use a temporary index file to avoid polluting any real git repo
	indexFile := filepath.Join(m.storeDir, "index-"+m.refName[len("refs/cove/"):])

	env := []string{
		"GIT_DIR=" + m.storeDir,
		"GIT_WORK_TREE=" + m.workDir,
		"GIT_INDEX_FILE=" + indexFile,
	}

	// Add all files. The exclusions live in the store's info/exclude (written by
	// writeStoreExcludes), so a plain --all already honors them.
	if err := m.gitCmd(env, "add", "--all"); err != nil {
		return "", fmt.Errorf("git add failed: %w", err)
	}

	// Commit
	if label == "" {
		label = fmt.Sprintf("checkpoint-%d", m.count+1)
	}
	msg := fmt.Sprintf("[cove] %s (%s)", label, time.Now().Format("15:04:05"))

	// (A commitArgs slice with a conditional "--amend" used to be built here and
	// then never passed to anything — the call below always used its own fixed
	// argument list. Removed rather than wired up: history is chained via
	// update-ref on m.refName, amending would rewrite the previous checkpoint.)
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

	// Files that a later checkpoint captured but the target one does not contain
	// have to be deleted, not just left in place. `checkout <hash> -- .` only
	// writes out what the commit contains, so a file created after the target
	// snapshot survived the rollback and the restore was never a true rollback.
	//
	// Scope note: only paths known to the checkpoint history are removed. A file
	// created since the LAST checkpoint appears in no commit at all, so it is
	// left alone — deleting it would mean running `git clean` over the user's
	// working tree on the strength of a snapshot that never saw it.
	tip := m.getRef([]string{"GIT_DIR=" + m.storeDir})
	var toDelete []string
	if tip != "" && tip != commitHash {
		// --diff-filter=A against (target → tip) = paths added after the target.
		out, err := m.gitOutput([]string{"GIT_DIR=" + m.storeDir},
			"diff", "--name-only", "--diff-filter=A", commitHash, tip)
		if err == nil {
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if p := strings.TrimSpace(line); p != "" {
					toDelete = append(toDelete, p)
				}
			}
		}
	}

	// Checkout the files from the commit
	if err := m.gitCmd(env, "checkout", commitHash, "--", "."); err != nil {
		return err
	}

	for _, rel := range toDelete {
		// Guard against a path escaping the working directory (a maliciously
		// crafted commit, or a stray absolute path in the diff output).
		full := filepath.Join(m.workDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(full, m.workDir+string(os.PathSeparator)) {
			continue
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("restore: removing %s: %w", rel, err)
		}
	}
	return nil
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
	// count is incremented under m.mu by Create, which runs on background
	// turn-end goroutines while the UI reads this for its status line.
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.count
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
