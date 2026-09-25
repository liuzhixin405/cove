package config

import "testing"

func TestExperimentalToolsAndWebSearchKeys(t *testing.T) {
	cfg := loadIn(t, "")
	if cfg.ExperimentalTools {
		t.Fatal("experimental_tools must default to false")
	}
	if cfg.WebSearch != nil {
		t.Fatal("web_search unset by default")
	}
	cfg = loadIn(t, `{"experimental_tools":true,"web_search":{"provider":"brave","api_key":"k"}}`)
	if !cfg.ExperimentalTools {
		t.Fatal("experimental_tools:true ignored in .cove.json")
	}
	if cfg.WebSearch == nil || cfg.WebSearch.Provider != "brave" || cfg.WebSearch.APIKey != "k" {
		t.Fatalf("web_search = %+v", cfg.WebSearch)
	}
}
