// Package fsatomic provides crash-safe file replacement.
//
// The naive os.WriteFile truncates the destination before writing, so a crash,
// a full disk, or two writers racing leave the file half-written — and for the
// persisted state this project keeps (memory entries, session records, notes),
// a truncated file is worse than a stale one. WriteFile here writes to a
// temporary file in the same directory and renames it into place, which is
// atomic within a filesystem on both POSIX and Windows.
package fsatomic

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tempPrefix marks the in-progress files WriteFile creates.
//
// The temp file lives in the destination directory (it has to, so the rename
// stays on one filesystem), which means any code that enumerates that
// directory can see it. If the process dies between create and rename the file
// is left behind — and directories like ~/.cove/memory are read back wholesale,
// so a leftover would be loaded forever as a bogus memory entry. Readers must
// skip names matching IsTempName.
const tempPrefix = ".cove-tmp-"

// IsTempName reports whether name is an in-progress file created by WriteFile.
// Every directory scan over a location this package writes to must skip these.
func IsTempName(name string) bool {
	return strings.HasPrefix(filepath.Base(name), tempPrefix)
}

// WriteFile atomically replaces path with data.
//
// The temporary file is created alongside the destination (never in the system
// temp dir) so the final rename stays within one filesystem. On any failure the
// temporary file is removed and the original destination is left untouched.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tempPrefix+filepath.Base(path)+".")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	// Flush to disk before the rename: without this the rename can land while
	// the data blocks are still buffered, which after a power loss yields a
	// correctly-named but empty file.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	// CreateTemp always uses 0600; apply the caller's mode explicitly.
	if err := os.Chmod(tmpName, perm); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod temp for %s: %w", path, err)
	}
	if err := renameWithRetry(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename temp onto %s: %w", path, err)
	}
	return nil
}

// renameBackoff is the wait before each retry of a failed rename.
var renameBackoff = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond}

// Seams for tests.
var (
	rename          = os.Rename
	retryableRename = isRetryableRename
	sleep           = time.Sleep
)

// renameWithRetry renames from onto to, retrying a transient failure.
//
// On Windows, replacing a file fails with a sharing violation or access
// denied while another process (a second cove, an editor, an antivirus
// scanner) has the destination open. That lasts milliseconds, and giving up
// on it turned a routine write of a shared file such as the session index
// into an error.
func renameWithRetry(from, to string) error {
	err := rename(from, to)
	for _, d := range renameBackoff {
		if err == nil || !retryableRename(err) {
			return err
		}
		sleep(d)
		err = rename(from, to)
	}
	return err
}
