//! Configuration: CLI arguments, the user config and profiles, the project `.cove.json`, and project trust.

pub mod trust;

use std::{
    collections::BTreeMap,
    io::Read,
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::Context;
use clap::Parser;
use serde::Deserialize;
use serde_json::Value;

use crate::safety::permission::Mode;

/// Wire protocol; each named provider maps onto one of these.
#[derive(Clone, Copy, Debug)]
pub enum Protocol {
    Openai,
    Anthropic,
}

#[derive(Clone, Parser)]
#[command(version, about = "Cove Rust preview: a permission-aware coding agent")]
pub struct Cli {
    /// One-shot prompt; piped stdin is appended (`cat log | cove -p "explain"`). Never reads approval answers from stdin.
    #[arg(short = 'p', long, conflicts_with = "prompt", num_args = 0..=1, default_missing_value = "", value_name = "PROMPT")]
    pub print: Option<String>,
    /// Single task with local approval prompts when attached to a terminal.
    pub prompt: Option<String>,
    /// Open a saved session (must match cwd, provider, model and token prices).
    #[arg(short = 'r', long)]
    pub resume: Option<String>,
    /// Attach a text file to the -p prompt (repeatable; at most 256 KiB each).
    #[arg(long = "file", value_name = "PATH", requires = "print")]
    pub files: Vec<PathBuf>,
    /// Attach an image to the -p prompt (repeatable).
    #[arg(long = "image", value_name = "PATH", requires = "print")]
    pub images: Vec<PathBuf>,
    /// Print the system prompt this invocation would use, then exit.
    #[arg(long)]
    pub dump_system_prompt: bool,
    /// Outbound proxy URL (http://, https://, socks5://, socks5h://), `system` or `none`;
    /// config `proxy` [default: system proxy and HTTPS_PROXY/ALL_PROXY].
    #[arg(long, env = "COVE_PROXY")]
    pub proxy: Option<String>,
    /// Echo trace events (model calls, tools, compactions) to stderr; config `debug`.
    #[arg(short = 'd', long)]
    pub debug: bool,
    /// Show complete tool results instead of their first lines; config `verbose`.
    #[arg(long)]
    pub verbose: bool,
    /// Run the diagnostic checks (config, key, network, tools, data dir) and exit.
    #[arg(long)]
    pub doctor: bool,
    /// Continue an interrupted task once, without interactive approval input.
    #[arg(long = "continue", requires = "resume", conflicts_with_all = ["print", "prompt"])]
    pub continue_task: bool,
    /// Private session/checkpoint directory, outside the workspace.
    #[arg(long, env = "COVE_DATA_DIR")]
    pub data_dir: Option<PathBuf>,
    /// Model context window in tokens [default: from the model table, else 32000].
    #[arg(long)]
    pub context_window: Option<usize>,
    /// Legacy context limit in bytes (about 3 per token).
    #[arg(long, hide = true)]
    pub context_bytes: Option<usize>,
    /// JSON config [default: <data-dir>/config.json when present]. Project config is never auto-loaded.
    #[arg(long, env = "COVE_CONFIG")]
    pub config: Option<PathBuf>,
    /// Profile from the config's `profiles`; overrides `active_profile`.
    #[arg(long, env = "COVE_PROFILE")]
    pub profile: Option<String>,
    /// Permit configured MCP servers/hooks to request process authorization.
    #[arg(long)]
    pub trust_extensions: bool,
    /// Provider name (openai, anthropic, deepseek, glm, kimi, qwen, ...); replaces the configured provider.
    #[arg(long, env = "COVE_PROVIDER")]
    pub provider: Option<String>,
    #[arg(long, env = "COVE_MODEL")]
    pub model: Option<String>,
    /// API root including /v1 when needed, not the final endpoint.
    #[arg(long, env = "COVE_BASE_URL")]
    pub base_url: Option<String>,
    #[arg(long, default_value = ".", value_name = "DIRECTORY")]
    pub cwd: PathBuf,
    /// default asks; plan is read-only; auto also allows build/test and in-project edits; bypass allows all but deny rules [default: default].
    #[arg(long, value_enum)]
    pub permission: Option<Mode>,
    /// Model requests per task [default: 20].
    #[arg(long)]
    pub max_turns: Option<u32>,
    /// Task time limit in seconds [default: 300].
    #[arg(long)]
    pub timeout: Option<u64>,
    /// [default: 4096]
    #[arg(long)]
    pub max_output_tokens: Option<u32>,
    /// Estimated USD cap; requires both prices below (per million tokens).
    #[arg(long)]
    pub budget: Option<f64>,
    #[arg(long)]
    pub input_price: Option<f64>,
    #[arg(long)]
    pub output_price: Option<f64>,
    /// List saved sessions of this workspace (`all`: every workspace) and exit.
    #[arg(long, value_name = "all", num_args = 0..=1, default_missing_value = "project")]
    pub list_sessions: Option<String>,
    /// No background memory extraction or automatic dream consolidation.
    #[arg(long)]
    pub no_auto: bool,
}

struct ProviderInfo {
    name: &'static str,
    protocol: Protocol,
    base_url: &'static str,
    env: &'static [&'static str],
}

