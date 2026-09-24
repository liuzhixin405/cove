package api

import "testing"

// With several keys configured, only the pool holds them and APIKey stays
// empty, so Validate reported "API key required" and the provider could not
// be used at all.
func TestValidateAcceptsKeyPool(t *testing.T) {
	keys := []string{"k1", "k2"}
	if err := newOpenAICompatProvider(ProviderConfig{Name: "deepseek", APIKeys: keys}).Validate(); err != nil {
		t.Fatalf("openai-compatible with a key pool: %v", err)
	}
	if err := newAnthropicProvider(ProviderConfig{APIKeys: keys}).Validate(); err != nil {
		t.Fatalf("anthropic with a key pool: %v", err)
	}
	if err := newOpenAICompatProvider(ProviderConfig{Name: "deepseek"}).Validate(); err == nil {
		t.Fatal("no key at all must still fail validation")
	}
}
