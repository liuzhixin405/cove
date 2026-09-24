package tool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove/internal/fsatomic"
)

// replaceFile writes data to path for write and edit. It used to be a plain
// os.WriteFile, which truncates the file before writing it, so a crash or a
// full disk in between left the user's file truncated. Now a complete new file
// is renamed into place (fsatomic), keeping the old file's permission bits; if
// writing the new file fails, the old one is untouched.
func replaceFile(path string, data []byte) error {
	// Write the file a symlink points to, so the link itself survives; renaming
	// onto the link would replace it with a regular file.
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	} else if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		// resolvePathInCwd cannot resolve a dangling link either, so it only
		// checked the link's directory; following the link (as os.WriteFile
		// did) could create a file anywhere, outside the working directory too.
		return fmt.Errorf("%s is a symbolic link to a file that does not exist; write to the target path instead", path)
	}

	info, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return fsatomic.WriteFile(target, data, 0o644)
	}
	if err != nil {
		return err
	}
	perm := info.Mode().Perm()
	// Two cases keep the old in-place write:
	//   - A read-only file: renaming over it would succeed wherever the
	//     directory is writable and so quietly bypass the read-only bit; in
	//     place, the write fails as it always did.
	//   - A file with several hard links: a rename gives this name a new file
	//     and leaves the other names on the old content, which silently splits
	//     what the user linked on purpose (shared configs, pnpm stores). Losing
	//     atomicity for these rare files is the lesser harm. The link count
	//     costs a stat on Unix and one extra open on Windows.
	if perm&0o200 == 0 || hardLinkCount(target, info) > 1 {
		return os.WriteFile(target, data, perm)
	}

	err = fsatomic.WriteFile(target, data, perm)
	// On Windows the rename fails while another process holds the file open
	// without FILE_SHARE_DELETE, which editors, indexers and antivirus
	// scanners do. A scanner lets go within milliseconds, so retry briefly
	// before giving up on atomicity.
	for delay := 10 * time.Millisecond; err != nil && replaceBlocked(err) && delay <= 40*time.Millisecond; delay *= 2 {
		time.Sleep(delay)
		err = fsatomic.WriteFile(target, data, perm)
	}
	// Still blocked (an editor keeps the file open), or the directory does not
	// let us create the temp file even though the file itself is writable:
	// write in place, as os.WriteFile did, rather than fail. Other errors (a
	// full disk) are returned with the old file intact; an in-place write would
	// truncate it.
	if err != nil && (replaceBlocked(err) || errors.Is(err, fs.ErrPermission)) {
		return os.WriteFile(target, data, perm)
	}
	return err
}
