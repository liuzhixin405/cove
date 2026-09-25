package permission

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// linkDir makes link point at target: a junction on Windows (no privilege
// needed), a symlink elsewhere. The test is skipped when neither can be made.
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("mklink /J failed: %v %s", err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
}

// A link inside the project that points outside it is outside: writing
// through it changes files the "inside the project" auto-allow never meant
// to cover. Existing files, new files and new subdirectories below the link
// all count.
func TestPathInsideFollowsLinksOutOfTheProject(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape")
	linkDir(t, outside, link)

	for _, target := range []string{
		"escape",
		"escape/secret.txt",
		filepath.Join(link, "secret.txt"),
		"escape/new.go",
		"escape/newdir/new.go",
	} {
		if PathInside(root, target) {
			t.Errorf("PathInside(root, %q) = true through a link to %s", target, outside)
		}
	}
	for _, target := range []string{"a.go", "sub/new/b.go", "."} {
		if !PathInside(root, target) {
			t.Errorf("PathInside(root, %q) = false, want true", target)
		}
	}
}

// A root reached through a link still contains its own files.
func TestPathInsideRootBehindLink(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "proj")
	linkDir(t, realDir, link)
	if !PathInside(link, "a.go") || !PathInside(link, filepath.Join(realDir, "a.go")) {
		t.Error("files of a linked root must be inside it")
	}
}

// A link chain longer than maxLinkHops cannot be followed to its end, so the
// path is not known to be inside: PathInside fails closed.
func TestPathInsideLinkChainTooLongIsOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	const n = maxLinkHops + 1
	// l0 -> l1 -> ... -> l{n-1} -> outside: n hops.
	linkDir(t, outside, filepath.Join(root, "l"+strconv.Itoa(n-1)))
	for i := n - 2; i >= 0; i-- {
		linkDir(t, filepath.Join(root, "l"+strconv.Itoa(i+1)), filepath.Join(root, "l"+strconv.Itoa(i)))
	}
	if PathInside(root, "l0/x.go") {
		t.Fatalf("a %d-link chain ending outside the project was judged inside", n)
	}
}

// Same, with the file system faked: a link that points at itself loops
// until the hop limit, and the result is "outside" (fail closed).
func TestPathInsideLinkLoopFailsClosed(t *testing.T) {
	root := t.TempDir()
	loop := filepath.Join(root, "loop")
	origStat, origRead := linkMode, readLink
	t.Cleanup(func() { linkMode, readLink = origStat, origRead })
	linkMode = func(p string) (os.FileMode, error) {
		if p == loop {
			return os.ModeSymlink, nil
		}
		return origStat(p)
	}
	readLink = func(p string) (string, error) {
		if p == loop {
			return loop, nil
		}
		return origRead(p)
	}
	if PathInside(root, "loop/x.go") {
		t.Fatal("a looping link was judged inside the project")
	}
	if !PathInside(root, "other/x.go") {
		t.Fatal("ordinary path must stay inside")
	}
}