/// Provider names, default API roots and their key environment variables.
const PROVIDERS: &[ProviderInfo] = &[
    p(
        "anthropic",
        Protocol::Anthropic,
        "https://api.anthropic.com/v1",
        &["ANTHROPIC_API_KEY"],
    ),
    p(
        "bedrock",
        Protocol::Anthropic,
        "",
        &["AWS_BEARER_TOKEN_BEDROCK"],
    ),
    p(
        "deepseek",
        Protocol::Openai,
        "https://api.deepseek.com/v1",
        &["DEEPSEEK_API_KEY"],
    ),
    p(
        "openai",
        Protocol::Openai,
        "https://api.openai.com/v1",
        &["OPENAI_API_KEY"],
    ),
    p(
        "openai-compatible",
        Protocol::Openai,
        "https://api.openai.com/v1",
        &["OPENAI_API_KEY"],
    ),
    p(
        "glm",
        Protocol::Openai,
        "https://open.bigmodel.cn/api/paas/v4",
        &["GLM_API_KEY", "ZHIPU_API_KEY", "BIGMODEL_API_KEY"],
    ),
    p(
        "kimi",
        Protocol::Openai,
        "https://api.moonshot.cn/v1",
        &["KIMI_API_KEY", "MOONSHOT_API_KEY"],
    ),
    p(
        "qwen",
        Protocol::Openai,
        "https://dashscope.aliyuncs.com/compatible-mode/v1",
        &["QWEN_API_KEY", "DASHSCOPE_API_KEY"],
    ),
    p(
        "doubao",
        Protocol::Openai,
        "https://ark.cn-beijing.volces.com/api/v3",
        &["DOUBAO_API_KEY", "ARK_API_KEY", "VOLCENGINE_API_KEY"],
    ),
    p(
        "openrouter",
        Protocol::Openai,
        "https://openrouter.ai/api/v1",
        &["OPENROUTER_API_KEY"],
    ),
    p(
        "siliconflow",
        Protocol::Openai,
        "https://api.siliconflow.cn/v1",
        &["SILICONFLOW_API_KEY"],
    ),
    p(
        "groq",
        Protocol::Openai,
        "https://api.groq.com/openai/v1",
        &["GROQ_API_KEY"],
    ),
    p(
        "together",
        Protocol::Openai,
        "https://api.together.xyz/v1",
        &["TOGETHER_API_KEY"],
    ),
    p(
        "fireworks",
        Protocol::Openai,
        "https://api.fireworks.ai/inference/v1",
        &["FIREWORKS_API_KEY"],
    ),
    p(
        "xai",
        Protocol::Openai,
        "https://api.x.ai/v1",
        &["XAI_API_KEY", "GROK_API_KEY"],
    ),
    p(
        "mistral",
        Protocol::Openai,
        "https://api.mistral.ai/v1",
        &["MISTRAL_API_KEY"],
    ),
];

const fn p(
    name: &'static str,
    protocol: Protocol,
    base_url: &'static str,
    env: &'static [&'static str],
) -> ProviderInfo {
    ProviderInfo {
        name,
        protocol,
        base_url,
        env,
    }
}

const ALIASES: &[(&str, &str)] = &[
    ("claude", "anthropic"),
    ("openai_compatible", "openai-compatible"),
    ("compatible", "openai-compatible"),
    ("zhipu", "glm"),
    ("bigmodel", "glm"),
    ("moonshot", "kimi"),
    ("dashscope", "qwen"),
    ("tongyi", "qwen"),
    ("ark", "doubao"),
    ("grok", "xai"),
    ("aws", "bedrock"),
    ("aws-bedrock", "bedrock"),
    ("amazon-bedrock", "bedrock"),
];

/// Model or credentials are missing; an interactive start can offer the setup wizard.
#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct NeedsSetup(pub String);

/// Default models of the providers that have one; other providers need an explicit model.
pub fn default_model(provider: &str) -> Option<&'static str> {
    match provider {
        "deepseek" => Some("deepseek-v4-pro"),
        "openai" | "openai-compatible" => Some("gpt-4o"),
        "anthropic" => Some("claude-sonnet-4-20250514"),
        "bedrock" => Some("anthropic.claude-sonnet-5"),
        _ => None,
    }
}

/// Catalog entry for the setup wizard: canonical name, default API root and key variables.
pub fn provider_defaults(name: &str) -> Option<(&'static str, String, &'static [&'static str])> {
    provider_info(name).map(|info| (info.name, default_base_url(info), info.env))
}

/// Wire protocol of a catalog provider; unknown names speak the OpenAI protocol.
pub fn provider_protocol(name: &str) -> Protocol {
    provider_info(name).map_or(Protocol::Openai, |info| info.protocol)
}

pub fn data_dir(cli: &Cli) -> anyhow::Result<PathBuf> {
    match &cli.data_dir {
        Some(path) => Ok(path.clone()),
        None => {
            let home = std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
                .ok_or_else(|| {
                    anyhow::anyhow!(
                        "请用 --data-dir 指定数据目录，或设置主目录环境变量（USERPROFILE/HOME）"
                    )
                })?;
            Ok(PathBuf::from(home).join(".cove"))
        }
    }
}

fn provider_info(name: &str) -> Option<&'static ProviderInfo> {
    let name = name.trim().to_ascii_lowercase();
    let name = ALIASES
        .iter()
        .find(|(alias, _)| *alias == name)
        .map_or(name.as_str(), |(_, canonical)| canonical);
    PROVIDERS.iter().find(|info| info.name == name)
}

fn default_base_url(info: &ProviderInfo) -> String {
    if info.name != "bedrock" {
        return info.base_url.into();
    }
    let region = ["AWS_REGION", "AWS_DEFAULT_REGION"]
        .iter()
        .filter_map(|key| std::env::var(key).ok())
        .map(|value| value.trim().to_owned())
        .find(|value| !value.is_empty())
        .unwrap_or_else(|| "us-east-1".into());
    // The native runtime API, as Claude Code uses it; the Anthropic-compatible
    // `bedrock-mantle` endpoint rejects callers by region and stays available via base_url.
    format!("https://bedrock-runtime.{region}.amazonaws.com")
}

