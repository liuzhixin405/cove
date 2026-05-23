package config

import (
	"os"
	"testing"
)

func TestEffectiveProviderReadsExpandedProviderEnvVars(t *testing.T) {
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("GLM_API_KEY", "glm-key")
	t.Setenv("KIMI_API_KEY", "")
	t.Setenv("QWEN_API_KEY", "")
	t.Setenv("DASHSCOPE_API_KEY", "")
	t.Setenv("DOUBAO_API_KEY", "")
	t.Setenv("ARK_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("SILICONFLOW_API_KEY", "")

	cfg := &Config{Provider: ProviderConfig{Name: "glm"}}
	pc := cfg.EffectiveProvider()
	if pc.APIKey != "glm-key" {
		t.Fatalf("expected GLM_API_KEY to be picked up, got %q", pc.APIKey)
	}
}

func TestEffectiveProviderFallsBackToLLMBaseURL(t *testing.T) {
	t.Setenv("LLM_BASE_URL", "https://example.com/v1")
	cfg := &Config{Provider: ProviderConfig{Name: "openai-compatible"}}
	pc := cfg.EffectiveProvider()
	if pc.BaseURL != "https://example.com/v1" {
		t.Fatalf("base url = %q, want env fallback", pc.BaseURL)
	}
}

func TestFirstEnvSkipsEmptyValues(t *testing.T) {
	os.Setenv("TEST_EMPTY_A", "")
	os.Setenv("TEST_EMPTY_B", "real")
	defer os.Unsetenv("TEST_EMPTY_A")
	defer os.Unsetenv("TEST_EMPTY_B")
	if got := firstEnv("TEST_EMPTY_A", "TEST_EMPTY_B"); got != "real" {
		t.Fatalf("firstEnv returned %q", got)
	}
}
