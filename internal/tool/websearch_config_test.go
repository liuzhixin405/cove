package tool

import "testing"

func TestResolveWebSearchBackend(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "")
	t.Setenv("BRAVE_API_KEY", "")
	t.Setenv("BRAVE_SEARCH_API_KEY", "")
	cases := []struct {
		name         string
		cfg          WebSearchSettings
		env          map[string]string
		wantProvider string
		wantKey      string
	}{
		{"nothing", WebSearchSettings{}, nil, "duckduckgo", ""},
		{"env tavily", WebSearchSettings{}, map[string]string{"TAVILY_API_KEY": "t"}, "tavily", "t"},
		{"env brave", WebSearchSettings{}, map[string]string{"BRAVE_API_KEY": "b"}, "brave", "b"},
		{"config brave", WebSearchSettings{Provider: "brave", APIKey: "c"}, nil, "brave", "c"},
		{"config brave key from env", WebSearchSettings{Provider: "Brave"}, map[string]string{"BRAVE_API_KEY": "e"}, "brave", "e"},
		{"config tavily no key falls back", WebSearchSettings{Provider: "tavily"}, nil, "duckduckgo", ""},
		{"config ddg beats env", WebSearchSettings{Provider: "duckduckgo"}, map[string]string{"TAVILY_API_KEY": "t"}, "duckduckgo", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			p, k := ResolveWebSearchBackend(c.cfg)
			if p != c.wantProvider || k != c.wantKey {
				t.Fatalf("got %q/%q, want %q/%q", p, k, c.wantProvider, c.wantKey)
			}
		})
	}
}

func TestWebSearchToolUsesSettings(t *testing.T) {
	tl := NewWebSearchToolWith(WebSearchSettings{Provider: "brave", APIKey: "x"}).(*WebSearchTool)
	if tl.settings.Provider != "brave" {
		t.Fatal("settings not kept")
	}
}
