package tool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// grep's built-in search (the usual one on Windows, where rg is rarely
// installed) opened every entry projectFiles listed, and os.Open follows
// links: a project file notes.txt -> ../../.ssh/id_rsa printed the private
// key, while read refused the same path. grep is auto-allowed, so nothing
// asked first. Needs symlink privilege; TestConfineFilesDropsEntriesThroughAJunction
// covers the same check without it.
func TestGrepSkipsSymlinksOutOfTheWorkingDirectory(t *testing.T) {
	for _, git := range []bool{false, true} {
		name := "walk"
		if git {
			name = "git"
		}
		t.Run(name, func(t *testing.T) {
			grepModes(t, func(t *testing.T) {
				cwd, outside := t.TempDir(), t.TempDir()
				if git {
					gitInit(t, cwd)
				}
				secret := filepath.Join(outside, "id_rsa")
				writeTree(t, outside, map[string]string{"id_rsa": "PRIVATE KEY MATERIAL\n"})
				writeTree(t, cwd, map[string]string{"real.txt": "inside content\n"})
				if err := os.Symlink(secret, filepath.Join(cwd, "notes.txt")); err != nil {
					t.Skipf("symlink: %v", err)
				}
				// A link to a file inside the project is still searched.
				if err := os.Symlink(filepath.Join(cwd, "real.txt"), filepath.Join(cwd, "alias.txt")); err != nil {
					t.Skipf("symlink: %v", err)
				}

				res := grep(t, cwd, Input{"pattern": "."})
				if strings.Contains(res.Data, "PRIVATE KEY") {
					t.Fatalf("grep printed a file outside the working directory through a link:\n%s", res.Data)
				}
				if !strings.Contains(res.Data, "real.txt") {
					t.Errorf("in-project file missing from grep result:\n%s", res.Data)
				}

				gres, _ := NewGlobTool().Call(context.Background(), Input{"pattern": "*.txt"}, Context{Cwd: cwd})
				if strings.Contains(gres.Data, "notes.txt") {
					t.Errorf("glob listed a link out of the working directory:\n%s", gres.Data)
				}
				if !strings.Contains(gres.Data, "real.txt") || !strings.Contains(gres.Data, "alias.txt") {
					t.Errorf("glob lost in-project files:\n%s", gres.Data)
				}
			})
		})
	}
}

// The same confinement without symlink privilege: a junction (mklink /J
// needs none) in the path of a listed entry points outside the project.
// projectFiles does not descend into it, but git may list files below one
// and the check must not rely on that.
func TestConfineFilesDropsEntriesThroughAJunction(t *testing.T) {
	cwd, outside := t.TempDir(), t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "x"})
	writeTree(t, cwd, map[string]string{"ok.txt": "x", "sub/b.txt": "x"})
	link := filepath.Join(cwd, "j")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
			t.Skipf("mklink /J failed: %v %s", err, out)
		}
	} else if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	got := confineFiles(cwd, cwd, []string{"ok.txt", "j", "j/secret.txt", "sub/b.txt"})
	if strings.Join(got, ",") != "ok.txt,sub/b.txt" {
		t.Fatalf("confineFiles = %v, want [ok.txt sub/b.txt]", got)
	}
	// Searched from a subdirectory, entries are relative to it.
	got = confineFiles(cwd, filepath.Join(cwd, "sub"), []string{"b.txt"})
	if strings.Join(got, ",") != "b.txt" {
		t.Fatalf("confineFiles from sub = %v, want [b.txt]", got)
	}
	// No working directory: nothing to confine to, as in resolvePathInCwd.
	got = confineFiles("", cwd, []string{"j/secret.txt"})
	if len(got) != 1 {
		t.Fatalf("confineFiles without root = %v", got)
	}

	// And end to end: grep through the junction's listing finds nothing
	// outside.
	withoutRipgrep(t)
	res := grep(t, cwd, Input{"pattern": "x"})
	if strings.Contains(res.Data, "secret") {
		t.Fatalf("grep reached a file behind a junction:\n%s", res.Data)
	}
}
