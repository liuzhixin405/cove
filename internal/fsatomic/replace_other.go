//go:build !windows

package fsatomic

import "os"

// platformRename is a plain rename off Windows. The temp file already has the
// caller's mode; ownership, xattrs and ACLs of the replaced file are not
// carried over (the same as any editor that saves by rename).
var platformRename = os.Rename
