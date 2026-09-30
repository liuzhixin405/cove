package dream

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/fsatomic"
	"github.com/liuzhixin405/cove-agent/internal/log"
	"github.com/liuzhixin405/cove-agent/internal/session"
)

const lockFileName = ".consolidate-lock"

// takeoverSuffix names the guard file (lock path + suffix) a worker creates
// with O_EXCL while it takes over an existing, stale lock.
const takeoverSuffix = ".takeover"

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
//
// Acquiring used to be stat -> read -> os.WriteFile (truncate + write) ->
// read back, which is not atomic: two workers exiting together both saw a
// dead holder, both wrote, and each read back its own PID (in one process,
// the same PID), so both ran; and a reader hitting the file between the
// truncate and the write parsed an empty PID as a dead holder and stole it.
// Now a missing lock is created with O_CREATE|O_EXCL and the PID written in
// that same open; an existing stale lock is taken over only under an
// O_EXCL takeover guard, after re-checking it is still the lock that was
// judged stale, and replaced by rename (never truncated in place). An empty
// or partial lock younger than lockWriteGrace is a write in progress: held.
func TryAcquireConsolidationLock() (time.Time, bool, error) {
	// In-process transitions of the lock are serialized: this process's own
	// PID counts as held only once ownLock is set, so a second goroutine
	// reading the lock between the create and setOwnLock would otherwise see
	// "our PID, no run" and take it over too.
	lockStateMu.Lock()
	defer lockStateMu.Unlock()
	path := lockPath()
	_ = os.MkdirAll(filepath.Dir(path), 0700)

	info, data, err := readLock(path)
	if os.IsNotExist(err) {
		if err := createLockExclusive(path); err != nil {
			if os.IsExist(err) {
				return time.Time{}, false, nil // another worker created it first
			}
			return time.Time{}, false, err
		}
		setOwnLock(path)
		return time.Time{}, true, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if lockHeld(path, info, data) {
		return time.Time{}, false, nil
	}

	// The lock is stale (dead holder, or older than holderStaleMs): take it
	// over, one worker at a time.
	guard := path + takeoverSuffix
	release, ok, err := acquireTakeoverGuard(guard)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	defer release()
	// Re-check under the guard: another worker may have taken it over (or
	// created a fresh one) between our read and the guard.
	info2, data2, err := readLock(path)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil // removed meanwhile (a rollback): retry later
		}
		return time.Time{}, false, err
	}
	if !info2.ModTime().Equal(info.ModTime()) || string(data2) != string(data) || info2.Size() != info.Size() {
		return time.Time{}, false, nil
	}
	if err := fsatomic.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return time.Time{}, false, err
	}
	setOwnLock(path)
	return time.UnixMilli(info.ModTime().UnixMilli()), true, nil
}

// lockDonePrefix starts the body of a lock whose run completed ("done
// <pid>"). The file stays, since its mtime is the last consolidation time,
// but it is held by nobody.
const lockDonePrefix = "done "

// ownLock is the lock path a run of this process holds ("" when none): from
// a successful acquire until the run completes (markConsolidationDone) or is
// rolled back. It tells this process's own PID in the lock apart from a run
// of this process actually holding it.
var ownLock struct {
	sync.Mutex
	path string
}

// lockStateMu serializes this process's acquire, completion and rollback of
// the lock, so ownLock always agrees with the lock file for this process.
var lockStateMu sync.Mutex

func setOwnLock(path string) {
	ownLock.Lock()
	ownLock.path = path
	ownLock.Unlock()
}

func clearOwnLock(path string) {
	ownLock.Lock()
	if ownLock.path == path {
		ownLock.path = ""
	}
	ownLock.Unlock()
}

func ownsLock(path string) bool {
	ownLock.Lock()
	defer ownLock.Unlock()
	return ownLock.path != "" && ownLock.path == path
}

// markConsolidationDone releases the lock of a run of this process that
// completed. The lock used to keep this process's PID, and a live PID counts
// as held for holderStaleMs: "/dream run" later in the same session was told
// another consolidation held the lock (and so was any other cove process
// for an hour). The body becomes lockDonePrefix+PID and the mtime is put
// back, so the last consolidation time is still when the run took the lock.
// A lock no longer carrying this process's PID (taken over meanwhile) is
// left alone.
func markConsolidationDone() {
	lockStateMu.Lock()
	defer lockStateMu.Unlock()
	path := lockPath()
	if !ownsLock(path) {
		return
	}
	defer clearOwnLock(path)
	info, data, err := readLock(path)
	if err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(os.Getpid()) {
		return
	}
	stamp := info.ModTime()
	if err := fsatomic.WriteFile(path, []byte(lockDonePrefix+strconv.Itoa(os.Getpid())), 0644); err != nil {
		log.Warnf("[autoDream] mark the consolidation lock done: %v", err)
		return
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		log.Warnf("[autoDream] restore the consolidation lock time: %v", err)
	}
}

