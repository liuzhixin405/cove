//go:build windows

package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeLink makes link look like a symbolic link whose stored target is dest,
// without the symlink privilege this account may not have.
func fakeLink(t *testing.T, link, dest string) {
	t.Helper()
	origStat, origRead := linkMode, readLink
	t.Cleanup(func() { linkMode, readLink = origStat, origRead })
	linkMode = func(p string) (os.FileMode, error) {
		if p == link {
			return os.ModeSymlink, nil
		}
		return origStat(p)
	}
	readLink = func(p string) (string, error) {
		if p == link {
			return dest, nil
		}
		return origRead(p)
	}
}

// A target starting with a single "\" is relative to the root of the link's
// drive, not to its directory: D:\proj\l -> \Users\me\.ssh is D:\Users\me\.ssh.
// It used to be joined under D:\proj, so D:\proj\l\id_rsa counted as inside
// the project.
func TestWithinRootRelativeLinkTargetUsesTheLinksDrive(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "l")
	for _, dest := range []string{`\Users\me\.ssh`, `/Users/me/.ssh`} {
		fakeLink(t, link, dest)
		if Within(root, `l\id_rsa`) {
			t.Errorf("link -> %s: l\\id_rsa judged inside %s", dest, root)
		}
	}

	// A root-relative target that really is inside the project stays inside.
	fakeLink(t, link, root[len(filepath.VolumeName(root)):]+`\sub`)
	if !Within(root, `l\x.go`) {
		t.Error("root-relative link back into the project judged outside")
	}
}

// "C:foo" is relative to the current directory of drive C:, which this
// process cannot know for the link's context: fail closed.
func TestWithinDriveRelativeLinkTargetFailsClosed(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "l")
	fakeLink(t, link, filepath.VolumeName(root)+"sub")
	if Within(root, `l\x.go`) {
		t.Error("drive-relative link target judged inside")
	}
}

func TestLinkTarget(t *testing.T) {
	for _, tc := range []struct {
		dir, dest, want string
		ok              bool
	}{
		{`D:\proj`, `\Users\me\.ssh`, `D:\Users\me\.ssh`, true},
		{`D:\proj`, `/Users/me`, `D:\Users\me`, true},
		{`D:\proj`, `sub\x`, `D:\proj\sub\x`, true},
		{`D:\proj`, `..\x`, `D:\x`, true},
		{`D:\proj`, `C:\x`, `C:\x`, true},
		{`\\srv\share\proj`, `\x`, `\\srv\share\x`, true},
		{`D:\proj`, `C:foo`, ``, false},
		{`D:\proj`, `D:foo`, ``, false},
	} {
		got, ok := linkTarget(tc.dir, tc.dest)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("linkTarget(%q, %q) = %q, %v; want %q, %v", tc.dir, tc.dest, got, ok, tc.want, tc.ok)
		}
	}
}
