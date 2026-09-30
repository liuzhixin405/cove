package repomap

import (
	"os"
	"path/filepath"
	"testing"
)

// A source file linked in from outside the project is not mapped: its
// outline used to reach the model although read refuses the path.
func TestScanSkipsLinksOutOfTheProject(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.go")
	if err := os.WriteFile(secret, []byte("package x\n\nfunc SecretOutside() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package x\n\nfunc Inside() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "linked.go")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "main.go"), filepath.Join(root, "alias.go")); err != nil {
		t.Fatal(err)
	}
	files := (&Index{root: root, files: map[string]*fileEntry{}}).scan()
	if _, ok := files["linked.go"]; ok {
		t.Fatalf("a link out of the project was mapped: %v", files)
	}
	if _, ok := files["main.go"]; !ok {
		t.Fatalf("the project's own file is missing: %v", files)
	}
	if _, ok := files["alias.go"]; !ok {
		t.Fatalf("a link inside the project was dropped: %v", files)
	}
}
