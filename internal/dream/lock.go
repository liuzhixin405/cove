package dream

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/liuzhixin405/cove/internal/log"
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
	return filepath.Join(home, ".cove", "memory")
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
	_ = os.MkdirAll(filepath.Dir(path), 0700)

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
	_ = os.MkdirAll(filepath.Dir(path), 0700)
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

// isProcessRunning reports whether a process with the given PID is currently alive.
//
// The previous implementation was `proc.Signal(os.Signal(nil))`, which can only
// ever fail: os.Signal is an interface, so a nil interface value fails the
// type assertion inside Signal and an error is returned unconditionally. That
// made this function return false for EVERY pid — so a lock held by a live
// process was always judged stale and reclaimed, and the consolidation lock
// provided no mutual exclusion at all. Two instances could then write
// ~/.cove/memory concurrently and overwrite each other.
func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Windows, FindProcess opens the process handle and fails when the
		// process does not exist, so an error here means "not running".
		return false
	}
	if runtime.GOOS == "windows" {
		// FindProcess succeeded (a handle was opened) -> the process exists.
		return true
	}
	// On Unix, FindProcess always succeeds regardless of liveness; probe with
	// signal 0. A nil error means the process exists and is signalable; EPERM
	// means it exists but is owned by another user (still alive -> lock valid).
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
