package diagnostic

import (
	"context"
	"os"
	"runtime"
	"syscall"
	"testing"
)

// checkDiskSpace turned every failed write into a fatal "磁盘空间不足": a
// ~/.cove the user may not write to (which checkDataDir already reports,
// with the right remedy) became a second, wrong fatal telling them to free
// disk space. Only a full disk is a disk-space problem.
func TestCheckDiskSpaceOnlyBlamesAFullDisk(t *testing.T) {
	c, _ := newTestChecker(t, nil)
	fail := func(err error) {
		c.writeFile = func(name string, _ []byte, _ os.FileMode) error {
			return &os.PathError{Op: "open", Path: name, Err: err}
		}
	}

	fail(syscall.EACCES)
	if res := c.checkDiskSpace(context.Background()); res.Error != nil || res.Status > SevInfo {
		t.Errorf("permission denied reported as %v / %+v, want no disk-space problem", res.Status, res.Error)
	}

	full := []error{syscall.ENOSPC}
	if runtime.GOOS == "windows" {
		full = append(full, syscall.Errno(112), syscall.Errno(39)) // ERROR_DISK_FULL, ERROR_HANDLE_DISK_FULL
	}
	for _, err := range full {
		fail(err)
		res := c.checkDiskSpace(context.Background())
		if res.Status != SevFatal || res.Error == nil || res.Error.Def.Code != ErrFSDiskFull {
			t.Errorf("%v: got %v / %+v, want a fatal disk-full error", err, res.Status, res.Error)
		}
	}

	c.writeFile = nil
	if res := c.checkDiskSpace(context.Background()); res.Status != SevInfo || res.Error != nil {
		t.Errorf("a writable data dir failed the check: %v / %+v", res.Status, res.Error)
	}
}
