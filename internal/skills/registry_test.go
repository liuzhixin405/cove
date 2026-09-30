package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// repoRoot is where skills-registry.json and skills/<name>/SKILL.md live.
const repoRoot = "../.."

const rawPrefix = "https://raw.githubusercontent.com/liuzhixin405/cove-agent/main/"

func loadRegistryFile(t *testing.T) []RegistryEntry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, "skills-registry.json"))
	if err != nil {
		t.Fatalf("read skills-registry.json: %v", err)
	}
	var entries []RegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse skills-registry.json: %v", err)
	}
	return entries
}

// TestFallbackMatchesRegistryFile keeps the offline fallback equal to the
// published registry, so /skill marketplace lists the same skills either way.
func TestFallbackMatchesRegistryFile(t *testing.T) {
	var fallback []RegistryEntry
	if err := json.Unmarshal([]byte(fallbackJSON), &fallback); err != nil {
		t.Fatalf("parse fallbackJSON: %v", err)
	}
	if file := loadRegistryFile(t); !reflect.DeepEqual(fallback, file) {
		t.Fatalf("fallbackJSON and skills-registry.json differ:\nfallback: %+v\nfile:     %+v", fallback, file)
	}
}

// TestRegistryEntriesPointAtRepoSkills pins that every entry has a url, that it
// names a SKILL.md committed in this repository, and that the file parses to a
// skill of the same name. The registry used to be missing and its fallback had
// no urls, so /skill install wrote a placeholder and reported success.
func TestRegistryEntriesPointAtRepoSkills(t *testing.T) {
	entries := loadRegistryFile(t)
	if len(entries) == 0 {
		t.Fatal("skills-registry.json has no entries")
	}
	for _, e := range entries {
		want := rawPrefix + "skills/" + e.Name + "/SKILL.md"
		if e.URL != want {
			t.Errorf("%s: url = %q, want %q", e.Name, e.URL, want)
			continue
		}
		rel := strings.TrimPrefix(e.URL, rawPrefix)
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("%s: %v", e.Name, err)
			continue
		}
		content := string(data)
		if strings.Contains(content, "\r\n") {
			t.Errorf("%s: SKILL.md has CRLF line endings; frontmatter parsing needs LF", e.Name)
		}
		if parseFrontmatter(content) == nil {
			t.Errorf("%s: SKILL.md has no frontmatter", e.Name)
			continue
		}
		if sk := parseSkill(e.Name, content, rel); sk.Name != e.Name || sk.Description != e.Description {
			t.Errorf("%s: SKILL.md parses as name=%q description=%q, want %q / %q",
				e.Name, sk.Name, sk.Description, e.Name, e.Description)
		}
	}
}