/// Accepts a pasted full endpoint; Anthropic roots get the `/v1` the SDKs append.
pub fn normalize_base_url(raw: &str, protocol: Protocol) -> String {
    let url = raw.trim().trim_end_matches('/');
    match protocol {
        Protocol::Openai => {
            let mut url = url
                .strip_suffix("/chat/completions")
                .unwrap_or(url)
                .trim_end_matches('/');
            while let Some(shorter) = url.strip_suffix("/v1/v1") {
                url = &url[..shorter.len() + 3];
            }
            url.into()
        }
        Protocol::Anthropic => {
            let url = url
                .strip_suffix("/messages")
                .unwrap_or(url)
                .trim_end_matches('/');
            if url.ends_with("/v1") {
                url.into()
            } else {
                format!("{url}/v1")
            }
        }
    }
}

#[derive(Clone, Default, Deserialize)]
struct ProviderConfig {
    #[serde(default)]
    name: String,
    api_key: Option<String>,
    /// Several keys of one account or team, used round-robin.
    api_keys: Option<Vec<String>>,
    base_url: Option<String>,
}

#[derive(Clone, Deserialize)]
#[serde(untagged)]
enum ProviderField {
    Name(String),
    Full(ProviderConfig),
}

impl From<ProviderField> for ProviderConfig {
    fn from(field: ProviderField) -> Self {
        match field {
            ProviderField::Name(name) => Self {
                name,
                ..Self::default()
            },
            ProviderField::Full(config) => config,
        }
    }
}

/// Model settings, shared by the config root and each profile.
#[derive(Clone, Default, Deserialize)]
struct Profile {
    model: Option<String>,
    provider: Option<ProviderField>,
    #[serde(alias = "mode")]
    permission_mode: Option<String>,
    max_budget_usd: Option<f64>,
    max_iterations: Option<u32>,
    max_turn_minutes: Option<u64>,
    subagent_max_iterations: Option<u32>,
    system_prompt: Option<String>,
    max_output_tokens: Option<u32>,
    context_window: Option<usize>,
    context_bytes: Option<usize>,
    input_price: Option<f64>,
    output_price: Option<f64>,
    model_fast: Option<String>,
    thinking: Option<String>,
    effort: Option<String>,
    show_reasoning: Option<bool>,
}

impl Profile {
    /// Fields set in `self` win over `base`; a provider block replaces the whole block.
    fn over(self, base: Self) -> Self {
        Self {
            model: self.model.or(base.model),
            provider: self.provider.or(base.provider),
            permission_mode: self.permission_mode.or(base.permission_mode),
            max_budget_usd: self.max_budget_usd.or(base.max_budget_usd),
            max_iterations: self.max_iterations.or(base.max_iterations),
            max_turn_minutes: self.max_turn_minutes.or(base.max_turn_minutes),
            subagent_max_iterations: self
                .subagent_max_iterations
                .or(base.subagent_max_iterations),
            system_prompt: self.system_prompt.or(base.system_prompt),
            max_output_tokens: self.max_output_tokens.or(base.max_output_tokens),
            context_window: self.context_window.or(base.context_window),
            context_bytes: self.context_bytes.or(base.context_bytes),
            input_price: self.input_price.or(base.input_price),
            output_price: self.output_price.or(base.output_price),
            model_fast: self.model_fast.or(base.model_fast),
            thinking: self.thinking.or(base.thinking),
            effort: self.effort.or(base.effort),
            show_reasoning: self.show_reasoning.or(base.show_reasoning),
        }
    }
}

const PROFILE_KEYS: &[&str] = &[
    "model",
    "provider",
    "permission_mode",
    "mode",
    "max_budget_usd",
    "max_iterations",
    "max_turn_minutes",
    "subagent_max_iterations",
    "system_prompt",
    "max_output_tokens",
    "context_window",
    "context_bytes",
    "input_price",
    "output_price",
    "model_fast",
    "thinking",
    "effort",
    "show_reasoning",
    "debug",
    "verbose",
];
/// Retired keys, accepted silently so an older config stays quiet.
const LEGACY_KEYS: &[&str] = &["telemetry", "thinking_tokens"];
const ROOT_KEYS: &[&str] = &[
    "active_profile",
    "profiles",
    "model_context_windows",
    "extensions",
    "done_verify_commands",
    "done_verify_auto",
    "done_verify_timeout_seconds",
    "web_search",
    "proxy",
    "mcp_servers",
    "disabled_skills",
    "skills_registry",
    "skill_review",
    "browser_path",
    "shell",
    "max_sessions",
    "memory_embedding",
    "agent_types",
    "experimental_tools",
    "regression_command",
    "max_background_agents",
    // Read by `gates` from the effective config JSON.
    "done_check",
    "done_verify_tests",
    "done_self_review",
    "workflow",
];
const PROVIDER_KEYS: &[&str] = &["name", "api_key", "api_keys", "base_url"];

