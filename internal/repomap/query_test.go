package repomap

import (
	"strings"
	"testing"
)

func TestQueryFindsFilesByPathAndSymbol(t *testing.T) {
	root := syntheticRepo(t)

	out := Query(root, []string{"turnContextNote"}, 12*1024)
	if !strings.Contains(out, "internal/engine/turn.go") {
		t.Fatalf("symbol query missed turn.go:\n%s", out)
	}
	if strings.Contains(out, "internal/store/store.go") {
		t.Fatalf("symbol query returned an unrelated file:\n%s", out)
	}

	out = Query(root, []string{"internal/store"}, 12*1024)
	if !strings.Contains(out, "internal/store/store.go") || !strings.Contains(out, "Save") {
		t.Fatalf("path query missed store.go:\n%s", out)
	}
	if strings.Contains(out, "engine.go") {
		t.Fatalf("path query returned engine.go:\n%s", out)
	}
}

func TestQueryRanksSourceAboveTests(t *testing.T) {
	root := syntheticRepo(t)
	out := Query(root, []string{"engine"}, 12*1024)
	src, test := strings.Index(out, "internal/engine/engine.go"), strings.Index(out, "turn_test.go")
	if src < 0 {
		t.Fatalf("engine query missed engine.go:\n%s", out)
	}
	if test >= 0 && test < src {
		t.Fatalf("test file ranked above source:\n%s", out)
	}
}

func TestQueryNoTermsListsTopRanked(t *testing.T) {
	root := syntheticRepo(t)
	out := Query(root, nil, 12*1024)
	if !strings.Contains(out, ".go") {
		t.Fatalf("empty query returned nothing useful:\n%s", out)
	}
}

func TestQueryNoMatch(t *testing.T) {
	root := syntheticRepo(t)
	if out := Query(root, []string{"zzzNothingMatches"}, 12*1024); out != "" {
		t.Fatalf("unmatched query = %q, want empty", out)
	}
}

func TestQueryRespectsBudget(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	var body strings.Builder
	body.WriteString("package big\n\n")
	for i := 0; i < 300; i++ {
		body.WriteString("func HandlerWithAVeryLongDescriptiveName" + itoa(i) + "(requestContext string, responseWriter int) {}\n")
	}
	for i := 0; i < 20; i++ {
		files["big/handler"+itoa(i)+".go"] = body.String()
	}
	writeTree(t, root, files)
	for _, budget := range []int{500, 4096, 12 * 1024} {
		out := Query(root, []string{"handler"}, budget)
		if len(out) > budget {
			t.Fatalf("budget %d: got %d bytes", budget, len(out))
		}
		if out == "" {
			t.Fatalf("budget %d: empty result", budget)
		}
	}
}

func TestQueryInRestrictsToPath(t *testing.T) {
	root := syntheticRepo(t)
	out := QueryIn(root, "internal/engine", nil, 12*1024)
	if !strings.Contains(out, "internal/engine/") || strings.Contains(out, "internal/store") || strings.Contains(out, "cmd/app") {
		t.Fatalf("path-restricted query leaked:\n%s", out)
	}
}

func TestExtractTerms(t *testing.T) {
	got := ExtractTerms("修复 internal/engine/engine.go 里的 panic，看看 `buildSystemPrompt` 和 turnContextNote() 的 the and")
	joined := strings.Join(got, " ")
	for _, want := range []string{"internal/engine/engine.go", "buildSystemPrompt", "turnContextNote", "panic"} {
		if !strings.Contains(joined, want) {
			t.Errorf("terms %v lack %q", got, want)
		}
	}
	for _, bad := range []string{"the", "and"} {
		for _, g := range got {
			if g == bad {
				t.Errorf("stopword %q kept: %v", bad, got)
			}
		}
	}
	if len(ExtractTerms("你好")) != 0 {
		t.Errorf("greeting produced terms: %v", ExtractTerms("你好"))
	}
}
