use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

mod http;
pub mod oauth;

use rmcp::{
    ClientHandler, RoleClient, ServiceExt,
    model::{
        CallToolRequestParams, CallToolResponse, PaginatedRequestParams, ReadResourceRequestParams,
        ReadResourceResponse, Tool,
    },
    service::{MaybeSendFuture, NotificationContext, RunningService},
    transport::async_rw::AsyncRwTransport,
};
use serde::Deserialize;
use serde_json::{Value, json};
use tokio::sync::Mutex;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    extensions::skills::clip,
    extensions::{McpTransport, Registry, ServerConfig, Sourced},
    process::ManagedChild,
    safety::permission::{Authorizer, Effect},
    session::workspace::Workspace,
    tools::ToolOutput,
};

const DESCRIPTION_LIMIT: usize = 2 * 1024;
const SCHEMA_LIMIT: usize = 4 * 1024;
const CATALOG_LIMIT: usize = 24 * 1024;

#[derive(Deserialize)]
#[serde(tag = "action", rename_all = "snake_case", deny_unknown_fields)]
pub enum McpArgs {
    Servers,
    Tools {
        server: String,
    },
    Call {
        server: String,
        tool: String,
        arguments: serde_json::Map<String, Value>,
    },
    Resources {
        server: String,
    },
    Read {
        server: String,
        uri: String,
    },
}

/// Client handler that notes `notifications/tools/list_changed`.
#[derive(Clone, Default)]
struct Watcher {
    changed: Arc<AtomicBool>,
}

impl ClientHandler for Watcher {
    fn on_tool_list_changed(
        &self,
        _context: NotificationContext<RoleClient>,
    ) -> impl Future<Output = ()> + MaybeSendFuture + '_ {
        self.changed.store(true, Ordering::SeqCst);
        std::future::ready(())
    }
}

struct Connection {
    client: RunningService<RoleClient, Watcher>,
    process: Option<ManagedChild>,
    changed: Arc<AtomicBool>,
}
impl Connection {
    async fn close(&mut self) -> Result<()> {
        let closed = self.client.close_with_timeout(Duration::from_secs(3)).await;
        let stderr = match &mut self.process {
            Some(process) => process.stop().await?,
            None => String::new(),
        };
        if let Err(err) = closed {
            return Err(Error::Tool(format!(
                "MCP shutdown failed: {err}; stderr: {stderr}"
            )));
        }
        if closed.is_ok_and(|result| result.is_none()) {
            return Err(Error::Tool(format!(
                "MCP shutdown timed out; stderr: {stderr}"
            )));
        }
        if !stderr.trim().is_empty() {
            crate::app::ui::print_text(&format!("[MCP 错误输出] {stderr}\n"), true)?;
        }
        Ok(())
    }
}

struct Server {
    config: Sourced<ServerConfig>,
    connection: Mutex<Option<Connection>>,
    /// The one automatic reconnect after a lost connection was used.
    reconnected: AtomicBool,
}

/// Compact tool summary shown in the `mcp` tool description once a server is connected.
#[derive(Clone)]
struct ToolSummary {
    name: String,
    description: String,
    schema: String,
}

pub struct Mcp {
    servers: BTreeMap<String, Server>,
    /// Sign-ins of HTTP servers that require OAuth.
    oauth: Arc<oauth::OAuth>,
    root: PathBuf,
    host: PathBuf,
    trusted: bool,
    registry: Arc<Registry>,
    authorizer: Arc<dyn Authorizer>,
    workspace: Arc<Workspace>,
    catalog: std::sync::Mutex<BTreeMap<String, Vec<ToolSummary>>>,
}

/// URL without credentials or query, safe to show (queries often carry tokens).
fn shown_url(raw: &str) -> String {
    match url::Url::parse(raw) {
        Ok(url) => {
            format!(
                "{}://{}{}",
                url.scheme(),
                url.host_str().unwrap_or_default(),
                url.port().map(|p| format!(":{p}")).unwrap_or_default()
            ) + url.path()
        }
        Err(_) => "<invalid url>".into(),
    }
}