#[derive(Default, Deserialize)]
struct FileConfig {
    #[serde(flatten)]
    root: Profile,
    active_profile: Option<String>,
    #[serde(default)]
    profiles: BTreeMap<String, Profile>,
    #[serde(default)]
    extensions: crate::extensions::ExtensionConfig,
    /// Replaces automatic detection; an empty list turns the gate off.
    done_verify_commands: Option<Vec<String>>,
    done_verify_auto: Option<bool>,
    done_verify_timeout_seconds: Option<u64>,
    web_search: Option<WebSearchConfig>,
    /// Windows learned from servers' overflow errors (lower-case model -> tokens).
    #[serde(default)]
    model_context_windows: BTreeMap<String, usize>,
}

#[derive(Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WebSearchConfig {
    /// tavily, brave or duckduckgo; unset picks by available key.
    pub provider: Option<String>,
    pub api_key: Option<String>,
}

impl FileConfig {
    fn read(path: &Path, warnings: &mut Vec<String>) -> anyhow::Result<(Self, Value)> {
        Self::parse(read_json_file(path)?, path, warnings)
    }

    fn parse(
        value: Value,
        path: &Path,
        warnings: &mut Vec<String>,
    ) -> anyhow::Result<(Self, Value)> {
        anyhow::ensure!(
            value.get("base_url").is_none(),
            "config：请把顶层的 \"base_url\" 移到 \"provider\" 中：{{\"base_url\": ...}}"
        );
        let unknown = unknown_keys(&value);
        if !unknown.is_empty() {
            warnings.push(format!(
                "config：已忽略 cove 不支持的键：{}",
                unknown.join(", ")
            ));
        }
        if has_api_key(&value) {
            warn_if_shared(path, warnings);
        }
        let parsed = serde_json::from_value(value.clone())
            .with_context(|| format!("解析配置文件 {} 失败", path.display()))?;
        Ok((parsed, value))
    }

    /// Root values overlaid by the selected profile.
    fn resolve(&mut self, requested: Option<String>) -> anyhow::Result<(Option<String>, Profile)> {
        let root = std::mem::take(&mut self.root);
        let Some(name) = requested
            .or_else(|| self.active_profile.clone())
            .filter(|name| !name.trim().is_empty())
        else {
            return Ok((None, root));
        };
        let profile = self.profiles.remove(&name).ok_or_else(|| {
            let known: Vec<_> = self.profiles.keys().map(String::as_str).collect();
            anyhow::anyhow!(
                "配置中没有 profile {name:?}；已配置的有：[{}]",
                known.join(", ")
            )
        })?;
        Ok((Some(name), profile.over(root)))
    }
}

/// The user config as JSON (64 KiB cap, UTF-8 BOM tolerated).
pub fn read_json_file(path: &Path) -> anyhow::Result<Value> {
    let mut data = Vec::new();
    std::fs::File::open(path)
        .with_context(|| format!("打开配置文件 {} 失败", path.display()))?
        .take(64 * 1024 + 1)
        .read_to_end(&mut data)?;
    anyhow::ensure!(data.len() <= 64 * 1024, "配置文件超过 64 KiB");
    // Windows PowerShell 5.1 writes UTF-8 with a BOM.
    let data = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data);
    serde_json::from_slice(data).with_context(|| format!("解析配置文件 {} 失败", path.display()))
}

/// Writes the user config atomically (owner-only on Unix: it may hold API keys).
pub fn write_json_file(path: &Path, value: &Value) -> anyhow::Result<()> {
    let parent = path
        .parent()
        .filter(|parent| !parent.as_os_str().is_empty())
        .unwrap_or(Path::new("."));
    std::fs::create_dir_all(parent)?;
    let mut temporary = tempfile::NamedTempFile::new_in(parent)?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        temporary
            .as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o600))?;
    }
    use std::io::Write;
    temporary.write_all(serde_json::to_string_pretty(value)?.as_bytes())?;
    temporary.as_file().sync_all()?;
    crate::session::replace_with(temporary, path)?;
    Ok(())
}

/// Comma-separated keys from one variable or config string.
fn split_keys(raw: &str) -> Vec<String> {
    raw.split(',')
        .map(|key| key.trim().to_owned())
        .filter(|key| !key.is_empty())
        .collect()
}

fn unknown_keys(value: &Value) -> Vec<String> {
    fn object_keys(value: &Value, prefix: &str, allowed: &[&str], out: &mut Vec<String>) {
        let Some(object) = value.as_object() else {
            return;
        };
        for key in object
            .keys()
            .filter(|key| !allowed.contains(&key.as_str()) && !LEGACY_KEYS.contains(&key.as_str()))
        {
            out.push(format!("{prefix}{key}"));
        }
        if let Some(provider) = object.get("provider") {
            object_keys(provider, &format!("{prefix}provider."), PROVIDER_KEYS, out);
        }
    }
    let mut out = Vec::new();
    let root: Vec<&str> = PROFILE_KEYS.iter().chain(ROOT_KEYS).copied().collect();
    object_keys(value, "", &root, &mut out);
    if let Some(profiles) = value.get("profiles").and_then(Value::as_object) {
        for (name, profile) in profiles {
            object_keys(
                profile,
                &format!("profiles.{name}."),
                PROFILE_KEYS,
                &mut out,
            );
        }
    }
    out
}

fn has_api_key(value: &Value) -> bool {
    let set = |profile: &Value| {
        profile
            .pointer("/provider/api_key")
            .and_then(Value::as_str)
            .is_some_and(|key| !key.trim().is_empty())
            || profile
                .pointer("/provider/api_keys")
                .and_then(Value::as_array)
                .is_some_and(|keys| !keys.is_empty())
    };
    set(value)
        || value
            .get("profiles")
            .and_then(Value::as_object)
            .is_some_and(|profiles| profiles.values().any(set))
}

