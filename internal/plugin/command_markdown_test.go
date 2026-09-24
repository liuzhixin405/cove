package plugin

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestParseCommandMarkdownKeepsChineseDescriptionValid: descriptions were cut
// at byte 57, which lands inside a three-byte Chinese character and put an
// invalid UTF-8 sequence (shown as U+FFFD) into /help and the completion menu.
func TestParseCommandMarkdownKeepsChineseDescriptionValid(t *testing.T) {
	// The leading ASCII byte shifts the cut off a rune boundary.
	long := "a" + strings.Repeat("审查代码", 20)
	desc, _ := parseCommandMarkdown("---\ndescription: " + long + "\n---\nbody")
	if !utf8.ValidString(desc) {
		t.Fatalf("description is not valid UTF-8: %q", desc)
	}
	if !strings.HasSuffix(desc, "...") || utf8.RuneCountInString(desc) > 60 {
		t.Errorf("description = %q, want at most 60 runes ending in ...", desc)
	}
}

// TestParseCommandMarkdownStripsBOM: a command file saved by a Windows editor
// with a UTF-8 byte-order mark does not start with "---". The frontmatter was
// then not recognised, so its lines were sent to the model as the prompt and
// the description became "---".
func TestParseCommandMarkdownStripsBOM(t *testing.T) {
	bom := string(rune(0xFEFF))
	crlf := string([]byte{13, 10})
	content := bom + "---" + crlf + "description: Review code" + crlf + "---" + crlf + "Please review." + crlf
	desc, body := parseCommandMarkdown(content)
	if desc != "Review code" {
		t.Errorf("description = %q, want %q", desc, "Review code")
	}
	if body != "Please review." {
		t.Errorf("body = %q, want the text after the frontmatter", body)
	}
}

// TestMarketplaceSearchKeepsChineseDescriptionValid is the same byte cut in
// the /plugin search listing.
func TestMarketplaceSearchKeepsChineseDescriptionValid(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	mgr := NewManager()
	mgr.Init()
	mgr.Marketplace().index = []MarketplaceEntry{{Name: "zh", Description: strings.Repeat("中文插件说明", 20)}}

	out := mgr.MarketplaceSearch("")
	if !utf8.ValidString(out) {
		t.Fatalf("search output is not valid UTF-8: %q", out)
	}
}
