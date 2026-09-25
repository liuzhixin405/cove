package engine

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/token"
)

// dedupeHistory builds a user message followed by one assistant/tool pair per
// content; the tool call IDs are call_0, call_1, ...
func dedupeHistory(contents []string) []api.Message {
	h := []api.Message{{Role: "user", Content: "go"}}
	for i, c := range contents {
		id := fmt.Sprintf("call_%d", i)
		h = append(h,
			api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: id, Name: "bash"}}},
			api.Message{Role: "tool", ToolCallID: id, Name: "bash", Content: c},
		)
	}
	return h
}

// smallResults returns n short, distinct tool results, used to push earlier
// results out of the recent-protection window.
func smallResults(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("ok %d", i)
	}
	return out
}

func newDedupeMasker(t *testing.T) *ToolOutputMasker {
	m := NewToolOutputMasker()
	m.outputDir = t.TempDir()
	return m
}

func stubFor(id string, n int) string {
	return fmt.Sprintf("[identical to earlier tool result for call %s (%d bytes); content omitted]", id, n)
}

func TestDedupeRepeatedCatReplacesLaterCopies(t *testing.T) {
	big := strings.Repeat("line of file content\n", 200) // 4200 bytes, ~1400 tokens
	contents := append([]string{big, big, big}, smallResults(4)...)
	history := dedupeHistory(contents)
	m := newDedupeMasker(t)
	res, out := m.Mask(history, nil)

	if len(out) != len(history) {
		t.Fatalf("message count changed: %d -> %d", len(history), len(out))
	}
	if out[2].Content != big {
		t.Fatalf("first occurrence must be kept, got %q", out[2].Content)
	}
	want := stubFor("call_0", len(big))
	for _, i := range []int{4, 6} {
		if out[i].Content != want {
			t.Fatalf("out[%d] = %q, want %q", i, out[i].Content, want)
		}
		if out[i].ToolCallID != history[i].ToolCallID || out[i].Role != "tool" {
			t.Fatalf("out[%d] identity changed: %+v", i, out[i])
		}
	}
	if history[4].Content != big {
		t.Fatal("input history must not be mutated")
	}
	wantSaved := 2 * (token.Estimate(big) - token.Estimate(want))
	if wantSaved < dedupeMinSavedTokens {
		t.Fatalf("fixture saves %d tokens, below the %d threshold", wantSaved, dedupeMinSavedTokens)
	}
	if res.TokensSaved != wantSaved {
		t.Fatalf("TokensSaved = %d, want %d", res.TokensSaved, wantSaved)
	}
	if res.MaskedCount != 2 {
		t.Fatalf("MaskedCount = %d, want 2 (engine invalidates caches only when > 0)", res.MaskedCount)
	}
}

// Rewriting history invalidates the prompt cache; one small duplicate is not
// worth that on its own.
func TestDedupeSkipsSmallSavingsWhenNothingElseRewritesHistory(t *testing.T) {
	dup := strings.Repeat("x", 600)
	contents := append([]string{dup, dup}, smallResults(4)...)
	history := dedupeHistory(contents)
	m := newDedupeMasker(t)
	res, out := m.Mask(history, nil)
	if out[4].Content != dup {
		t.Fatalf("600-byte duplicate rewritten alone: %q", out[4].Content)
	}
	if res.MaskedCount != 0 || res.TokensSaved != 0 {
		t.Fatalf("unexpected savings: %+v", res)
	}
}

func TestDedupeKeepsDuplicatesWithinRecentFourToolResults(t *testing.T) {
	big := strings.Repeat("x", 9000)
	// The second copy is the 3rd-from-last tool result: protected.
	contents := append([]string{big, big}, smallResults(2)...)
	history := dedupeHistory(contents)
	m := newDedupeMasker(t)
	res, out := m.Mask(history, nil)
	if out[4].Content != big {
		t.Fatalf("recent duplicate replaced: %q", out[4].Content[:80])
	}
	if res.TokensSaved != 0 || res.MaskedCount != 0 {
		t.Fatalf("unexpected savings: %+v", res)
	}
}

func TestDedupeIgnoresShortResults(t *testing.T) {
	short := strings.Repeat("y", 511)
	contents := []string{}
	for i := 0; i < 20; i++ { // many copies: savings would pass the threshold
		contents = append(contents, short)
	}
	contents = append(contents, smallResults(4)...)
	m := newDedupeMasker(t)
	res, out := m.Mask(dedupeHistory(contents), nil)
	for i := 0; i < 20; i++ {
		if out[2+2*i].Content != short {
			t.Fatalf("short result %d replaced: %q", i, out[2+2*i].Content)
		}
	}
	if res.TokensSaved != 0 {
		t.Fatalf("TokensSaved = %d, want 0", res.TokensSaved)
	}
}

func TestDedupeIsIdempotent(t *testing.T) {
	big := strings.Repeat("z", 9000)
	contents := append([]string{big, big}, smallResults(4)...)
	m := newDedupeMasker(t)
	res1, once := m.Mask(dedupeHistory(contents), nil)
	if res1.MaskedCount != 1 {
		t.Fatalf("first pass MaskedCount = %d, want 1", res1.MaskedCount)
	}
	res, twice := m.Mask(once, nil)
	if res.TokensSaved != 0 || res.MaskedCount != 0 {
		t.Fatalf("second pass reported savings: %+v", res)
	}
	for i := range once {
		if once[i].Content != twice[i].Content {
			t.Fatalf("second pass changed message %d", i)
		}
	}
}

// When disk masking rewrites history this round anyway, even a small
// duplicate is collapsed; the first copy goes to disk and the stub still
// names its call, so nothing is lost.
func TestDedupeRidesAlongWithDiskMasking(t *testing.T) {
	dup := strings.Repeat("a", 600)
	contents := append([]string{dup, dup}, smallResults(4)...)
	history := dedupeHistory(contents)
	m := newDedupeMasker(t)
	m.protectionThreshold = 40 // only the last few messages are protected
	m.minPrunableThreshold = 100

	res, out := m.Mask(history, nil)
	if !strings.HasPrefix(out[2].Content, maskedPrefix) {
		t.Fatalf("first copy not masked to disk: %q", out[2].Content)
	}
	path := regexp.MustCompile(`masked to (.+)$`).FindStringSubmatch(out[2].Content)
	if path == nil {
		t.Fatalf("placeholder has no file path: %q", out[2].Content)
	}
	data, err := os.ReadFile(path[1])
	if err != nil || string(data) != dup {
		t.Fatalf("masked file lost the content (err %v)", err)
	}
	if want := stubFor("call_0", len(dup)); out[4].Content != want {
		t.Fatalf("out[4] = %q, want %q", out[4].Content, want)
	}
	if res.MaskedCount != 2 {
		t.Fatalf("MaskedCount = %d, want 2 (1 masked + 1 deduped)", res.MaskedCount)
	}
}

// Placeholders of outputs already masked to disk are never treated as
// duplicates of each other.
func TestDedupeLeavesMaskedPlaceholdersAlone(t *testing.T) {
	ph := maskedPrefix + "bash...] 3000 tokens masked to " + strings.Repeat("p", 9000)
	contents := append([]string{ph, ph}, smallResults(4)...)
	m := newDedupeMasker(t)
	res, out := m.Mask(dedupeHistory(contents), nil)
	if out[2].Content != ph || out[4].Content != ph {
		t.Fatal("masked placeholder rewritten")
	}
	if res.MaskedCount != 0 {
		t.Fatalf("MaskedCount = %d, want 0", res.MaskedCount)
	}
}