#[cfg(unix)]
fn warn_if_shared(path: &Path, warnings: &mut Vec<String>) {
    use std::os::unix::fs::PermissionsExt;
    if std::fs::metadata(path).is_ok_and(|meta| meta.permissions().mode() & 0o077 != 0) {
        warnings.push(format!(
            "config：{} 含有 API key，但其他用户可读；请运行 chmod 600",
            path.display()
        ));
    }
}

#[cfg(not(unix))]
fn warn_if_shared(_: &Path, _: &mut Vec<String>) {}

/// Permission modes; the older names `ask` and `readonly` stay accepted.
fn parse_mode(raw: &str) -> anyhow::Result<Mode> {
    Mode::parse(raw).ok_or_else(|| {
        anyhow::anyhow!("未知的 permission_mode {raw:?}；可用值：default、plan、auto、bypass")
    })
}

/// A boolean config key of the effective config (root overlaid by the profile).
fn flag(file: &Value, key: &str) -> bool {
    file.get(key).and_then(Value::as_bool).unwrap_or(false)
}

/// The config object as modules see it: root keys overlaid by the selected profile's keys.
fn effective_json(raw: &Value, profile: Option<&str>) -> Value {
    let mut merged = raw.as_object().cloned().unwrap_or_default();
    merged.remove("profiles");
    if let Some(Value::Object(keys)) = profile.and_then(|name| {
        raw.pointer(&format!(
            "/profiles/{}",
            name.replace('~', "~0").replace('/', "~1")
        ))
    }) {
        for (key, value) in keys {
            merged.insert(key.clone(), value.clone());
        }
    }
    Value::Object(merged)
}

pub struct Settings {
    /// Effective config JSON (root + selected profile) for modules that read their own keys.
    /// Keys read this way must also be listed in `ROOT_KEYS`/`PROFILE_KEYS`.
    pub file: Value,
    pub extensions: crate::extensions::ExtensionConfig,
    pub extension_base: PathBuf,
    /// `--trust-extensions`, or the project was trusted with `/config trust`.
    pub trust_extensions: bool,
    /// The cwd's `.cove.json` as loaded (path, hash, applied/ignored keys).
    pub project_config: Option<crate::config::trust::ProjectConfig>,
    /// Config file actually loaded and selected profile, for diagnostics.
    pub config_path: Option<PathBuf>,
    pub profile: Option<String>,
    pub warnings: Vec<String>,
    pub provider: String,
    pub protocol: Protocol,
    pub model: String,
    pub base_url: String,
    pub api_key: String,
    /// Every usable key, `api_key` first; more than one forms a round-robin pool.
    pub api_keys: Vec<String>,
    /// Where the key came from (an environment variable or the config), for `/status usage`.
    pub key_source: String,
    /// Cheaper model for simple tasks and background calls (`model_fast`).
    pub model_fast: Option<String>,
    pub thinking: Option<crate::model::provider::Thinking>,
    pub effort: Option<String>,
    /// Print reasoning text live instead of hiding it.
    pub show_reasoning: bool,
    /// The command line this run started with, for reloading after live changes.
    pub cli: Cli,
    pub instructions: Option<String>,
    pub cwd: PathBuf,
    pub prompt: Option<String>,
    pub resume: Option<String>,
    pub continue_task: bool,
    pub one_shot: bool,
    pub data_dir: PathBuf,
    /// Model window and the compaction budget derived from it, in estimated tokens.
    pub context_window: usize,
    pub context_tokens: usize,
    pub permission: Mode,
    pub interactive: bool,
    pub max_turns: u32,
    pub timeout: Duration,
    pub max_output_tokens: u32,
    pub budget: Option<f64>,
    pub prices: Option<(f64, f64)>,
    /// Commands run when a task that changed files ends; empty disables the gate.
    pub verify_commands: Vec<String>,
    pub verify_timeout: Option<Duration>,
    pub web_search: WebSearchConfig,
    pub subagent_max_turns: u32,
    /// `--dump-system-prompt`: print the assembled system prompt and exit.
    pub dump_system_prompt: bool,
    /// `--no-auto`: no background memory extraction or automatic dreams.
    pub no_auto: bool,
    /// `debug` / `-d`: trace events echoed to stderr.
    pub debug: bool,
    /// `verbose`: complete tool results in the terminal.
    pub verbose: bool,
}

impl Settings {
    pub fn load(cli: Cli) -> anyhow::Result<Self> {
        let raw = match Self::config_file(&cli)? {
            Some(path) => Some(read_json_file(&path)?),
            None => None,
        };
        Self::load_with(cli, raw)
    }

    /// The user config file in use: `--config`, else `<data-dir>/config.json` when present.
    pub fn config_file(cli: &Cli) -> anyhow::Result<Option<PathBuf>> {
        Ok(match &cli.config {
            Some(path) => Some(path.clone()),
            None => Some(data_dir(cli)?.join("config.json")).filter(|path| path.is_file()),
        })
    }

