package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/liuzhixin405/cove/internal/api"
	"github.com/liuzhixin405/cove/internal/fsatomic"
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
	// Debug and Verbose are pointers so "absent" is distinguishable from
	// "false". As plain bools, applyProfile could only ever turn them ON
	// (`if prof.Debug { cfg.Debug = true }`), so a profile written specifically
	// to quieten a noisy global config — "debug": false — did nothing at all.
	Debug        *bool  `json:"debug,omitempty"`
	Verbose      *bool  `json:"verbose,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// The per-turn limits and max_sessions, as in Config. MaxTurnMinutes is a
	// pointer because 0 ("off") is a value a profile may set.
	MaxIterations         int  `json:"max_iterations,omitempty"`
	MaxTurnMinutes        *int `json:"max_turn_minutes,omitempty"`
	SubagentMaxIterations int  `json:"subagent_max_iterations,omitempty"`
	MaxSessions           int  `json:"max_sessions,omitempty"`
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
	// The former "telemetry" key is no longer read (the recorder had no
	// callers and was removed). Old files that still carry it load fine:
	// unknown keys are ignored, and Save keeps them on disk.
	// DoneVerifyCommands, if set, are shell commands (e.g. "go build ./...")
	// run before the engine accepts a model's "no more tool calls" response
	// as actually complete; see internal/engine/verify_gate.go. Off by
	// default; an empty/absent list disables the gate entirely.
	DoneVerifyCommands []string `json:"done_verify_commands,omitempty"`
	// DoneVerifyAuto, when no done_verify_commands are configured, derives a
	// verification command from the project (go.mod -> "go build ./...",
	// Cargo.toml -> "cargo check", a local TypeScript install -> tsc) and runs
	// it only on turns that changed files. nil means on.
	DoneVerifyAuto *bool `json:"done_verify_auto,omitempty"`
	// DoneVerifyTimeoutSeconds bounds each verification command; 0 (the
	// default) keeps 120 s, and 300 s for dotnet and npm commands. A command
	// that runs out of time is reported, not counted as a failure.
	DoneVerifyTimeoutSeconds int `json:"done_verify_timeout_seconds,omitempty"`
	// DoneCheck controls the one-time "is the request fully met?" prompt the
	// engine shows a model that is about to end a turn which changed files:
	// "on", "off", or "auto" (the default, also for an empty or unknown
	// value): only fast-tier models and providers other than anthropic.
	DoneCheck string `json:"done_check,omitempty"`
	// Thinking selects the model's thinking mode on providers that support it
	// ("adaptive" or "disabled"); empty keeps the model's default. Effort
	// ("low", "medium", "high", "xhigh", "max") sets reasoning depth.
	Thinking string `json:"thinking,omitempty"`
	Effort   string `json:"effort,omitempty"`
	// ShowReasoning streams a thinking model's full reasoning into the
	// conversation. Off by default: the status line shows its progress.
	ShowReasoning bool `json:"show_reasoning,omitempty"`
	// DisabledSkills are skills (built-in or otherwise) that are not loaded.
	DisabledSkills []string `json:"disabled_skills,omitempty"`
	// MemoryEmbedding, if set, opts the memory store into blending BM25
	// keyword search with real semantic similarity from a remote embeddings
	// API. Off by default; nil means pure BM25 with zero extra network calls
	// or cost, exactly like before this field existed.
	MemoryEmbedding *MemoryEmbeddingConfig `json:"memory_embedding,omitempty"`
	// ExperimentalTools registers the experimental coordination tools
	// (task/task_*, team_*, send_message, brief, sleep). Off by default:
	// every registered tool costs prompt tokens on every request.
	ExperimentalTools bool `json:"experimental_tools,omitempty"`
	// WebSearch selects the websearch backend ("tavily", "brave" or
	// "duckduckgo") and its API key. A configured provider wins over the
	// environment; its key falls back to the provider's environment variable
	// (TAVILY_API_KEY, BRAVE_API_KEY / BRAVE_SEARCH_API_KEY). Without a
	// provider the environment variables pick the backend as before.
	WebSearch *WebSearchConfig `json:"web_search,omitempty"`
	// MaxSessions is how many saved sessions are kept: older ones are deleted
	// automatically at the end of a turn (the session in use never is).
	// Unset or 0 means DefaultMaxSessions; a negative value turns pruning off.
	MaxSessions int `json:"max_sessions,omitempty"`
	// MaxIterations caps the model calls of one turn. At the cap the
	// interactive shell asks whether to go on; -p stops (--max-turns
	// overrides it there). Unset or <= 0 means DefaultMaxIterations.
	MaxIterations int `json:"max_iterations,omitempty"`
	// MaxTurnMinutes limits how long one turn runs before the shell asks
	// whether to go on (-p stops). 0 or negative turns the limit off, so the
	// key has no omitempty: an explicit 0 must survive a Save.
	MaxTurnMinutes int `json:"max_turn_minutes"`
	// SubagentMaxIterations caps each sub-agent's model calls (agent,
	// execute_plan). Unset or <= 0 means DefaultSubagentMaxIterations.
	SubagentMaxIterations int `json:"subagent_max_iterations,omitempty"`

	// loadedView is the effective config as Load returned it (see rawView).
	// Save writes only the fields that differ from it, so values that came
	// from .cove.json, the active profile or the built-in defaults stay where
	// they came from instead of being copied into ~/.cove/config.json.
	// nil (a Config not built by Load) means "write every field".
	loadedView map[string]json.RawMessage
	// turnMinutesExplicit records that config.json, .cove.json or the active
	// profile set max_turn_minutes (UnattendedTurnMinutes).
	turnMinutesExplicit bool
}

// UnattendedTurnMinutes is the turn time limit for -p and headless runs,
// where nobody can answer the prompt that the interactive shell shows at the
// limit: only a max_turn_minutes the user wrote applies there, the default
// does not (0 = no limit).
func (c *Config) UnattendedTurnMinutes() int {
	if c == nil || !c.turnMinutesExplicit {
		return 0
	}
	return c.MaxTurnMinutes
}

// hasKey reports whether the JSON object in data has a top-level key.
func hasKey(data []byte, key string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

// DoneCheckMode is the effective done_check setting: "on", "off" or "auto".
func (c *Config) DoneCheckMode() string {
	if c != nil {
		switch m := strings.ToLower(strings.TrimSpace(c.DoneCheck)); m {
		case "on", "off":
			return m
		}
	}
	return "auto"
}

// VerifyAutoEnabled reports whether automatic completion verification is on.
func (c *Config) VerifyAutoEnabled() bool {
	return c.DoneVerifyAuto == nil || *c.DoneVerifyAuto
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

// WebSearchConfig configures the websearch tool (config key web_search).
type WebSearchConfig struct {
	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
}

type MCPServerConfig struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
}

// DefaultMaxSessions is the max_sessions default.
const DefaultMaxSessions = 200

// Defaults of the per-turn limits.
const (
	DefaultMaxIterations         = 200
	DefaultMaxTurnMinutes        = 60
	DefaultSubagentMaxIterations = 60
)

func DefaultConfig() *Config {
	return &Config{
		Model:          "claude-sonnet-4-20250514",
		PermissionMode: "default",
		MaxBudgetUsd:   10,
		ThinkingTokens: 16000,
		MaxSessions:    DefaultMaxSessions,

		MaxIterations:         DefaultMaxIterations,
		MaxTurnMinutes:        DefaultMaxTurnMinutes,
		SubagentMaxIterations: DefaultSubagentMaxIterations,
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
	// DefaultConfig's model is Anthropic's. Left in place, a config that only
	// says "provider": {"name": "deepseek"} sent claude-sonnet-4 to DeepSeek and
	// every request failed; empty lets applyDefaults pick the provider's default.
	cfg.Model = ""
	finish := func(err error) (*Config, error) {
		applyDefaults(cfg)
		cfg.loadedView, _ = rawView(cfg)
		return cfg, err
	}
	dir, err := ConfigDir()
	if err == nil {
		p := filepath.Join(dir, "config.json")
		data, err := os.ReadFile(p)
		if err == nil {
			if err := json.Unmarshal(stripBOM(data), cfg); err != nil {
				return finish(fmt.Errorf("parse config %s: %w", p, err))
			}
			cfg.turnMinutesExplicit = hasKey(stripBOM(data), "max_turn_minutes")
		}
	}
	if err := loadProjectOverride(cfg); err != nil {
		return finish(err)
	}
	if profileName == "" {
		profileName = cfg.ActiveProfile
	}
	if profileName != "" {
		prof, ok := cfg.Profiles[profileName]
		if !ok {
			// Used to be ignored, so `cove --profile wrok` quietly ran on the
			// base settings. The base config is still returned and usable.
			return finish(fmt.Errorf("profile %q not found in config", profileName))
		}
		applyProfile(cfg, prof)
	}
	return finish(nil)
}

// utf8BOM is what Windows PowerShell 5.1 (Out-File, Set-Content -Encoding
// utf8) and older Notepad put in front of a UTF-8 file. encoding/json rejects
// it, which made such a config fail to parse and every setting in it ignored.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func stripBOM(data []byte) []byte { return bytes.TrimPrefix(data, utf8BOM) }

// CheckFile reports whether the config file at path would be rejected by
// Load. A missing file is fine: cove runs on defaults and environment keys.
func CheckFile(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var probe Config
	if err := json.Unmarshal(stripBOM(data), &probe); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
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
	if err := json.Unmarshal(stripBOM(data), &override); err != nil {
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
	if override.DoneVerifyAuto != nil {
		cfg.DoneVerifyAuto = override.DoneVerifyAuto
	}
	if override.DoneVerifyTimeoutSeconds > 0 {
		cfg.DoneVerifyTimeoutSeconds = override.DoneVerifyTimeoutSeconds
	}
	if override.DoneCheck != "" {
		cfg.DoneCheck = override.DoneCheck
	}
	if override.Thinking != "" {
		cfg.Thinking = override.Thinking
	}
	if override.Effort != "" {
		cfg.Effort = override.Effort
	}
	if override.ShowReasoning {
		cfg.ShowReasoning = true
	}
	if len(override.DisabledSkills) > 0 {
		cfg.DisabledSkills = override.DisabledSkills
	}
	if override.MemoryEmbedding != nil {
		cfg.MemoryEmbedding = override.MemoryEmbedding
	}
	if override.ExperimentalTools {
		cfg.ExperimentalTools = true
	}
	if override.WebSearch != nil {
		cfg.WebSearch = override.WebSearch
	}
	// Provider and ThinkingTokens were silently dropped here, so a project that
	// pinned its own endpoint or thinking budget in .cove.json was ignored with
	// no message — the user's setting simply had no effect.
	if override.Provider.Name != "" {
		cfg.Provider.Name = override.Provider.Name
	}
	if override.Provider.APIKey != "" {
		cfg.Provider.APIKey = override.Provider.APIKey
	}
	if override.Provider.BaseURL != "" {
		cfg.Provider.BaseURL = override.Provider.BaseURL
	}
	if override.ThinkingTokens > 0 {
		cfg.ThinkingTokens = override.ThinkingTokens
	}
	// The per-turn limits and max_sessions were only read from config.json.
	if override.MaxIterations > 0 {
		cfg.MaxIterations = override.MaxIterations
	}
	if override.SubagentMaxIterations > 0 {
		cfg.SubagentMaxIterations = override.SubagentMaxIterations
	}
	if override.MaxSessions != 0 {
		cfg.MaxSessions = override.MaxSessions
	}
	// max_turn_minutes has no omitempty and 0 means "off": only the key's
	// presence tells an explicit value from an absent one.
	if hasKey(stripBOM(data), "max_turn_minutes") {
		cfg.MaxTurnMinutes = override.MaxTurnMinutes
		cfg.turnMinutesExplicit = true
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
	if prof.Debug != nil {
		cfg.Debug = *prof.Debug
	}
	if prof.Verbose != nil {
		cfg.Verbose = *prof.Verbose
	}
	if prof.SystemPrompt != "" {
		cfg.SystemPrompt = prof.SystemPrompt
	}
	if prof.MaxIterations > 0 {
		cfg.MaxIterations = prof.MaxIterations
	}
	if prof.SubagentMaxIterations > 0 {
		cfg.SubagentMaxIterations = prof.SubagentMaxIterations
	}
	if prof.MaxSessions != 0 {
		cfg.MaxSessions = prof.MaxSessions
	}
	if prof.MaxTurnMinutes != nil {
		cfg.MaxTurnMinutes = *prof.MaxTurnMinutes
		cfg.turnMinutesExplicit = true
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
	if cfg.MaxSessions == 0 {
		cfg.MaxSessions = DefaultMaxSessions
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = DefaultMaxIterations
	}
	if cfg.SubagentMaxIterations <= 0 {
		cfg.SubagentMaxIterations = DefaultSubagentMaxIterations
	}
	// MaxTurnMinutes starts at its default (DefaultConfig) and only an
	// explicit value in a config file changes it; 0 is "off", not "unset".
	if cfg.MaxTurnMinutes < 0 {
		cfg.MaxTurnMinutes = 0
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

// Save writes the settings cfg changed since Load into ~/.cove/config.json.
//
// It used to serialize the whole Config over the file. Because Load merges
// .cove.json, the active profile and the defaults into cfg, one /model in a
// cloned repo copied that repo's base_url, MCP servers and permission mode
// into the global config, the active profile's values leaked into the top
// level, keys this version does not know were dropped, and two cove windows
// reverted each other's changes. Now the file is re-read and only the fields
// that differ from what Load returned are replaced (see loadedView). A file
// that no longer parses is left alone: overwriting it with defaults destroyed
// the user's whole config, API key included.
func Save(cfg *Config) error {
	dir, err := ConfigDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "config.json")

	view, err := rawView(cfg)
	if err != nil {
		return err
	}
	onDisk := map[string]json.RawMessage{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if body := stripBOM(data); len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &onDisk); err != nil {
				return fmt.Errorf("%s is not valid JSON (%w); fix or delete it first, it was not overwritten", path, err)
			}
			if onDisk == nil { // the file said "null"
				onDisk = map[string]json.RawMessage{}
			}
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	mergeChanges(onDisk, cfg.loadedView, view)
	out, err := json.MarshalIndent(onDisk, "", "  ")
	if err != nil {
		return err
	}
	// Atomic replace: a crash mid-write used to leave a truncated config.json.
	// It also applies 0600 every time, where os.WriteFile only did so when it
	// created the file, so a pre-existing 0644 file holding the key stayed so.
	if err := fsatomic.WriteFile(path, out, 0o600); err != nil {
		return err
	}
	cfg.loadedView = view
	return nil
}

// mergeChanges copies into dst every top-level field of cur that differs from
// base, and removes the ones cur no longer has. Keys dst has that neither
// knows about are kept. A nil base (a Config not built by Load) writes all.
func mergeChanges(dst, base, cur map[string]json.RawMessage) {
	for k, v := range cur {
		if old, ok := base[k]; ok && bytes.Equal(old, v) {
			continue
		}
		if k == "provider" {
			// Field by field: a project's base_url must not ride along with
			// a global /api-key change.
			mergeProvider(dst, base[k], v, base == nil)
			continue
		}
		dst[k] = v
	}
	if base == nil {
		return
	}
	for k := range base {
		if _, ok := cur[k]; !ok {
			delete(dst, k)
		}
	}
}

func mergeProvider(dst map[string]json.RawMessage, base, cur json.RawMessage, writeAll bool) {
	var disk, was, now map[string]json.RawMessage
	_ = json.Unmarshal(dst["provider"], &disk)
	if disk == nil {
		disk = map[string]json.RawMessage{}
	}
	_ = json.Unmarshal(base, &was)
	_ = json.Unmarshal(cur, &now)
	for _, k := range []string{"name", "api_key", "base_url"} {
		v, ok := now[k]
		if !writeAll && bytes.Equal(was[k], v) {
			continue
		}
		if ok {
			disk[k] = v
		} else {
			delete(disk, k)
		}
	}
	dst["provider"], _ = json.Marshal(disk)
}

// rawView is cfg as it is written to disk: each field under its JSON name, the
// API keys in full (ProviderConfig.MarshalJSON masks them for display).
func rawView(cfg *Config) (map[string]json.RawMessage, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	// Re-marshal provider using rawProvider  - no masking MarshalJSON.
	providerRaw, err := json.Marshal(rawProvider{
		Name:    cfg.Provider.Name,
		APIKey:  cfg.Provider.APIKey,
		BaseURL: cfg.Provider.BaseURL,
	})
	if err != nil {
		return nil, err
	}
	m["provider"] = providerRaw

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
						return nil, err
					}
					var profProviderVal interface{}
					_ = json.Unmarshal(providerRaw, &profProviderVal)
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
				// Round-trip an explicit false as well, so saving does not
				// quietly discard a profile that deliberately turns these off.
				if prof.Debug != nil {
					profileVal["debug"] = *prof.Debug
				}
				if prof.Verbose != nil {
					profileVal["verbose"] = *prof.Verbose
				}
				if prof.SystemPrompt != "" {
					profileVal["system_prompt"] = prof.SystemPrompt
				}
			}
			profilesRaw[name] = profileVal
		}
		if m["profiles"], err = json.Marshal(profilesRaw); err != nil {
			return nil, err
		}
	}
	return m, nil
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
