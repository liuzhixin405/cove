package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
)

type ProviderConfig struct {
	Name    string   `json:"name"`
	APIKey  string   `json:"api_key,omitempty"`
	APIKeys []string `json:"-"`
	BaseURL string   `json:"base_url,omitempty"`
}

// MarshalJSON masks the API key to prevent leakage in logs/display.
func (p ProviderConfig) MarshalJSON() ([]byte, error) {
	type alias ProviderConfig
	a := alias(p)
	if a.APIKey != "" {
		a.APIKey = maskKey(a.APIKey)
	}
	return json.Marshal(a)
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

type Profile struct {
	Model          string          `json:"model,omitempty"`
	ModelFast      string          `json:"model_fast,omitempty"`
	Provider       *ProviderConfig `json:"provider,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	MaxBudgetUsd   float64         `json:"max_budget_usd,omitempty"`
	ThinkingTokens int             `json:"thinking_tokens,omitempty"`
	Debug          bool            `json:"debug,omitempty"`
	Verbose        bool            `json:"verbose,omitempty"`
	SystemPrompt   string          `json:"system_prompt,omitempty"`
}

// UnmarshalJSON keeps backward compatibility with older configs that used
// profile "mode" instead of "permission_mode".
func (p *Profile) UnmarshalJSON(data []byte) error {
	type alias Profile
	aux := struct {
		alias
		Mode string `json:"mode,omitempty"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*p = Profile(aux.alias)
	if p.PermissionMode == "" && aux.Mode != "" {
		p.PermissionMode = aux.Mode
	}
	return nil
}

type Config struct {
	Model          string                     `json:"model"`
	ModelFast      string                     `json:"model_fast,omitempty"`
	Provider       ProviderConfig             `json:"provider"`
	PermissionMode string                     `json:"permission_mode"`
	MaxBudgetUsd   float64                    `json:"max_budget_usd"`
	ThinkingTokens int                        `json:"thinking_tokens"`
	Debug          bool                       `json:"debug"`
	Verbose        bool                       `json:"verbose"`
	SystemPrompt   string                     `json:"system_prompt,omitempty"`
	MCPServers     map[string]MCPServerConfig `json:"mcp_servers,omitempty"`
	Profiles       map[string]*Profile        `json:"profiles,omitempty"`
	ActiveProfile  string                     `json:"active_profile,omitempty"`
	// Telemetry enables local, opt-in usage recording (~/.cove/telemetry.json).
	// Off by default; can also be enabled with COVE_TELEMETRY=1.
	Telemetry bool `json:"telemetry,omitempty"`
	// DoneVerifyCommands, if set, are shell commands (e.g. "go build ./...")
	// run before the engine accepts a model's "no more tool calls" response
	// as actually complete; see internal/engine/verify_gate.go. Off by
	// default; an empty/absent list disables the gate entirely.
	DoneVerifyCommands []string `json:"done_verify_commands,omitempty"`
	// MemoryEmbedding, if set, opts the memory store into blending BM25
	// keyword search with real semantic similarity from a remote embeddings
	// API. Off by default; nil means pure BM25 with zero extra network calls
	// or cost, exactly like before this field existed.
	MemoryEmbedding *MemoryEmbeddingConfig `json:"memory_embedding,omitempty"`
}

// MemoryEmbeddingConfig configures the optional remote embeddings endpoint
// used for semantic memory search. BaseURL/APIKey default to the main
// provider's values when empty, so in the common case a user who wants
// this only needs to add `"memory_embedding": {}` (or set a model name);
// no separate account or key is needed, reusing what is already configured for chat.
type MemoryEmbeddingConfig struct {
	BaseURL string `json:"base_url,omitempty"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
}

func DefaultConfig() *Config {
	return &Config{
		Model:          "claude-sonnet-4-20250514",
		PermissionMode: "default",
		MaxBudgetUsd:   10,
		ThinkingTokens: 16000,
	}
}

func ConfigDir() (string, error) {
	if d := os.Getenv("COVE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cove"), nil
}

func Load() (*Config, error) {
	return LoadWithProfile("")
}

func LoadWithProfile(profileName string) (*Config, error) {
	cfg := DefaultConfig()
	dir, err := ConfigDir()
	if err == nil {
		p := filepath.Join(dir, "config.json")
		data, err := os.ReadFile(p)
		if err == nil {
			if err := json.Unmarshal(data, cfg); err != nil {
				applyDefaults(cfg)
				return cfg, fmt.Errorf("parse config %s: %w", p, err)
			}
		}
	}
	if err := loadProjectOverride(cfg); err != nil {
		applyDefaults(cfg)
		return cfg, err
	}
	if profileName == "" {
		profileName = cfg.ActiveProfile
	}
	if profileName != "" {
		if prof, ok := cfg.Profiles[profileName]; ok {
			applyProfile(cfg, prof)
		}
	}
	applyDefaults(cfg)
	return cfg, nil
}

func loadProjectOverride(cfg *Config) error {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	p := filepath.Join(cwd, ".cove.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var override Config
	if err := json.Unmarshal(data, &override); err != nil {
		return fmt.Errorf("parse project config %s: %w", p, err)
	}
	if override.Model != "" {
		cfg.Model = override.Model
	}
	if override.ModelFast != "" {
		cfg.ModelFast = override.ModelFast
	}
	if override.PermissionMode != "" {
		cfg.PermissionMode = override.PermissionMode
	}
	if override.MaxBudgetUsd > 0 {
		cfg.MaxBudgetUsd = override.MaxBudgetUsd
	}
	if override.SystemPrompt != "" {
		cfg.SystemPrompt = override.SystemPrompt
	}
	if len(override.MCPServers) > 0 {
		cfg.MCPServers = override.MCPServers
	}
	if len(override.DoneVerifyCommands) > 0 {
		cfg.DoneVerifyCommands = override.DoneVerifyCommands
	}
	if override.MemoryEmbedding != nil {
		cfg.MemoryEmbedding = override.MemoryEmbedding
	}
	return nil
}

func applyProfile(cfg *Config, prof *Profile) {
	if prof == nil {
		return
	}
	if prof.Model != "" {
		cfg.Model = prof.Model
	}
	if prof.ModelFast != "" {
		cfg.ModelFast = prof.ModelFast
	}
	if prof.Provider != nil {
		cfg.Provider = *prof.Provider
	}
	if prof.PermissionMode != "" {
		cfg.PermissionMode = prof.PermissionMode
	}
	if prof.MaxBudgetUsd > 0 {
		cfg.MaxBudgetUsd = prof.MaxBudgetUsd
	}
	if prof.ThinkingTokens > 0 {
		cfg.ThinkingTokens = prof.ThinkingTokens
	}
	if prof.Debug {
		cfg.Debug = true
	}
	if prof.Verbose {
		cfg.Verbose = true
	}
	if prof.SystemPrompt != "" {
		cfg.SystemPrompt = prof.SystemPrompt
	}
}

func applyDefaults(cfg *Config) {
	normalizeConfig(cfg)
	if cfg.Model == "" || strings.EqualFold(cfg.Model, "auto") {
		cfg.Model = DefaultModelForProvider(cfg.Provider.Name)
	}
	if cfg.ModelFast == "" || strings.EqualFold(cfg.ModelFast, "auto") {
		// No fast model configured  - reuse the main model. Routing a "simple"
		// task to the same model is a no-op, which is correct and provider-safe.
		// (Previously this hardcoded deepseek-v4-flash for every provider, which
		// broke routing whenever the active provider wasn't deepseek.)
		cfg.ModelFast = cfg.Model
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = "default"
	}
	if cfg.ThinkingTokens < 1024 {
		cfg.ThinkingTokens = 16000
	}
}

func normalizeConfig(cfg *Config) {
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.PermissionMode = strings.TrimSpace(cfg.PermissionMode)
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.Provider.Name = strings.TrimSpace(cfg.Provider.Name)
	cfg.Provider.APIKey = strings.TrimSpace(cfg.Provider.APIKey)
	cfg.Provider.BaseURL = strings.TrimSpace(cfg.Provider.BaseURL)
	// Clear keys that look masked (contain ****) to force env-var fallback.
	// This heals config files corrupted by earlier versions that saved masked keys.
	if strings.Contains(cfg.Provider.APIKey, "****") {
		cfg.Provider.APIKey = ""
	}
}

func DefaultModelForProvider(providerName string) string {
	switch api.NormalizeProviderName(providerName) {
	case "deepseek":
		return "deepseek-v4-pro"
	case "openai", "openai-compatible":
		return "gpt-4o"
	default:
		return "claude-sonnet-4-20250514"
	}
}

func ResolveModelForProvider(model, providerName string) string {
	model = strings.TrimSpace(model)
	if model == "" || strings.EqualFold(model, "auto") {
		return DefaultModelForProvider(providerName)
	}
	return model
}

func Save(cfg *Config) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	os.MkdirAll(dir, 0700)

	// First marshal triggers ProviderConfig.MarshalJSON (masks key for display).
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	// Unmarshal into map so we can fix the masked api_key.
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	// Re-marshal provider using rawProvider  - no masking MarshalJSON.
	providerRaw, err := json.Marshal(rawProvider{
		Name:    cfg.Provider.Name,
		APIKey:  cfg.Provider.APIKey,
		BaseURL: cfg.Provider.BaseURL,
	})
	if err != nil {
		return err
	}
	var providerVal interface{}
	json.Unmarshal(providerRaw, &providerVal)
	m["provider"] = providerVal

	if len(cfg.Profiles) > 0 {
		profilesRaw := make(map[string]interface{}, len(cfg.Profiles))
		for name, prof := range cfg.Profiles {
			profileVal := map[string]interface{}{}
			if prof != nil {
				if prof.Model != "" {
					profileVal["model"] = prof.Model
				}
				if prof.ModelFast != "" {
					profileVal["model_fast"] = prof.ModelFast
				}
				if prof.Provider != nil {
					providerRaw, err := json.Marshal(rawProvider{
						Name:    prof.Provider.Name,
						APIKey:  prof.Provider.APIKey,
						BaseURL: prof.Provider.BaseURL,
					})
					if err != nil {
						return err
					}
					var profProviderVal interface{}
					json.Unmarshal(providerRaw, &profProviderVal)
					profileVal["provider"] = profProviderVal
				}
				if prof.PermissionMode != "" {
					profileVal["permission_mode"] = prof.PermissionMode
				}
				if prof.MaxBudgetUsd > 0 {
					profileVal["max_budget_usd"] = prof.MaxBudgetUsd
				}
				if prof.ThinkingTokens > 0 {
					profileVal["thinking_tokens"] = prof.ThinkingTokens
				}
				if prof.Debug {
					profileVal["debug"] = prof.Debug
				}
				if prof.Verbose {
					profileVal["verbose"] = prof.Verbose
				}
				if prof.SystemPrompt != "" {
					profileVal["system_prompt"] = prof.SystemPrompt
				}
			}
			profilesRaw[name] = profileVal
		}
		m["profiles"] = profilesRaw
	}

	data, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)
}

// rawProvider mirrors ProviderConfig fields without the masking MarshalJSON method.
// Used by Save to write the full API key to disk.
type rawProvider struct {
	Name    string `json:"name"`
	APIKey  string `json:"api_key,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

func (c *Config) EffectiveProvider() ProviderConfig {
	pc := c.Provider
	if pc.Name == "" {
		pc.Name = "anthropic"
	}
	pc.Name = api.NormalizeProviderName(pc.Name)
	if pc.APIKey == "" {
		pc.APIKey = firstEnv(api.ProviderEnvCandidates(pc.Name)...)
	}
	if pc.BaseURL == "" {
		pc.BaseURL = os.Getenv("LLM_BASE_URL")
	}
	if pc.BaseURL == "" && api.IsOpenAICompatibleProvider(pc.Name) {
		pc.BaseURL = api.DefaultBaseURL(pc.Name)
	}
	return pc
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
