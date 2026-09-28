package api

import "testing"

// The engine holds the provider behind the metering and switching wrappers;
// what the provider declares must come through both.
func TestCapabilitiesPassThroughTheWrappers(t *testing.T) {
	anth := NewProvider(ProviderConfig{Name: "anthropic", APIKey: "k"})
	wrapped := NewMeteredProvider(NewSwitchableProvider(anth), nil)
	c := CapabilitiesOf(wrapped)
	if !c.CacheBreakpoints || !c.ToolsWithToolHistory || c.Family != "anthropic" {
		t.Fatalf("anthropic behind the wrappers = %+v", c)
	}
	openai := NewMeteredProvider(NewSwitchableProvider(NewProvider(ProviderConfig{Name: "deepseek", APIKey: "k"})), nil)
	if c := CapabilitiesOf(openai); c.CacheBreakpoints || c.ToolsWithToolHistory || c.Family == "anthropic" {
		t.Fatalf("deepseek = %+v", c)
	}
}
