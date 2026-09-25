package command

import (
	"strings"
	"testing"

	"github.com/liuzhixin405/cove/internal/config"
)

func TestWebSearchHint(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	if h := webSearchHint(&config.Config{}); !strings.Contains(h, "web_search") {
		t.Fatalf("unconfigured hint missing: %q", h)
	}
	if h := webSearchHint(&config.Config{WebSearch: &config.WebSearchConfig{Provider: "tavily"}}); !strings.Contains(h, "api_key") {
		t.Fatalf("missing-key hint: %q", h)
	}
	if h := webSearchHint(&config.Config{WebSearch: &config.WebSearchConfig{Provider: "brave", APIKey: "k"}}); h != "" {
		t.Fatalf("configured should be silent: %q", h)
	}
	if h := webSearchHint(&config.Config{WebSearch: &config.WebSearchConfig{Provider: "duckduckgo"}}); h != "" {
		t.Fatalf("explicit duckduckgo should be silent: %q", h)
	}
}
