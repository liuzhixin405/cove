package tool

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// screenshotPath only compared the path text with the workspace, and
// os.WriteFile follows links: a junction out -> C:\Users\me\Desktop (mklink
// /J needs no privilege) with output "out/x.png" wrote outside the
// workspace, from a tool read-only sub-agents run with no permission gate.
func TestScreenshotPathRefusesLinkedDirectoryOutOfWorkspace(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(cwd, "out")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Skipf("mklink /J failed: %v %s", err, out)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, out := range []string{"out/x.png", "out/new/x.png", filepath.Join(link, "x.png")} {
		if p, err := screenshotPath(Input{"output": out}, cwd); err == nil {
			t.Errorf("output %q through a junction out of the workspace was accepted: %s", out, p)
		}
	}
	if _, err := screenshotPath(Input{"output": "shots/x.png"}, cwd); err != nil {
		t.Errorf("in-workspace output refused: %v", err)
	}
}

// A symlinked file: writing the screenshot would overwrite the link's target.
// Needs symlink privilege.
func TestScreenshotPathRefusesSymlinkedFile(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "logo.png")
	inTarget := filepath.Join(cwd, "real.png")
	for _, p := range []string{target, inTarget} {
		if err := os.WriteFile(p, []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(cwd, "logo.png")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(inTarget, filepath.Join(cwd, "alias.png")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "missing.png"), filepath.Join(cwd, "dangling.png")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, out := range []string{"logo.png", "alias.png", "dangling.png"} {
		if _, err := screenshotPath(Input{"output": out}, cwd); err == nil {
			t.Errorf("output %q is a symlink and was accepted", out)
		}
	}
}