    /// Like `load`, with `raw` as the config file content; lets a live change be
    /// validated before it is written.
    pub fn load_with(cli: Cli, raw: Option<Value>) -> anyhow::Result<Self> {
        let original = cli.clone();
        let data_dir = data_dir(&cli)?;
        let config_path = match (Self::config_file(&cli)?, &raw) {
            (Some(path), _) => Some(path),
            (None, Some(_)) => Some(data_dir.join("config.json")),
            (None, None) => None,
        };
        let mut warnings = Vec::new();
        let (mut file, mut raw) = match (&config_path, raw) {
            (Some(path), Some(value)) => FileConfig::parse(value, path, &mut warnings)?,
            (Some(path), None) => FileConfig::read(path, &mut warnings)?,
            _ => (FileConfig::default(), Value::Object(Default::default())),
        };
        crate::config::trust::set_data_dir(&data_dir);
        // The cwd's .cove.json: safe keys always, sensitive ones only when trusted.
        let project_config = cli.cwd.canonicalize().ok().and_then(|cwd| {
            crate::config::trust::apply_project_config(&mut raw, &cwd, &data_dir, &mut warnings)
        });
        if project_config
            .as_ref()
            .is_some_and(|c| !c.applied.is_empty())
        {
            file =
                serde_json::from_value(raw.clone()).context("parse project config .cove.json")?;
        }
        anyhow::ensure!(
            cli.profile.is_none() || config_path.is_some(),
            "--profile requires a config file"
        );
        let (profile, config) = file.resolve(cli.profile)?;
        let file_json = effective_json(&raw, profile.as_deref());
        let debug = cli.debug || flag(&file_json, "debug");
        let proxy = cli
            .proxy
            .clone()
            .or_else(|| {
                file_json
                    .get("proxy")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
            })
            .unwrap_or_default();
        crate::model::net::configure(
            crate::model::net::parse(&proxy).map_err(|err| anyhow::anyhow!("proxy: {err}"))?,
        );
        let verbose = cli.verbose || flag(&file_json, "verbose");

        // A command-line provider replaces the configured block, so a key or
        // root meant for another provider is never sent to it.
        // The configured fast model belongs to the configured provider, not a command-line one.
        let provider_overridden = cli.provider.is_some();
        let provider = match cli.provider {
            Some(name) => ProviderConfig {
                name,
                ..ProviderConfig::default()
            },
            None => config.provider.map(Into::into).unwrap_or_default(),
        };
        let provider_name = match provider.name.trim() {
            "" => "openai-compatible".to_owned(),
            name => name.to_ascii_lowercase(),
        };
        let info = provider_info(&provider_name);
        let protocol = info.map_or(Protocol::Openai, |info| info.protocol);
        let base_url = match cli
            .base_url
            .or(provider.base_url)
            .filter(|url| !url.trim().is_empty())
        {
            Some(url) => normalize_base_url(&url, protocol),
            None => match info {
                Some(info) => default_base_url(info),
                None => anyhow::bail!(
                    "未知的 provider {provider_name:?} 需要设置 \"base_url\"；已知的有：{}",
                    PROVIDERS
                        .iter()
                        .map(|info| info.name)
                        .collect::<Vec<_>>()
                        .join(", ")
                ),
            },
        };
        let model = cli
            .model
            .or(config.model)
            .map(|model| model.trim().to_owned())
            .filter(|model| !model.is_empty() && !model.eq_ignore_ascii_case("auto"))
            .or_else(|| default_model(info?.name).map(String::from))
            .ok_or_else(|| {
                NeedsSetup("请设置 --model、COVE_MODEL 或配置中的 \"model\"".into())
            })?;
        anyhow::ensure!(!model.is_empty(), "model 不能为空");

        // COVE_API_KEY > provider.api_key > provider variables. Keys masked
        // by an earlier save ("sk-1****abcd") are never sent.
        let env_names: Vec<&str> = info
            .map_or(&["OPENAI_API_KEY"][..], |info| info.env)
            .to_vec();
        // Each source may hold several comma-separated keys (a key pool).
        let unmasked = |keys: Vec<String>| -> Vec<String> {
            let mut unique = Vec::new();
            for key in keys {
                if !key.contains("****") && !unique.contains(&key) {
                    unique.push(key);
                }
            }
            unique
        };
        let configured = {
            let mut keys = provider
                .api_key
                .as_deref()
                .map(split_keys)
                .unwrap_or_default();
            keys.extend(provider.api_keys.clone().unwrap_or_default());
            unmasked(keys.into_iter().map(|key| key.trim().to_owned()).collect())
        };
        let api_keys = std::env::var("COVE_API_KEY")
            .ok()
            .map(|keys| split_keys(&keys))
            .filter(|keys| !keys.is_empty())
            .map(|keys| (keys, "COVE_API_KEY".to_owned()))
            .or_else(|| (!configured.is_empty()).then(|| (configured, "config".to_owned())))
            .or_else(|| {
                env_names.iter().find_map(|name| {
                    let keys = unmasked(split_keys(&std::env::var(name).ok()?));
                    (!keys.is_empty()).then(|| (keys, (*name).to_owned()))
                })
            })
            .ok_or_else(|| {
                NeedsSetup(format!(
                    "请设置 COVE_API_KEY、{} 或配置中的 \"provider.api_key\"",
                    env_names.join(", ")
                ))
            })?;
        let (api_keys, key_source) = api_keys;
        let api_key = api_keys[0].clone();
        let thinking = match config.thinking.as_deref().map(str::trim) {
            None | Some("") => None,
            Some(value) => Some(match value.to_ascii_lowercase().as_str() {
                "adaptive" | "enabled" | "on" | "auto" => {
                    crate::model::provider::Thinking::Adaptive
                }
                "disabled" | "off" => crate::model::provider::Thinking::Disabled,
                other => anyhow::bail!("thinking 只能是 adaptive 或 disabled，而不是 {other:?}"),
            }),
        };
        let effort = match config.effort.as_deref().map(str::trim) {
            None | Some("") => None,
            Some(value) => {
                let value = value.to_ascii_lowercase();
                anyhow::ensure!(
                    matches!(value.as_str(), "low" | "medium" | "high" | "xhigh" | "max"),
                    "effort 只能是 low、medium、high、xhigh 或 max"
                );
                Some(value)
            }
        };
        let model_fast = config
            .model_fast
            .as_deref()
            .filter(|_| !provider_overridden)
            .map(str::trim)
            .filter(|fast| !fast.is_empty() && !fast.eq_ignore_ascii_case("off"))
            .map(str::to_owned);

        let interactive = cli.print.is_none() && !cli.continue_task;
        let one_shot = cli.print.is_some() || cli.prompt.is_some() || cli.continue_task;
        let prompt = cli.print.or(cli.prompt);
        anyhow::ensure!(
            prompt.as_ref().is_none_or(|p| !p.trim().is_empty()),
            "prompt cannot be empty"
        );
        let context_window = cli
            .context_window
            .or(config.context_window)
            .or_else(|| {
                file.model_context_windows
                    .get(&model.to_ascii_lowercase())
                    .copied()
                    .filter(|window| (8192..=10_000_000).contains(window))
            })
            .unwrap_or_else(|| crate::model::models::context_window(&model));
        anyhow::ensure!(
            (8192..=10_000_000).contains(&context_window),
            "context_window must be 8192..10000000 tokens"
        );
        let max_turns = cli.max_turns.or(config.max_iterations).unwrap_or(200);
        let timeout = match (cli.timeout, config.max_turn_minutes) {
            (Some(seconds), _) => seconds,
            (None, Some(minutes)) => {
                anyhow::ensure!(minutes > 0, "max_turn_minutes 必须为正数");
                minutes.saturating_mul(60)
            }
            (None, None) => 3600,
        };
        let max_output_tokens = cli
            .max_output_tokens
            .or(config.max_output_tokens)
            .unwrap_or_else(|| crate::model::models::max_output_tokens(&model, context_window));
        let context_tokens = match cli.context_bytes.or(config.context_bytes) {
            Some(bytes) => {
                anyhow::ensure!(
                    (16384..=524288).contains(&bytes),
                    "context bytes must be 16384..524288"
                );
                bytes / 3
            }
            None => crate::model::models::context_budget(context_window, max_output_tokens)
                .min(2_000_000),
        };
        anyhow::ensure!(
            max_turns > 0 && timeout > 0 && max_output_tokens > 0,
            "limits must be positive"
        );
        // Prices are a pair: any command-line price replaces both configured prices.
        let price_pair = if cli.input_price.is_some() || cli.output_price.is_some() {
            (cli.input_price, cli.output_price)
        } else {
            (config.input_price, config.output_price)
        };
        let prices = match price_pair {
            (Some(input), Some(output))
                if input.is_finite() && output.is_finite() && input >= 0.0 && output >= 0.0 =>
            {
                Some((input, output))
            }
            // Built-in list prices; unknown models stay "cost unknown".
            (None, None) => crate::model::models::prices(&model),
            _ => anyhow::bail!("请同时提供两个有限的非负 token 价格（input_price、output_price）"),
        };
        let budget = match (cli.budget, config.max_budget_usd) {
            (Some(budget), _) => Some(budget),
            // The default cap, when the model's price is known.
            (None, None) if prices.is_some() => Some(10.0),
            // A configured budget without prices cannot be enforced.
            (None, Some(budget)) if prices.is_none() => {
                warnings.push(format!(
                    "config：已忽略 max_budget_usd {budget}；需同时设置 input_price 和 output_price 才能生效"
                ));
                None
            }
            (None, budget) => budget,
        };
        if let Some(budget) = budget {
            anyhow::ensure!(
                budget.is_finite() && budget > 0.0 && prices.is_some(),
                "预算必须为正数，并且需要同时设置两个 token 价格（input_price、output_price）"
            );
        }
        let permission = match (cli.permission, config.permission_mode) {
            (Some(mode), _) => mode,
            (None, Some(mode)) => parse_mode(&mode)?,
            (None, None) => Mode::Default,
        };
        let instructions = config
            .system_prompt
            .map(|text| text.trim().to_owned())
            .filter(|text| !text.is_empty());
        let cwd = cli.cwd.canonicalize()?;
        anyhow::ensure!(cwd.is_dir(), "cwd 必须是目录");
        let verify_commands = match file.done_verify_commands.take() {
            Some(commands) => {
                anyhow::ensure!(
                    commands.iter().all(|command| !command.trim().is_empty()),
                    "done_verify_commands cannot contain empty commands"
                );
                commands
            }
            None if file.done_verify_auto != Some(false) => crate::safety::verify::detect(&cwd),
            None => Vec::new(),
        };
        let verify_timeout = match file.done_verify_timeout_seconds {
            Some(seconds) => {
                anyhow::ensure!(
                    (1..=3600).contains(&seconds),
                    "done_verify_timeout_seconds must be 1..3600"
                );
                Some(Duration::from_secs(seconds))
            }
            None => None,
        };
        let subagent_max_turns = config.subagent_max_iterations.unwrap_or(60);
        anyhow::ensure!(
            subagent_max_turns > 0,
            "subagent_max_iterations must be positive"
        );
        let extension_base = match &config_path {
            Some(path) if path.is_file() => path
                .canonicalize()?
                .parent()
                .ok_or_else(|| anyhow::anyhow!("config has no parent"))?
                .to_path_buf(),
            // A config about to be created for the first time.
            Some(path) => path.parent().map_or_else(|| cwd.clone(), Path::to_path_buf),
            None => cwd.clone(),
        };
        Ok(Self {
            file: file_json,
            extensions: file.extensions,
            extension_base,
            trust_extensions: cli.trust_extensions
                || crate::config::trust::is_trusted_in(&data_dir, &cwd),
            project_config,
            config_path,
            profile,
            warnings,
            provider: info.map_or(provider_name, |info| info.name.to_owned()),
            protocol,
            model,
            base_url,
            api_key,
            api_keys,
            key_source,
            model_fast,
            thinking,
            effort,
            show_reasoning: config.show_reasoning.unwrap_or(false),
            cli: original,
            instructions,
            cwd,
            prompt,
            resume: cli.resume,
            continue_task: cli.continue_task,
            one_shot,
            data_dir,
            context_window,
            context_tokens,
            permission,
            interactive,
            max_turns,
            timeout: Duration::from_secs(timeout),
            max_output_tokens,
            budget,
            prices,
            verify_commands,
            verify_timeout,
            web_search: file.web_search.take().unwrap_or_default(),
            subagent_max_turns,
            dump_system_prompt: cli.dump_system_prompt,
            no_auto: cli.no_auto,
            debug,
            verbose,
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn base_urls_accept_pasted_endpoints() {
        assert_eq!(
            normalize_base_url(
                "https://api.deepseek.com/v1/chat/completions",
                Protocol::Openai
            ),
            "https://api.deepseek.com/v1"
        );
        assert_eq!(
            normalize_base_url("https://x.example/v1/v1/", Protocol::Openai),
            "https://x.example/v1"
        );
        assert_eq!(
            normalize_base_url("https://open.bigmodel.cn/api/paas/v4", Protocol::Openai),
            "https://open.bigmodel.cn/api/paas/v4"
        );
        assert_eq!(
            normalize_base_url("https://api.deepseek.com/anthropic", Protocol::Anthropic),
            "https://api.deepseek.com/anthropic/v1"
        );
        assert_eq!(
            normalize_base_url("https://api.anthropic.com/v1/messages", Protocol::Anthropic),
            "https://api.anthropic.com/v1"
        );
    }

    #[test]
    fn aliases_resolve_to_catalog_entries() {
        assert_eq!(provider_info("Moonshot").unwrap().name, "kimi");
        assert!(matches!(
            provider_info("claude").unwrap().protocol,
            Protocol::Anthropic
        ));
        assert!(provider_info("my-gateway").is_none());
    }

    #[test]
    fn unsupported_config_keys_are_reported_not_rejected() {
        let value = serde_json::json!({
            "active_profile": "a",
            "colour": "auto",
            "debug": true,
            "verbose": false,
            "telemetry": true,
            "mcp_servers": {},
            "provider": {"name": "deepseek", "image_files_api": true},
            "profiles": {"a": {"model": "m", "model_fast": "f", "theme": "dark", "thinking_tokens": 8000, "debug": false}}
        });
        // debug/verbose are supported; telemetry/thinking_tokens are retired keys.
        assert_eq!(
            unknown_keys(&value),
            ["colour", "provider.image_files_api", "profiles.a.theme"]
        );
        assert!(flag(&value, "debug") && !flag(&value, "verbose"));
        let mut file: FileConfig = serde_json::from_value(value).unwrap();
        let (name, profile) = file.resolve(None).unwrap();
        assert_eq!(name.as_deref(), Some("a"));
        assert_eq!(profile.model.as_deref(), Some("m"));
        let provider: ProviderConfig = profile.provider.unwrap().into();
        assert_eq!(provider.name, "deepseek");
    }

    #[test]
    fn documented_example_config_loads_without_warnings() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("docs/config.example.json");
        let mut warnings = Vec::new();
        let (mut file, _) = FileConfig::read(&path, &mut warnings).unwrap();
        assert!(warnings.is_empty(), "{warnings:?}");
        assert_eq!(file.done_verify_commands.as_ref().map(Vec::len), Some(2));
        for name in ["deepseek", "claude", "bedrock", "local"] {
            let (_, profile) = FileConfig {
                profiles: file.profiles.clone(),
                ..FileConfig::default()
            }
            .resolve(Some(name.into()))
            .unwrap();
            assert!(profile.model.is_some(), "{name}");
        }
        let (_, bedrock) = FileConfig {
            profiles: file.profiles.clone(),
            ..FileConfig::default()
        }
        .resolve(Some("bedrock".into()))
        .unwrap();
        assert_eq!(bedrock.max_budget_usd, Some(500.0));
        assert_eq!(bedrock.max_output_tokens, Some(16000));
        assert_eq!(bedrock.thinking.as_deref(), Some("adaptive"));
        assert_eq!(file.resolve(None).unwrap().0.as_deref(), Some("deepseek"));
    }

    #[test]
    fn profile_provider_replaces_root_provider() {
        let mut file: FileConfig = serde_json::from_value(serde_json::json!({
            "provider": {"name": "deepseek", "api_key": "root-key"},
            "max_iterations": 7,
            "profiles": {"k": {"provider": {"name": "kimi"}}}
        }))
        .unwrap();
        let (_, profile) = file.resolve(Some("k".into())).unwrap();
        let provider: ProviderConfig = profile.provider.unwrap().into();
        assert_eq!(provider.name, "kimi");
        assert!(provider.api_key.is_none());
        assert_eq!(profile.max_iterations, Some(7));
        assert!(file.resolve(Some("missing".into())).is_err());
    }
}
