package skills

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallSkillRejectsTraversalName is the regression test for the arbitrary
// write in InstallSkill: the name became a directory under ~/.cove/skills with
// no validation, so "../../../.ssh" created that directory and wrote a
// SKILL.md into it.
func TestInstallSkillRejectsTraversalName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows

	for _, name := range []string{
		"../../../.ssh",
		"..",
		"a/b",
		`a\b`,
		"",
		".hidden",
	} {
		if err := InstallSkill(name, "local", ""); err == nil {
			t.Errorf("InstallSkill(%q) = nil, want a rejection", name)
		}
	}

	// Nothing may have been created outside the skills root.
	if _, err := os.Stat(filepath.Join(home, ".ssh")); err == nil {
		t.Fatal("InstallSkill created a directory outside the skills root")
	}
}

// TestInstallSkillAcceptsNormalName guards against the validation being too
// strict for real use.
func TestInstallSkillAcceptsNormalName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if err := InstallSkill("my-skill", "local", ""); err != nil {
		t.Fatalf("InstallSkill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".cove", "skills", "my-skill", "SKILL.md")); err != nil {
		t.Fatalf("skill file not created: %v", err)
	}
}

// TestInstallSkillRejectsInternalURL covers the SSRF hole: registry entries are
// fetched from the network, so a hostile entry could point the download at a
// link-local address and have the response written into a skill file — which is
// then injected into the model's system prompt.
func TestInstallSkillRejectsInternalURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	for _, url := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://127.0.0.1:8080/secret",
		"http://localhost/secret",
		"http://[::1]/secret",
		"file:///etc/passwd",
		"gopher://evil/x",
	} {
		err := InstallSkill("probe", "url", url)
		if err == nil {
			t.Errorf("InstallSkill accepted %q", url)
		}
	}

	// A refused download must not leave a skill directory behind.
	if _, err := os.Stat(filepath.Join(home, ".cove", "skills", "probe")); err == nil {
		t.Fatal("a refused download left an empty skill directory behind")
	}
}

// TestInstallSkillSurfacesHTTPError pins that a non-200 is reported instead of
// being written into the skill file as its content.
func TestInstallSkillSurfacesHTTPError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("404 page not found"))
	}))
	defer srv.Close()

	// The test server listens on loopback, which the SSRF guard blocks — so
	// either rejection is acceptable here; what must NOT happen is a skill file
	// containing the error body.
	err := InstallSkill("probe", "url", srv.URL)
	if err == nil {
		t.Fatal("expected an error for a non-200 / blocked download")
	}
	if data, rerr := os.ReadFile(filepath.Join(home, ".cove", "skills", "probe", "SKILL.md")); rerr == nil {
		if strings.Contains(string(data), "404") {
			t.Fatal("an HTTP error body was written into the skill file")
		}
	}
}