// lockWriteGrace is how long an empty or unparseable lock file counts as
// held: that is a lock being created (O_EXCL create, PID not yet written).
// A rolled-back lock is empty too, but carries its old timestamp.
const lockWriteGrace = 5 * time.Second

// takeoverGuardStale is the age past which a takeover guard is judged left
// behind by a crashed worker and removed.
const takeoverGuardStale = 30 * time.Second

func readLock(path string) (os.FileInfo, []byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return info, data, nil
}

// lockHeld reports whether the lock at path, described by info and data,
// belongs to a live run (or to one being written right now). A completed
// run's lock (lockDonePrefix) is held by nobody, and this process's own PID
// is held only while a run of this process has the lock: a live PID alone
// used to count, so this process's finished run blocked its own next one.
func lockHeld(path string, info os.FileInfo, data []byte) bool {
	age := time.Since(info.ModTime())
	if age >= time.Duration(holderStaleMs)*time.Millisecond {
		return false
	}
	body := strings.TrimSpace(string(data))
	if strings.HasPrefix(body, strings.TrimSpace(lockDonePrefix)) {
		return false
	}
	pid, err := strconv.Atoi(body)
	if err == nil && pid == os.Getpid() {
		return ownsLock(path)
	}
	if err != nil || pid <= 0 {
		// Empty or partial: a lock mid-creation when fresh.
		return age < lockWriteGrace && age > -lockWriteGrace
	}
	if isProcessRunning(pid) {
		log.Debugf("[autoDream] lock held by live PID %d (mtime %ds ago)", pid, int64(age/time.Second))
		return true
	}
	return false
}

// createLockExclusive creates a lock that did not exist, with this
// process's PID written in the same open.
func createLockExclusive(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(strconv.Itoa(os.Getpid()))
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// acquireTakeoverGuard creates the takeover guard with O_EXCL. ok is false
// while another worker holds it; a guard older than takeoverGuardStale was
// left by a crashed worker and is removed (the caller retries later).
func acquireTakeoverGuard(guard string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(guard, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		if os.IsExist(err) {
			if gi, serr := os.Stat(guard); serr == nil && time.Since(gi.ModTime()) > takeoverGuardStale {
				_ = os.Remove(guard)
			}
			return nil, false, nil
		}
		return nil, false, err
	}
	_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
	_ = f.Close()
	return func() { _ = os.Remove(guard) }, true, nil
}

// RollbackConsolidationLock rewinds the lock mtime to the prior value (or
// removes it if zero) for a run of this process that did not complete.
//
// It only acts while this process owns the lock. A run's completion is
// markConsolidationDone (which releases ownership) and then task.Complete;
// CancelActive in between (the process exiting as a run finished) failed the
// task and called this, which rewrote the lock regardless: the finished
// run's done marker and timestamp were destroyed, so its sessions were
// reviewed again by the next run. recoverDeadWorker, which rolls back
// another process's lock on purpose, uses rollbackLockFile directly.
func RollbackConsolidationLock(priorMtime time.Time) error {
	lockStateMu.Lock()
	defer lockStateMu.Unlock()
	path := lockPath()
	if !ownsLock(path) {
		return nil
	}
	defer clearOwnLock(path)
	return rollbackLockFile(path, priorMtime)
}

// rollbackLockFile is RollbackConsolidationLock's file change, without
// touching this process's ownership (recoverDeadWorker rolls back another
// process's lock).
func rollbackLockFile(path string, priorMtime time.Time) error {
	if priorMtime.IsZero() {
		return os.Remove(path)
	}
	// Clear the PID body by replacing the file rather than truncating it in
	// place. Until the Chtimes lands the empty file is young, which a
	// concurrent acquirer reads as a lock mid-creation (held), never free.
	if err := fsatomic.WriteFile(path, nil, 0644); err != nil {
		return err
	}
	return os.Chtimes(path, priorMtime, priorMtime)
}

// RecordConsolidation stamps the lock file (used by manual dream trigger).
// It records a consolidation time, not a run in progress, so the body is a
// done marker: it used to be this process's PID, which every process then
// read as a live holder for an hour.
func RecordConsolidation() error {
	path := lockPath()
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	// Replaced atomically: a truncated-then-written lock could be read empty.
	return fsatomic.WriteFile(path, []byte(fmt.Sprintf("%s%d", lockDonePrefix, os.Getpid())), 0644)
}

// ListSessionsTouchedSince returns session IDs with mtime after the given time.
func ListSessionsTouchedSince(since time.Time, sessionsDir string) ([]string, error) {
	// session.ListSessionFiles knows the directory layout: <id>.jsonl and
	// legacy <id>.json are sessions, index.json is not.
	names, err := session.ListSessionFiles(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, name := range names {
		info, err := os.Stat(filepath.Join(sessionsDir, name))
		if err != nil {
			continue
		}
		if info.ModTime().After(since) {
			ids = append(ids, session.SessionIDFromFile(name))
		}
	}
	return ids, nil
}
