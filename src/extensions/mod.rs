//! Extensions: the skill/plugin/hook/MCP registry and catalog, plus skills, plugins, hooks and MCP clients.

pub mod hooks;
pub mod mcp;
pub mod plugins;
pub mod skill_review;
pub mod skills;

use std::{
    collections::{BTreeMap, BTreeSet},
    path::{Path, PathBuf},
    sync::Mutex,
};

use serde::{Deserialize, Serialize};

use crate::{
    domain::{Error, Result},
    session::{atomic_json, read_json},
};

pub const TEXT_LIMIT: usize = 32 * 1024;

pub fn identifier(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

#[derive(Clone, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ExtensionConfig {
    pub skills: Vec<PathBuf>,
    pub mcp: BTreeMap<String, ServerConfig>,
    pub hooks: Vec<HookConfig>,
    pub plugins: Vec<PathBuf>,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum McpTransport {
    #[default]
    Stdio,
    /// Legacy HTTP+SSE (GET event stream, POST to the announced endpoint).
    Sse,
    /// Streamable HTTP (MCP 2025-03-26+).
    #[serde(alias = "streamable_http", alias = "streamable-http")]
    Http,
}

#[derive(Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServerConfig {
    #[serde(default)]
    pub command: String,
    #[serde(default)]
    pub args: Vec<String>,
    /// Names of variables passed through from cove's own environment.
    #[serde(default)]
    pub env: Vec<String>,
    #[serde(default)]
    pub tools: Vec<String>,
    #[serde(default = "timeout")]
    pub timeout_seconds: u64,
    #[serde(default, rename = "type")]
    pub transport: McpTransport,
    #[serde(default)]
    pub url: Option<String>,
    /// Explicit values from an `mcp_servers` env map. Secrets: never shown or persisted.
    #[serde(skip)]
    pub env_values: Vec<(String, String)>,
    /// `mcp_servers` entries expose every tool (each call is still authorized).
    #[serde(skip)]
    pub all_tools: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct GoServer {
    #[serde(default)]
    command: String,
    #[serde(default)]
    args: Vec<String>,
    #[serde(default)]
    env: BTreeMap<String, String>,
    #[serde(default, rename = "type")]
    transport: Option<String>,
    #[serde(default)]
    url: Option<String>,
    #[serde(default)]
    timeout_seconds: Option<u64>,
}

/// The top-level `mcp_servers: {name: {command, args, env, type, url}}` from the effective
/// config JSON, as servers with every tool exposed.
pub fn top_level_servers(file: &serde_json::Value) -> Result<BTreeMap<String, ServerConfig>> {
    let Some(servers) = file.get("mcp_servers") else {
        return Ok(BTreeMap::new());
    };
    let servers: BTreeMap<String, GoServer> = serde_json::from_value(servers.clone())
        .map_err(|err| Error::Tool(format!("mcp_servers 配置无效：{err}")))?;
    let mut out = BTreeMap::new();
    for (name, server) in servers {
        let transport = match server
            .transport
            .as_deref()
            .map(str::to_ascii_lowercase)
            .as_deref()
        {
            None | Some("") if server.url.is_some() && server.command.is_empty() => {
                McpTransport::Http
            }
            None | Some("" | "stdio") => McpTransport::Stdio,
            Some("sse") => McpTransport::Sse,
            Some("http" | "streamable_http" | "streamable-http" | "streamablehttp") => {
                McpTransport::Http
            }
            Some(other) => {
                return Err(Error::Tool(format!(
                    "mcp_servers.{name}：未知的 type {other:?}（可用 stdio、sse、http）"
                )));
            }
        };
        if server.env.len() > 64
            || server.env.iter().any(|(k, v)| {
                k.is_empty() || k.contains(['=', '\0']) || v.len() > 32 * 1024 || v.contains('\0')
            })
        {
            return Err(Error::Tool(format!("mcp_servers.{name}：env 无效")));
        }
        out.insert(
            name,
            ServerConfig {
                command: server.command,
                args: server.args,
                env: Vec::new(),
                tools: Vec::new(),
                timeout_seconds: server.timeout_seconds.unwrap_or(60),
                transport,
                url: server.url,
                env_values: server.env.into_iter().collect(),
                all_tools: true,
            },
        );
    }
    Ok(out)
}

fn timeout() -> u64 {
    30
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum HookEvent {
    SessionStart,
    TaskStart,
    BeforeTool,
    AfterTool,
    TaskEnd,
    SessionEnd,
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HookConfig {
    pub name: String,
    pub event: HookEvent,
    pub command: String,
    #[serde(default = "timeout")]
    pub timeout_seconds: u64,
    /// Tool-name regex (anchored, case-insensitive, Claude Code names accepted) for tool events.
    #[serde(default)]
    pub matcher: Option<String>,
    /// Fire and forget; cannot block.
    #[serde(default, rename = "async")]
    pub run_async: bool,
    /// Loaded from `hooks.json` (Claude Code format): stdin JSON, `{"continue": false}`
    /// or exit 2 blocks a PreToolUse, failures do not stop the task (see `hooks.rs`).
    #[serde(skip)]
    pub claude: bool,
}

#[derive(Clone, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Manifest {
    pub version: u32,
    pub name: String,
    #[serde(default)]
    pub skills: Vec<PathBuf>,
    #[serde(default)]
    pub mcp: BTreeMap<String, ServerConfig>,
    #[serde(default)]
    pub hooks: Vec<HookConfig>,
}

impl Manifest {
    pub fn load(path: &Path) -> Result<Self> {
        let manifest: Self = read_json(path)?;
        if manifest.version != 1 || !identifier(&manifest.name) {
            return Err(Error::Tool("invalid plugin name/version".into()));
        }
        Ok(manifest)
    }
}

#[derive(Clone)]
pub struct Sourced<T> {
    pub id: String,
    pub plugin: Option<String>,
    pub value: T,
}

#[derive(Clone, Default, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Activation {
    pub disabled: BTreeSet<String>,
    pub installed: BTreeMap<String, PathBuf>,
}

pub struct Registry {
    path: PathBuf,
    state: Mutex<Activation>,
}

impl Registry {
    pub fn open(data: &Path) -> Result<Self> {
        let path = data.join("extensions.json");
        let state = if path.try_exists()? {
            read_json(&path)?
        } else {
            Activation::default()
        };
        Ok(Self {
            path,
            state: Mutex::new(state),
        })
    }
    pub fn path(&self) -> &Path {
        &self.path
    }
    pub fn snapshot(&self) -> Result<Activation> {
        Ok(self
            .state
            .lock()
            .map_err(|_| Error::Tool("extension registry poisoned".into()))?
            .clone())
    }
    pub fn active(&self, kind: &str, id: &str, plugin: Option<&str>) -> Result<bool> {
        let state = self.snapshot()?;
        Ok(!state.disabled.contains(&format!("{kind}:{id}"))
            && plugin.is_none_or(|p| !state.disabled.contains(&format!("plugin:{p}"))))
    }
    pub fn change(&self, change: impl FnOnce(&mut Activation) -> Result<()>) -> Result<()> {
        let mut guard = self
            .state
            .lock()
            .map_err(|_| Error::Tool("extension registry poisoned".into()))?;
        let mut next = guard.clone();
        change(&mut next)?;
        atomic_json(&self.path, &next)?;
        *guard = next;
        Ok(())
    }
}

pub struct Catalog {
    pub skills: Vec<Sourced<PathBuf>>,
    pub servers: Vec<Sourced<ServerConfig>>,
    pub hooks: Vec<Sourced<HookConfig>>,
    pub plugins: BTreeMap<String, PathBuf>,
}

impl Catalog {
    pub fn validate_plugin(path: &Path) -> Result<Manifest> {
        let path = path.canonicalize()?;
        let manifest = Manifest::load(&path)?;
        let base = path
            .parent()
            .ok_or_else(|| Error::Tool("plugin has no parent".into()))?;
        let mut catalog = Self {
            skills: Vec::new(),
            servers: Vec::new(),
            hooks: Vec::new(),
            plugins: BTreeMap::new(),
        };
        catalog.append(
            manifest.skills.clone(),
            manifest.mcp.clone(),
            manifest.hooks.clone(),
            base,
            Some(manifest.name.clone()),
        )?;
        if catalog
            .skills
            .iter()
            .any(|root| !root.value.starts_with(base))
            || catalog.servers.len() > 16
            || catalog.hooks.len() > 64
            || catalog.skills.len() > 32
        {
            return Err(Error::Tool(
                "plugin resources escape directory or exceed catalog limits".into(),
            ));
        }
        Ok(manifest)
    }

    pub fn load(config: ExtensionConfig, base: &Path, registry: &Registry) -> Result<Self> {
        let mut catalog = Self {
            skills: Vec::new(),
            servers: Vec::new(),
            hooks: Vec::new(),
            plugins: BTreeMap::new(),
        };
        catalog.append(config.skills, config.mcp, config.hooks, base, None)?;
        let paths = config
            .plugins
            .into_iter()
            .map(|p| base.join(p))
            .chain(registry.snapshot()?.installed.into_values())
            .collect::<Vec<_>>();
        if paths.len() > 32 {
            return Err(Error::Tool("at most 32 plugins".into()));
        }
        for path in paths {
            let path = path.canonicalize()?;
            let manifest = Manifest::load(&path)?;
            let name = manifest.name;
            if catalog.plugins.insert(name.clone(), path.clone()).is_some() {
                return Err(Error::Tool(format!("duplicate plugin {name}")));
            }
            let base = path
                .parent()
                .ok_or_else(|| Error::Tool("plugin has no parent".into()))?;
            for skill in &manifest.skills {
                let root = base.join(skill).canonicalize()?;
                if !root.starts_with(base) {
                    return Err(Error::Tool("plugin skill escaped its directory".into()));
                }
            }
            catalog.append(
                manifest.skills,
                manifest.mcp,
                manifest.hooks,
                base,
                Some(name),
            )?;
        }
        if catalog.servers.len() > 16 || catalog.hooks.len() > 64 || catalog.skills.len() > 32 {
            return Err(Error::Tool("extension catalog limits exceeded".into()));
        }
        Ok(catalog)
    }
    fn append(
        &mut self,
        skills: Vec<PathBuf>,
        servers: BTreeMap<String, ServerConfig>,
        hooks: Vec<HookConfig>,
        base: &Path,
        plugin: Option<String>,
    ) -> Result<()> {
        for root in skills {
            let root = base.join(root).canonicalize()?;
            if !root.is_dir() {
                return Err(Error::Tool("skill root must be a directory".into()));
            }
            self.skills.push(Sourced {
                id: String::new(),
                plugin: plugin.clone(),
                value: root,
            });
        }
        for (name, server) in servers {
            let endpoint_ok = match server.transport {
                McpTransport::Stdio => !server.command.trim().is_empty() && server.url.is_none(),
                McpTransport::Sse | McpTransport::Http => server.url.as_deref().is_some_and(|u| {
                    u.len() <= 4096
                        && url::Url::parse(u).is_ok_and(|u| matches!(u.scheme(), "http" | "https"))
                }),
            };
            if !identifier(&name)
                || !endpoint_ok
                || server.command.len() > 4096
                || server.args.len() > 64
                || server.args.iter().any(|s| s.len() > 8192)
                || !(1..=120).contains(&server.timeout_seconds)
                || server.env.len() > 64
                || server
                    .env
                    .iter()
                    .any(|s| s.is_empty() || s.contains(['=', '\0']))
                || server.tools.len() > 256
            {
                return Err(Error::Tool(format!("invalid MCP configuration: {name}")));
            }
            let id = plugin
                .as_ref()
                .map_or(name.clone(), |p| format!("{p}/{name}"));
            if self.servers.iter().any(|s| s.id == id) {
                return Err(Error::Tool(format!("duplicate MCP server {id}")));
            }
            self.servers.push(Sourced {
                id,
                plugin: plugin.clone(),
                value: server,
            });
        }
        for hook in hooks {
            if !identifier(&hook.name)
                || hook.command.trim().is_empty()
                || hook.command.len() > 64 * 1024
                || !(1..=120).contains(&hook.timeout_seconds)
                || hook
                    .matcher
                    .as_deref()
                    .is_some_and(|m| crate::extensions::hooks::matcher_regex(m).is_err())
            {
                return Err(Error::Tool("invalid lifecycle hook configuration".into()));
            }
            let id = plugin
                .as_ref()
                .map_or(hook.name.clone(), |p| format!("{p}/{}", hook.name));
            if self.hooks.iter().any(|h| h.id == id) {
                return Err(Error::Tool(format!("duplicate hook {id}")));
            }
            self.hooks.push(Sourced {
                id,
                plugin: plugin.clone(),
                value: hook,
            });
        }
        Ok(())
    }
}

pub fn bounded(text: String) -> String {
    if text.len() <= TEXT_LIMIT {
        return text;
    }
    let end = text.floor_char_boundary(TEXT_LIMIT);
    format!("{}\n[extension output truncated]", &text[..end])
}
