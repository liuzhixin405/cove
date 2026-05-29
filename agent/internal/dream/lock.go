package dream

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentgo/internal/log"
)

const lockFileName = ".consolidate-lock"

// stale threshold: if the lock holder is older than this, reclaim it.
const holderStaleMs = 60 * 60 * 1000 // 1 hour

// lockPath returns the path to the consolidation lock file inside the memory dir.
func lockPath() string {
	return filepath.Join(memoryDir(), lockFileName)
}

// memoryDir returns the auto-dream memory directory (same as memory store root).
func memoryDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".agentgo", "memory")
}

// ReadLastConsolidatedAt returns the mtime of the lock file (= last consolidation time).
// Returns 0 if no lock file exists.
func ReadLastConsolidatedAt() (time.Time, error) {
	info, err := os.Stat(lockPath())
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// TryAcquireConsolidationLock attempts to acquire the lock.
// Returns (priorMtime, true) on success, or (zero, false) if blocked.
func TryAcquireConsolidationLock() (time.Time, bool, error) {
	path := lockPath()

	var mtimeMs int64
	var holderPid int
	hasPrior := false

	info, err := os.Stat(path)
	if err == nil {
		hasPrior = true
		mtimeMs = info.ModTime().UnixMilli()

		data, _ := os.ReadFile(path)
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			holderPid = pid
		}
	}

	// If lock exists and is recent, check if holder is alive
	if hasPrior && (time.Now().UnixMilli()-mtimeMs) < int64(holderStaleMs) {
		if holderPid > 0 && isProcessRunning(holderPid) {
			log.Debugf("[autoDream] lock held by live PID %d (mtime %ds ago)",
				holderPid, (time.Now().UnixMilli()-mtimeMs)/1000)
			return time.Time{}, false, nil
		}
		// Dead PID or unparseable — reclaim
	}

	// Ensure memory dir exists
	os.MkdirAll(filepath.Dir(path), 0700)

	// Write our PID
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid())), 0644); err != nil {
		return time.Time{}, false, err
	}

	// Verify we won the race
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, false, err
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	if pid != os.Getpid() {
		return time.Time{}, false, nil
	}

	var priorMtime time.Time
	if hasPrior {
		priorMtime = time.UnixMilli(mtimeMs)
	}
	return priorMtime, true, nil
}

// RollbackConsolidationLock rewinds the lock mtime to the prior value (or removes it if zero).
func RollbackConsolidationLock(priorMtime time.Time) error {
	path := lockPath()
	if priorMtime.IsZero() {
		return os.Remove(path)
	}
	// Clear PID body
	if err := os.WriteFile(path, []byte(""), 0644); err != nil {
		return err
	}
	return os.Chtimes(path, priorMtime, priorMtime)
}

// RecordConsolidation stamps the lock file (used by manual dream trigger).
func RecordConsolidation() error {
	path := lockPath()
	os.MkdirAll(filepath.Dir(path), 0700)
	return os.WriteFile(path, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

// ListSessionsTouchedSince returns session IDs with mtime after the given time.
func ListSessionsTouchedSince(since time.Time, sessionsDir string) ([]string, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(since) {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return ids, nil
}

// isProcessRunning checks if a process with the given PID exists.
func isProcessRunning(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds; send signal 0 to check.
	// On Windows, FindProcess fails for non-existent processes.
	// Use a cross-platform approach: try to signal.
	err = proc.Signal(os.Signal(nil))
	return err == nil
}