fn summary(tool: &Tool) -> ToolSummary {
    let schema = serde_json::to_string(&tool.input_schema).unwrap_or_default();
    ToolSummary {
        name: tool.name.to_string(),
        description: clip(
            tool.description.as_deref().unwrap_or_default().trim(),
            DESCRIPTION_LIMIT,
        ),
        schema: if schema.len() > SCHEMA_LIMIT {
            let properties: Vec<String> = tool
                .input_schema
                .get("properties")
                .and_then(Value::as_object)
                .map(|p| p.keys().cloned().collect())
                .unwrap_or_default();
            clip(
                &format!(
                    "{{\"properties\":{properties:?},\"note\":\"schema clipped; use action=tools\"}}"
                ),
                SCHEMA_LIMIT,
            )
        } else {
            schema
        },
    }
}

impl Mcp {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        configs: Vec<Sourced<ServerConfig>>,
        root: &Path,
        host: PathBuf,
        trusted: bool,
        registry: Arc<Registry>,
        authorizer: Arc<dyn Authorizer>,
        workspace: Arc<Workspace>,
        oauth_dir: PathBuf,
    ) -> Self {
        Self {
            oauth: Arc::new(oauth::OAuth::new(oauth_dir)),
            servers: configs
                .into_iter()
                .map(|config| {
                    (
                        config.id.clone(),
                        Server {
                            config,
                            connection: Mutex::new(None),
                            reconnected: AtomicBool::new(false),
                        },
                    )
                })
                .collect(),
            root: root.into(),
            host,
            trusted,
            registry,
            authorizer,
            workspace,
            catalog: std::sync::Mutex::new(BTreeMap::new()),
        }
    }

    /// Signs in to the HTTP server `id` through the browser (`say` reports progress)
    /// and reconnects it with the new token.
    pub async fn login(&self, id: &str, say: &(dyn Fn(&str) + Sync)) -> Result<()> {
        let url = self.http_url(id)?;
        self.authorizer
            .authorize(
                Effect::Network(format!("sign in to MCP {id}: {}", shown_url(&url))),
                CancellationToken::new(),
            )
            .await?;
        self.oauth.login(&url, say).await?;
        self.close(None, Some(id)).await
    }

    /// Forgets the sign-in of the HTTP server `id`; false when there was none.
    pub async fn logout(&self, id: &str) -> Result<bool> {
        let url = self.http_url(id)?;
        let removed = self.oauth.logout(&url)?;
        self.close(None, Some(id)).await?;
        Ok(removed)
    }

    fn http_url(&self, id: &str) -> Result<String> {
        let server = self
            .servers
            .get(id)
            .ok_or_else(|| Error::Tool(format!("unknown MCP server {id}")))?;
        if server.config.value.transport == McpTransport::Stdio {
            return Err(Error::Tool(format!(
                "{id} 是本地进程；登录只适用于 HTTP 服务器"
            )));
        }
        Ok(server.config.value.url.clone().unwrap_or_default())
    }

    pub fn ids(&self) -> Vec<String> {
        self.servers.keys().cloned().collect()
    }

    /// Connected servers' tools, appended to the `mcp` tool description.
    pub fn catalog_note(&self) -> String {
        let Ok(catalog) = self.catalog.lock() else {
            return String::new();
        };
        if catalog.is_empty() {
            return String::new();
        }
        let mut out =
            String::from("\nConnected MCP tools (call with action=call, server, tool, arguments):");
        for (server, tools) in catalog.iter() {
            for tool in tools {
                let line = format!(
                    "\n- {server}/{}: {} input={}",
                    tool.name, tool.description, tool.schema
                );
                if out.len() + line.len() > CATALOG_LIMIT {
                    out.push_str("\n- ... (more: action=tools)");
                    return out;
                }
                out.push_str(&line);
            }
        }
        out
    }

    fn forget(&self, id: &str) {
        if let Ok(mut catalog) = self.catalog.lock() {
            catalog.remove(id);
        }
    }

    pub async fn close(&self, plugin: Option<&str>, server: Option<&str>) -> Result<()> {
        let closings = self
            .servers
            .values()
            .filter(|entry| {
                plugin.is_none_or(|p| entry.config.plugin.as_deref() == Some(p))
                    && server.is_none_or(|id| entry.config.id == id)
            })
            .map(|entry| async {
                self.forget(&entry.config.id);
                if let Some(mut connection) = entry.connection.lock().await.take() {
                    connection.close().await?;
                }
                Ok::<_, Error>(())
            });
        let errors = futures_util::future::join_all(closings)
            .await
            .into_iter()
            .filter_map(|result| result.err().map(|err| err.to_string()))
            .collect::<Vec<_>>();
        if !errors.is_empty() {
            return Err(Error::Tool(errors.join("; ")));
        }
        Ok(())
    }

    fn effect(config: &Sourced<ServerConfig>, what: &str) -> Effect {
        match config.value.transport {
            // A host process, not a shell line: rules and modes treat it as Process.
            McpTransport::Stdio => Effect::Process(format!(
                "{what} MCP {}: {} {:?}",
                config.id, config.value.command, config.value.args
            )),
            _ => Effect::Network(format!(
                "{what} MCP {}: {}",
                config.id,
                shown_url(config.value.url.as_deref().unwrap_or_default())
            )),
        }
    }

    async fn connect(
        &self,
        config: &Sourced<ServerConfig>,
        cancel: &CancellationToken,
    ) -> Result<Connection> {
        self.authorizer
            .authorize(Self::effect(config, "start"), cancel.clone())
            .await?;
        let watcher = Watcher::default();
        let changed = watcher.changed.clone();
        if config.value.transport != McpTransport::Stdio {
            let url = config.value.url.clone().unwrap_or_default();
            let auth = http::Auth {
                oauth: self.oauth.clone(),
                server: url.clone(),
                name: config.id.clone(),
            };
            let transport = http::HttpTransport::connect(
                config.value.transport,
                &url,
                Duration::from_secs(config.value.timeout_seconds),
                auth,
            )
            .await
            .map_err(|err| Error::Tool(format!("MCP connect {}: {err}", config.id)))?;
            let client = tokio::select! {
                _ = cancel.cancelled() => return Err(Error::Cancelled),
                client = watcher.serve(transport) => client
                    .map_err(|err| Error::Uncertain(format!("MCP initialize {}: {err}", config.id)))?,
            };
            return Ok(Connection {
                client,
                process: None,
                changed,
            });
        }
        self.workspace.note_shell()?;
        let mut process = ManagedChild::spawn_with(
            &self.host,
            &self.root,
            &config.value.command,
            &config.value.args,
            &config.value.env,
            &config.value.env_values,
            cancel,
        )
        .await?;
        let read = process
            .stdout
            .take()
            .ok_or_else(|| Error::Tool("missing MCP stdout".into()))?;
        let write = process
            .stdin
            .take()
            .ok_or_else(|| Error::Tool("missing MCP stdin".into()))?;
        let transport = AsyncRwTransport::new_client(read, write).with_max_line_length(1024 * 1024);
        let client = match watcher.serve(transport).await {
            Ok(client) => client,
            Err(err) => {
                let detail = match process.stop().await {
                    Ok(stderr) => format!("stderr: {stderr}"),
                    Err(cleanup) => format!("cleanup: {cleanup}"),
                };
                return Err(Error::Uncertain(format!(
                    "MCP initialize {}: {err}; {detail}",
                    config.id
                )));
            }
        };
        Ok(Connection {
            client,
            process: Some(process),
            changed,
        })
    }

    fn allowed(config: &ServerConfig, tool: &str) -> bool {
        config.all_tools || config.tools.iter().any(|name| name == tool)
    }

    /// All allowed tools of a live connection (paginated), refreshing the description cache.
    async fn list_tools(
        &self,
        server: &Server,
        client: &RunningService<RoleClient, Watcher>,
        changed: &AtomicBool,
    ) -> Result<Vec<Tool>> {
        let mut cursor = None;
        let mut tools = Vec::new();
        let mut cursors = std::collections::HashSet::new();
        for _ in 0..16 {
            let params = Some(PaginatedRequestParams::default().with_cursor(cursor));
            let page = client
                .list_tools(params)
                .await
                .map_err(|err| Error::Uncertain(format!("MCP discovery failed: {err}")))?;
            tools.extend(
                page.tools
                    .into_iter()
                    .filter(|tool| Self::allowed(&server.config.value, tool.name.as_ref())),
            );
            if tools.len() > 256 {
                return Err(Error::Tool("MCP discovery exceeds 256 entries".into()));
            }
            let Some(next) = page.next_cursor else {
                changed.store(false, Ordering::SeqCst);
                if let Ok(mut catalog) = self.catalog.lock() {
                    catalog.insert(
                        server.config.id.clone(),
                        tools.iter().map(summary).collect(),
                    );
                }
                return Ok(tools);
            };
            if !cursors.insert(next.clone()) {
                return Err(Error::Tool("MCP returned a repeated cursor".into()));
            }
            cursor = Some(next);
        }
        Err(Error::Tool("MCP pagination exceeds 16 pages".into()))
    }

    fn checked(&self, id: &str) -> Result<&Server> {
        let server = self
            .servers
            .get(id)
            .ok_or_else(|| Error::Tool("unknown MCP server".into()))?;
        if !self
            .registry
            .active("mcp", id, server.config.plugin.as_deref())?
        {
            return Err(Error::Tool("MCP server is disabled".into()));
        }
        if !self.trusted {
            return Err(Error::Approval(
                "MCP 服务器需要 --trust-extensions（或先用 /config trust 信任此项目并重启），并且需要授权".into(),
            ));
        }
        Ok(server)
    }

    /// `/config mcp connect`: (re)connects and lists tools; resets the reconnect allowance.
    pub async fn connect_now(&self, id: &str, cancel: CancellationToken) -> Result<Vec<String>> {
        let server = self.checked(id)?;
        let mut guard = server.connection.lock().await;
        if let Some(mut old) = guard.take() {
            old.close().await?;
        }
        server.reconnected.store(false, Ordering::SeqCst);
        let connection = self.connect(&server.config, &cancel).await?;
        let tools = tokio::time::timeout(
            Duration::from_secs(server.config.value.timeout_seconds),
            self.list_tools(server, &connection.client, &connection.changed),
        )
        .await
        .map_err(|_| Error::Tool("MCP tools/list timed out".into()))??;
        *guard = Some(connection);
        Ok(tools.into_iter().map(|t| t.name.to_string()).collect())
    }

    pub async fn execute(&self, args: McpArgs, cancel: CancellationToken) -> Result<ToolOutput> {
        if let McpArgs::Servers = args {
            let mut entries = Vec::new();
            for server in self.servers.values() {
                let value = &server.config.value;
                let connected = server
                    .connection
                    .try_lock()
                    .map(|c| c.as_ref().is_some_and(|c| !c.client.is_closed()))
                    .unwrap_or(true);
                // Env values are secrets: only their names are ever shown.
                entries.push(json!({"server":server.config.id,"plugin":server.config.plugin,
                    "transport":value.transport,
                    "url":value.url.as_deref().map(shown_url),
                    "env":value.env.iter().cloned().chain(value.env_values.iter().map(|(k, _)| k.clone())).collect::<Vec<_>>(),
                    "connected":connected,
                    "signed_in":value.url.as_deref().map(|url| self.oauth.signed_in(url)),
                    "enabled":self.registry.active("mcp",&server.config.id,server.config.plugin.as_deref())?,
                    "tools_allowlist":if value.all_tools { json!("all") } else { json!(value.tools) }}));
            }
            return Ok(ToolOutput {
                content: crate::tools::json_output(json!(entries))?,
                failed: false,
                ..Default::default()
            });
        }
        let id = match &args {
            McpArgs::Tools { server }
            | McpArgs::Call { server, .. }
            | McpArgs::Resources { server }
            | McpArgs::Read { server, .. } => server.clone(),
            McpArgs::Servers => unreachable!(),
        };
        let server = self.checked(&id)?;
        if let McpArgs::Call {
            tool, arguments, ..
        } = &args
        {
            if !Self::allowed(&server.config.value, tool) {
                return Err(Error::Approval(
                    "这个 MCP 工具不在显式允许列表中（只开放服务器声明的工具）".into(),
                ));
            }
            if serde_json::to_vec(arguments)?.len() > 64 * 1024 {
                return Err(Error::Tool("MCP arguments exceed 64 KiB".into()));
            }
        }
        if let McpArgs::Read { uri, .. } = &args
            && (uri.is_empty() || uri.len() > 8192)
        {
            return Err(Error::Tool("invalid resource URI".into()));
        }
        self.authorizer
            .authorize(
                Self::effect(
                    &server.config,
                    "operation on (server annotations do not confer read-only trust)",
                ),
                cancel.clone(),
            )
            .await?;
        if server.config.value.transport == McpTransport::Stdio {
            self.workspace.note_shell()?;
        }
        let mut guard = tokio::select! {
            biased;
            _ = cancel.cancelled() => return Err(Error::Cancelled),
            guard = server.connection.lock() => guard,
        };
        let operation = async {
            if guard.as_ref().is_some_and(|c| c.client.is_closed())
                && let Some(mut old) = guard.take()
            {
                self.forget(&id);
                let _ = old.close().await;
                // One automatic reconnect per lost connection; after that, the operator decides.
                if server.reconnected.swap(true, Ordering::SeqCst) {
                    return Err(Error::Tool(format!(
                        "MCP {id} connection lost again after an automatic reconnect; use /config mcp connect {id}"
                    )));
                }
            }
            if guard.is_none() {
                let connection = self.connect(&server.config, &cancel).await?;
                self.list_tools(server, &connection.client, &connection.changed)
                    .await?;
                *guard = Some(connection);
            }
            let connection = guard
                .as_ref()
                .ok_or_else(|| Error::Tool("MCP connection missing".into()))?;
            let (client, changed) = (&connection.client, connection.changed.clone());
            if changed.load(Ordering::SeqCst) {
                self.list_tools(server, client, &changed).await?;
            }
            match args {
                McpArgs::Call {
                    tool, arguments, ..
                } => {
                    let result = client
                        .call_tool_once(CallToolRequestParams::new(tool).with_arguments(arguments))
                        .await
                        .map_err(|err| {
                            Error::Uncertain(format!("MCP 调用失败（不会自动重试）：{err}"))
                        })?;
                    let CallToolResponse::Complete(result) = result else {
                        return Err(Error::Uncertain(
                            "MCP 返回了后台任务或需要输入的响应；暂不支持，也不会自动跟进".into(),
                        ));
                    };
                    Ok(ToolOutput {
                        failed: result.is_error.unwrap_or(false),
                        content: crate::tools::external::wrap(
                            "mcp tool",
                            &crate::tools::json_output(serde_json::to_value(result)?)?,
                        ),
                        ..Default::default()
                    })
                }
                McpArgs::Read { uri, .. } => {
                    let result = client
                        .read_resource_once(ReadResourceRequestParams::new(uri))
                        .await
                        .map_err(|err| {
                            Error::Uncertain(format!("MCP resource read failed: {err}"))
                        })?;
                    let ReadResourceResponse::Complete(result) = result else {
                        return Err(Error::Tool(
                            "MCP resource requires unsupported client input".into(),
                        ));
                    };
                    Ok(ToolOutput {
                        failed: false,
                        content: crate::tools::external::wrap(
                            "mcp resource",
                            &crate::tools::json_output(serde_json::to_value(result)?)?,
                        ),
                        ..Default::default()
                    })
                }
                McpArgs::Tools { .. } => {
                    let tools = self.list_tools(server, client, &changed).await?;
                    Ok(ToolOutput {
                        failed: false,
                        content: crate::tools::json_output(serde_json::to_value(tools)?)?,
                        ..Default::default()
                    })
                }
                McpArgs::Resources { .. } => {
                    let mut cursor = None;
                    let mut entries = Vec::new();
                    let mut cursors = std::collections::HashSet::new();
                    for _ in 0..16 {
                        let params = Some(PaginatedRequestParams::default().with_cursor(cursor));
                        let page = client.list_resources(params).await.map_err(|err| {
                            Error::Uncertain(format!("MCP resources failed: {err}"))
                        })?;
                        entries.extend(
                            page.resources
                                .into_iter()
                                .map(serde_json::to_value)
                                .collect::<std::result::Result<Vec<_>, _>>()?,
                        );
                        if entries.len() > 256 {
                            return Err(Error::Tool("MCP discovery exceeds 256 entries".into()));
                        }
                        let Some(next) = page.next_cursor else {
                            return Ok(ToolOutput {
                                failed: false,
                                content: crate::tools::json_output(json!(entries))?,
                                ..Default::default()
                            });
                        };
                        if !cursors.insert(next.clone()) {
                            return Err(Error::Tool("MCP returned a repeated cursor".into()));
                        }
                        cursor = Some(next);
                    }
                    Err(Error::Tool("MCP pagination exceeds 16 pages".into()))
                }
                McpArgs::Servers => unreachable!(),
            }
        };
        let result = tokio::select! {
            biased;
            _ = cancel.cancelled() => Err(Error::Cancelled),
            result = tokio::time::timeout(Duration::from_secs(server.config.value.timeout_seconds), operation) =>
                result.map_err(|_| Error::Uncertain("MCP 操作超时；远端可能已产生效果".into())).and_then(|r| r),
        };
        if result.is_err()
            && let Some(mut connection) = guard.take()
        {
            self.forget(&id);
            connection.close().await.map_err(|cleanup| {
                Error::Uncertain(format!(
                    "MCP operation failed: {}; cleanup: {cleanup}",
                    result
                        .as_ref()
                        .err()
                        .map_or(String::new(), ToString::to_string)
                ))
            })?;
        }
        result
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls_are_shown_without_secrets() {
        assert_eq!(
            shown_url("https://user:pw@mcp.example:8443/v1/mcp?token=abc"),
            "https://mcp.example:8443/v1/mcp"
        );
    }

    #[test]
    fn top_level_servers_parse_types_and_keep_env_values_private() {
        let servers = crate::extensions::top_level_servers(&json!({"mcp_servers":{
            "a":{"command":"npx","args":["x"],"env":{"TOKEN":"secret"}},
            "b":{"type":"streamable_http","url":"https://h/mcp"},
            "c":{"url":"http://127.0.0.1:9/mcp"},
            "d":{"type":"sse","url":"https://h/sse"}}}))
        .unwrap();
        assert_eq!(servers["a"].transport, McpTransport::Stdio);
        assert_eq!(
            servers["a"].env_values,
            [("TOKEN".to_owned(), "secret".to_owned())]
        );
        assert!(servers["a"].all_tools);
        assert_eq!(servers["b"].transport, McpTransport::Http);
        assert_eq!(servers["c"].transport, McpTransport::Http);
        assert_eq!(servers["d"].transport, McpTransport::Sse);
        assert!(
            crate::extensions::top_level_servers(&json!({"mcp_servers":{"x":{"type":"ws"}}}))
                .is_err()
        );
    }
}
