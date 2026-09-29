package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkStaleMemories(t *testing.T) {
	root, mem := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "internal", "engine"), 0o755))
	must(os.WriteFile(filepath.Join(root, "internal", "engine", "turn.go"), []byte("package engine\nfunc turnContextNote() {}\nvar key = \"done_verify_auto\"\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "CHANGELOG.md"), []byte("removed `oldHelper`\n"), 0o644))

	stale := "---\nname: engine-notes\n---\n\nThe note is built in internal/engine/turn.go by `turnContextNote`; see internal/engine/gone.go and `oldHelper()`, `EnhancedGenerator.GenerateIncremental`.\nConfig `done_verify_auto`. Logs in ~/.cove/errors.log and sessions/x.jsonl. Use `go test` and `turn.go`, not `missing.go`.\n"
	fresh := "---\nname: ok\n---\n\nAll in internal/engine/turn.go.\n"
	must(os.WriteFile(filepath.Join(mem, "stale.md"), []byte(stale), 0o644))
	must(os.WriteFile(filepath.Join(mem, "fresh.md"), []byte(fresh), 0o644))
	must(os.WriteFile(filepath.Join(mem, "INDEX.md"), []byte("- [x](stale.md) internal/engine/nope.go\n"), 0o644))

	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	reports := markStaleMemories(root, mem, now)
	if len(reports) != 1 || filepath.Base(reports[0].File) != "stale.md" {
		t.Fatalf("reports = %+v", reports)
	}
	got := strings.Join(reports[0].Missing, ",")
	for _, want := range []string{"internal/engine/gone.go", "oldHelper", "GenerateIncremental", "missing.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing list %q lacks %q", got, want)
		}
	}
	for _, bad := range []string{"turn.go", "turnContextNote", "done_verify_auto", "errors.log", "x.jsonl", "go test"} {
		for _, m := range reports[0].Missing {
			if strings.HasSuffix(m, bad) && !strings.Contains(m, "gone") {
				t.Errorf("%q wrongly reported missing (%q)", bad, got)
			}
		}
	}
	data, _ := os.ReadFile(filepath.Join(mem, "stale.md"))
	body := string(data)
	if !strings.HasPrefix(body, "---\nname: engine-notes\n---\n"+staleMarkerPrefix+"2026-09-29]") {
		t.Fatalf("marker not under the frontmatter:\n%s", body)
	}
	if d, _ := os.ReadFile(filepath.Join(mem, "fresh.md")); string(d) != fresh {
		t.Fatalf("a fresh memory was changed:\n%s", d)
	}

	// Idempotent: the same findings keep the marker (and its date).
	markStaleMemories(root, mem, now.Add(48*time.Hour))
	again, _ := os.ReadFile(filepath.Join(mem, "stale.md"))
	if string(again) != body {
		t.Fatalf("second run changed the memory:\n%s", again)
	}

	// Fixed memory: the marker goes.
	must(os.WriteFile(filepath.Join(mem, "stale.md"), []byte(strings.Replace(body, "see internal/engine/gone.go and `oldHelper()`, `EnhancedGenerator.GenerateIncremental`.\n", "\n", 1)), 0o644))
	fixed := strings.ReplaceAll(string(mustRead(t, filepath.Join(mem, "stale.md"))), " not `missing.go`", "")
	must(os.WriteFile(filepath.Join(mem, "stale.md"), []byte(fixed), 0o644))
	if r := markStaleMemories(root, mem, now); len(r) != 0 {
		t.Fatalf("fixed memory still reported: %+v", r)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(mem, "stale.md"))), staleMarkerPrefix) {
		t.Fatal("marker not removed after the fix")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	d, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
