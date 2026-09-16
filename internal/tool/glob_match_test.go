package tool

import "testing"

// TestMatchGlob covers the `**` handling. The regression it guards: a `**` in a
// middle position (`src/**/*.ts`) matched nothing at all, because filepath.Match
// has no doublestar support and `*` does not cross separators.
func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Bare pattern → basename match (the common shorthand).
		{"*.go", "main.go", true},
		{"*.go", "internal/tool/glob.go", true},
		{"*.go", "internal/tool/glob.ts", false},

		// Leading ** (the form that used to work).
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/tool/glob.go", true},

		// ** in the middle — the actual bug.
		{"src/**/*.ts", "src/index.ts", true},
		{"src/**/*.ts", "src/a/b/c.ts", true},
		{"src/**/*.ts", "lib/a/b/c.ts", false},
		{"src/**/*.ts", "src/a/b/c.go", false},

		// Trailing ** swallows the remainder.
		{"docs/**", "docs/guide/intro.md", true},
		{"docs/**", "docs", true},
		{"docs/**", "internal/docs/x.md", false},

		// Multiple **.
		{"**/test/**/*_test.go", "a/test/b/x_test.go", true},
		{"**/test/**/*_test.go", "test/x_test.go", true},
		{"**/test/**/*_test.go", "a/b/x_test.go", false},

		// Exact multi-segment patterns still work segment-by-segment.
		{"internal/*/glob.go", "internal/tool/glob.go", true},
		{"internal/*/glob.go", "internal/a/b/glob.go", false},
	}

	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.path); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

// TestMatchGlobWindowsSeparators verifies backslash paths (what filepath.Rel
// returns on Windows) are normalized before matching.
func TestMatchGlobWindowsSeparators(t *testing.T) {
	if !matchGlob("src/**/*.ts", `src\a\b\c.ts`) {
		t.Error("backslash-separated path did not match")
	}
	if !matchGlob(`src\**\*.ts`, "src/a/b/c.ts") {
		t.Error("backslash-separated pattern did not match")
	}
}
