package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agentgo/internal/config"
)

func TestProviderHelpLineListsExpandedProviders(t *testing.T) {
	line := providerHelpLine()
	checks := []string{
		"/provider <name>",
		"anthropic",
		"deepseek",
		"openai-compatible",
		"glm",
		"openrouter",
		"mistral",
	}
	for _, want := range checks {
		if !strings.Contains(line, want) {
			t.Fatalf("provider help line missing %q: %s", want, line)
		}
	}
}

func TestProviderEnvHelpLineListsExpandedEnvVars(t *testing.T) {
	line := providerEnvHelpLine()
	checks := []string{
		"LLM_API_KEY",
		"ANTHROPIC_API_KEY",
		"DEEPSEEK_API_KEY",
		"OPENAI_API_KEY",
		"GLM_API_KEY",
		"OPENROUTER_API_KEY",
		"SILICONFLOW_API_KEY",
		"LLM_BASE_URL",
	}
	for _, want := range checks {
		if !strings.Contains(line, want) {
			t.Fatalf("provider env help line missing %q: %s", want, line)
		}
	}
}

func TestMissingAPIKeyMessageIncludesProviderFirstSetupGuidance(t *testing.T) {
	msg := missingAPIKeyMessage("anthropic")

	checks := []string{
		"先看当前厂商：anthropic",
		"如果你用 Claude / Anthropic",
		"ANTHROPIC_API_KEY",
		"如果你用 DeepSeek",
		"DEEPSEEK_API_KEY",
		"如果你用 OpenAI",
		"OPENAI_API_KEY",
		"GLM / Kimi / Qwen / 豆包 / OpenRouter / 硅基流动 / Groq / Together / Fireworks / xAI",
		"/provider openai-compatible",
		"/base-url <兼容 OpenAI 的接口地址>",
		"例如 GLM",
		"例如 Kimi",
		"例如 Qwen",
		"也可用通用变量：LLM_API_KEY",
		"设置后执行 /config，确认 api_key_set: true。",
	}
	for _, want := range checks {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q\nfull message:\n%s", want, msg)
		}
	}
}

func TestWindowsUTF8NoticeIncludesTerminalGuidance(t *testing.T) {
	notice := windowsUTF8Notice("windows")
	checks := []string{
		"Windows terminal encoding tip / Windows 终端编码提示",
		"UTF-8",
		"Windows Terminal",
		"chcp 65001",
	}
	for _, want := range checks {
		if !strings.Contains(notice, want) {
			t.Fatalf("notice missing %q\nfull notice:\n%s", want, notice)
		}
	}

	if windowsUTF8Notice("linux") != "" {
		t.Fatalf("expected non-windows platforms to skip notice")
	}
}

type stubProviderReloader struct {
	calls []providerReloadCall
	err   error
}

type stubStreamingRunner struct {
	result string
	err    error
}

type providerReloadCall struct {
	provider string
	model    string
	baseURL  string
	apiKey   string
}

func (s *stubProviderReloader) ReloadProvider(provider, model, baseURL, apiKey string) error {
	s.calls = append(s.calls, providerReloadCall{
		provider: provider,
		model:    model,
		baseURL:  baseURL,
		apiKey:   apiKey,
	})
	return s.err
}

func (s stubStreamingRunner) RunWithStream(ctx context.Context, input string, onDelta func(delta string)) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if onDelta != nil {
		onDelta(s.result)
	}
	return s.result, nil
}

func TestApplyProviderConfigChangeReloadsProviderForAPIKey(t *testing.T) {
	reloader := &stubProviderReloader{}
	cfg := &config.Config{Model: "claude-sonnet-4-20250514"}

	if err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Provider.APIKey = "sk-test"
		return nil
	}); err != nil {
		t.Fatalf("applyProviderConfigChange returned error: %v", err)
	}

	if len(reloader.calls) != 1 {
		t.Fatalf("expected 1 reload call, got %d", len(reloader.calls))
	}
	call := reloader.calls[0]
	if call.provider != "anthropic" {
		t.Fatalf("expected anthropic provider, got %q", call.provider)
	}
	if call.apiKey != "sk-test" {
		t.Fatalf("expected api key to be reloaded, got %q", call.apiKey)
	}
}

func TestApplyProviderConfigChangeReturnsReloadError(t *testing.T) {
	wantErr := errors.New("reload failed")
	reloader := &stubProviderReloader{err: wantErr}
	cfg := &config.Config{Model: "claude-sonnet-4-20250514"}

	err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Provider.APIKey = "sk-test"
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected reload error %v, got %v", wantErr, err)
	}
}

func TestRunChatInteractionReturnsVisibleError(t *testing.T) {
	runner := stubStreamingRunner{err: errors.New("api: connection refused")}
	out := runChatInteraction(context.Background(), runner, "hello")
	if !strings.Contains(out, "Request failed: api: connection refused") {
		t.Fatalf("expected visible request failure, got %q", out)
	}
}

func TestRunChatInteractionStreamsAndTerminatesCleanly(t *testing.T) {
	runner := stubStreamingRunner{result: "MOCK_REPLY: hello"}
	out := runChatInteraction(context.Background(), runner, "hello")
	if !strings.Contains(out, "MOCK_REPLY: hello") {
		t.Fatalf("expected streamed reply, got %q", out)
	}
	if !strings.HasSuffix(out, "\r\n\r\n") {
		t.Fatalf("expected trailing line break, got %q", out)
	}
}

func TestApplyProviderConfigChangeTrimsModelAndProviderValues(t *testing.T) {
	reloader := &stubProviderReloader{}
	cfg := &config.Config{}

	err := applyProviderConfigChange(cfg, reloader, func() error {
		cfg.Model = "  deepseek-v4-pro  "
		cfg.Provider.Name = "  deepseek  "
		cfg.Provider.APIKey = "  sk-test  "
		cfg.Provider.BaseURL = "  https://api.deepseek.com  "
		return nil
	})
	if err != nil {
		t.Fatalf("applyProviderConfigChange returned error: %v", err)
	}
	if got, want := cfg.Model, "deepseek-v4-pro"; got != want {
		t.Fatalf("cfg.Model = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.Name, "deepseek"; got != want {
		t.Fatalf("cfg.Provider.Name = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.APIKey, "sk-test"; got != want {
		t.Fatalf("cfg.Provider.APIKey = %q, want %q", got, want)
	}
	if got, want := cfg.Provider.BaseURL, "https://api.deepseek.com"; got != want {
		t.Fatalf("cfg.Provider.BaseURL = %q, want %q", got, want)
	}
	if len(reloader.calls) != 1 {
		t.Fatalf("expected one reload call, got %d", len(reloader.calls))
	}
	call := reloader.calls[0]
	if got, want := call.model, "deepseek-v4-pro"; got != want {
		t.Fatalf("reload model = %q, want %q", got, want)
	}
	if got, want := call.provider, "deepseek"; got != want {
		t.Fatalf("reload provider = %q, want %q", got, want)
	}
	if got, want := call.apiKey, "sk-test"; got != want {
		t.Fatalf("reload api key = %q, want %q", got, want)
	}
	if got, want := call.baseURL, "https://api.deepseek.com"; got != want {
		t.Fatalf("reload baseURL = %q, want %q", got, want)
	}
}
