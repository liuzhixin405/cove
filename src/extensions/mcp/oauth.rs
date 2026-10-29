//! OAuth for HTTP MCP servers, as the MCP authorization flow describes it:
//! protected-resource and authorization-server discovery, dynamic client
//! registration, authorization code with PKCE through the browser and a loopback
//! redirect, and refresh. Tokens live in `<data_dir>/mcp-oauth/`, inside the data
//! directory no tool can reach, and are never shown.

use std::{
    collections::HashMap,
    path::PathBuf,
    sync::Mutex,
    time::{Duration, SystemTime, UNIX_EPOCH},
};

use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use url::Url;

use crate::domain::{Error, Result};

/// How long the browser sign-in may take.
const LOGIN_TIMEOUT: Duration = Duration::from_secs(300);
/// A token this close to expiry is refreshed first.
const EXPIRY_MARGIN: u64 = 60;

/// A login for one MCP server URL.
#[derive(Clone, Serialize, Deserialize)]
struct Saved {
    server: String,
    resource: String,
    token_endpoint: String,
    client_id: String,
    #[serde(default)]
    client_secret: Option<String>,
    access_token: String,
    #[serde(default)]
    refresh_token: Option<String>,
    /// Unix seconds; `None` when the server did not say.
    #[serde(default)]
    expires_at: Option<u64>,
}

#[derive(Deserialize)]
struct ResourceMetadata {
    #[serde(default)]
    resource: Option<String>,
    #[serde(default)]
    authorization_servers: Vec<String>,
    #[serde(default)]
    scopes_supported: Vec<String>,
}

#[derive(Deserialize)]
struct ServerMetadata {
    authorization_endpoint: String,
    token_endpoint: String,
    #[serde(default)]
    registration_endpoint: Option<String>,
}

#[derive(Deserialize)]
struct TokenResponse {
    access_token: String,
    #[serde(default)]
    refresh_token: Option<String>,
    #[serde(default)]
    expires_in: Option<u64>,
}

pub struct OAuth {
    dir: PathBuf,
    client: reqwest::Client,
    cache: Mutex<HashMap<String, Saved>>,
    /// Off in tests: the authorization URL is only reported.
    browser: bool,
}

fn now() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |time| time.as_secs())
}

fn failed(what: &str, err: impl std::fmt::Display) -> Error {
    Error::Tool(format!(
        "MCP 登录失败（{what}）：{err}；检查网络或服务器设置后，可再次运行 /config mcp login <名称>"
    ))
}

/// URL-safe base64 without padding (PKCE, state).
fn encode(bytes: &[u8]) -> String {
    base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(bytes)
}

fn random() -> String {
    let bytes: Vec<u8> = (0..2)
        .flat_map(|_| uuid::Uuid::new_v4().into_bytes())
        .collect();
    encode(&bytes)
}

/// `resource_metadata` and `scope` of a `WWW-Authenticate: Bearer ...` header.
fn challenge(header: &str) -> (Option<String>, Option<String>) {
    let value = |name: &str| {
        let start = header.find(&format!("{name}=\""))? + name.len() + 2;
        let end = header[start..].find('"')? + start;
        Some(header[start..end].to_owned())
    };
    (value("resource_metadata"), value("scope"))
}

/// Well-known locations of `document` for `base` (RFC 8414 path insertion first).
fn well_known(base: &Url, document: &str) -> Vec<String> {
    let origin = base.origin().ascii_serialization();
    let path = base.path().trim_end_matches('/');
    let mut urls = Vec::new();
    if !path.is_empty() {
        urls.push(format!("{origin}/.well-known/{document}{path}"));
        urls.push(format!("{origin}{path}/.well-known/{document}"));
    }
    urls.push(format!("{origin}/.well-known/{document}"));
    urls
}

impl OAuth {
    pub fn new(dir: PathBuf) -> Self {
        let client = crate::model::net::apply(reqwest::Client::builder())
            .connect_timeout(Duration::from_secs(10))
            .timeout(Duration::from_secs(30))
            .redirect(reqwest::redirect::Policy::limited(3))
            .user_agent(concat!("cove/", env!("CARGO_PKG_VERSION")))
            .build()
            .unwrap_or_default();
        Self {
            dir,
            client,
            cache: Mutex::new(HashMap::new()),
            browser: true,
        }
    }

    /// Reports the authorization URL without opening a browser.
    pub fn without_browser(mut self) -> Self {
        self.browser = false;
        self
    }

    fn path(&self, server: &str) -> PathBuf {
        let key = format!("{:x}", Sha256::digest(server.as_bytes()));
        self.dir.join(format!("{}.json", &key[..16]))
    }

    fn load(&self, server: &str) -> Option<Saved> {
        if let Some(saved) = self.cache.lock().ok()?.get(server) {
            return Some(saved.clone());
        }
        let saved: Saved = crate::session::read_json(&self.path(server)).ok()?;
        (saved.server == server).then_some(saved)
    }

    fn store(&self, saved: Saved) -> Result<()> {
        std::fs::create_dir_all(&self.dir)?;
        crate::session::atomic_json(&self.path(&saved.server), &saved)?;
        if let Ok(mut cache) = self.cache.lock() {
            cache.insert(saved.server.clone(), saved);
        }
        Ok(())
    }

    /// Whether `server` has a stored sign-in.
    pub fn signed_in(&self, server: &str) -> bool {
        self.load(server).is_some()
    }

    /// Forgets the sign-in of `server`; false when there was none.
    pub fn logout(&self, server: &str) -> Result<bool> {
        if let Ok(mut cache) = self.cache.lock() {
            cache.remove(server);
        }
        match std::fs::remove_file(self.path(server)) {
            Ok(()) => Ok(true),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(false),
            Err(err) => Err(err.into()),
        }
    }

    /// A usable access token for `server`, refreshed when it expires (or when
    /// `force`, after the server refused it); `None` without a sign-in.
    pub async fn bearer(&self, server: &str, force: bool) -> Result<Option<String>> {
        let Some(saved) = self.load(server) else {
            return Ok(None);
        };
        let expired = saved
            .expires_at
            .is_some_and(|at| at <= now() + EXPIRY_MARGIN);
        if !expired && !force {
            return Ok(Some(saved.access_token));
        }
        let Some(refresh) = saved.refresh_token.clone() else {
            return Ok((!expired).then_some(saved.access_token));
        };
        let mut form = vec![
            ("grant_type", "refresh_token".to_owned()),
            ("refresh_token", refresh),
            ("client_id", saved.client_id.clone()),
            ("resource", saved.resource.clone()),
        ];
        if let Some(secret) = &saved.client_secret {
            form.push(("client_secret", secret.clone()));
        }
        let token = self.token(&saved.token_endpoint, &form).await?;
        let updated = Saved {
            access_token: token.access_token,
            refresh_token: token.refresh_token.or(saved.refresh_token.clone()),
            expires_at: token.expires_in.map(|seconds| now() + seconds),
            ..saved
        };
        let access = updated.access_token.clone();
        self.store(updated)?;
        Ok(Some(access))
    }

    async fn token(&self, endpoint: &str, form: &[(&str, String)]) -> Result<TokenResponse> {
        let response = self
            .client
            .post(endpoint)
            .form(form)
            .send()
            .await
            .map_err(|err| failed("token request", err))?;
        let status = response.status();
        let text = response.text().await.unwrap_or_default();
        if !status.is_success() {
            return Err(failed(
                "token request",
                format!(
                    "HTTP {status}: {}",
                    crate::extensions::skills::clip(&text, 300)
                ),
            ));
        }
        serde_json::from_str(&text).map_err(|err| failed("token response", err))
    }

    async fn json<T: serde::de::DeserializeOwned>(&self, url: &str) -> Option<T> {
        let response = self.client.get(url).send().await.ok()?;
        if !response.status().is_success() {
            return None;
        }
        response.json().await.ok()
    }

    /// The resource, its authorization server's metadata and the scopes to ask for.
    async fn discover(&self, server: &Url) -> Result<(String, ServerMetadata, Option<String>)> {
        let probe = self
            .client
            .post(server.clone())
            .header(
                reqwest::header::ACCEPT,
                "application/json, text/event-stream",
            )
            .json(
                &json!({"jsonrpc":"2.0","id":0,"method":"initialize","params":{
                "protocolVersion":"2025-06-18","capabilities":{},
                "clientInfo":{"name":"cove","version":env!("CARGO_PKG_VERSION")}}}),
            )
            .send()
            .await
            .map_err(|err| failed("contacting the server", err))?;
        let (metadata_url, scope) = probe
            .headers()
            .get(reqwest::header::WWW_AUTHENTICATE)
            .and_then(|value| value.to_str().ok())
            .map(challenge)
            .unwrap_or_default();
        let mut resource_urls: Vec<String> = metadata_url.into_iter().collect();
        resource_urls.extend(well_known(server, "oauth-protected-resource"));
        let mut resource: Option<ResourceMetadata> = None;
        for url in resource_urls {
            if let Some(found) = self.json::<ResourceMetadata>(&url).await {
                resource = Some(found);
                break;
            }
        }
        // Without resource metadata the server's own origin is the authorization server.
        let issuer = resource
            .as_ref()
            .and_then(|meta| meta.authorization_servers.first().cloned())
            .unwrap_or_else(|| server.origin().ascii_serialization());
        let issuer = Url::parse(&issuer).map_err(|err| failed("authorization server", err))?;
        let mut candidates = well_known(&issuer, "oauth-authorization-server");
        candidates.extend(well_known(&issuer, "openid-configuration"));
        let mut metadata = None;
        for url in candidates {
            if let Some(found) = self.json::<ServerMetadata>(&url).await {
                metadata = Some(found);
                break;
            }
        }
        let metadata = metadata.ok_or_else(|| {
            failed(
                "discovery",
                format!("no authorization server metadata at {issuer}"),
            )
        })?;
        let scope = scope.or_else(|| {
            resource
                .as_ref()
                .filter(|meta| !meta.scopes_supported.is_empty())
                .map(|meta| meta.scopes_supported.join(" "))
        });
        let resource = resource
            .and_then(|meta| meta.resource)
            .unwrap_or_else(|| server.to_string());
        Ok((resource, metadata, scope))
    }

    /// Signs in to `server` through the browser; `say` reports progress.
    pub async fn login(&self, server: &str, say: &(dyn Fn(&str) + Sync)) -> Result<()> {
        let url = Url::parse(server).map_err(|err| failed("server URL", err))?;
        let (resource, metadata, scope) = self.discover(&url).await?;
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await?;
        let redirect = format!(
            "http://127.0.0.1:{}/callback",
            listener.local_addr()?.port()
        );
        let registration = metadata.registration_endpoint.as_deref().ok_or_else(|| {
            failed(
                "registration",
                "服务器不支持动态客户端注册（dynamic client registration），无法自动登录；请联系服务器提供方，或改用该服务器的本地（stdio）版本",
            )
        })?;
        let registered: Value = self
            .client
            .post(registration)
            .json(&json!({
                "client_name": "cove",
                "redirect_uris": [redirect],
                "grant_types": ["authorization_code", "refresh_token"],
                "response_types": ["code"],
                "token_endpoint_auth_method": "none",
            }))
            .send()
            .await
            .map_err(|err| failed("registration", err))?
            .json()
            .await
            .map_err(|err| failed("registration response", err))?;
        let client_id = registered["client_id"]
            .as_str()
            .ok_or_else(|| failed("registration", "no client_id in the response"))?
            .to_owned();
        let client_secret = registered["client_secret"].as_str().map(str::to_owned);
        let verifier = random();
        let challenge = encode(&Sha256::digest(verifier.as_bytes()));
        let state = random();
        let mut authorize = Url::parse(&metadata.authorization_endpoint)
            .map_err(|err| failed("authorization endpoint", err))?;
        {
            let mut query = authorize.query_pairs_mut();
            query
                .append_pair("response_type", "code")
                .append_pair("client_id", &client_id)
                .append_pair("redirect_uri", &redirect)
                .append_pair("code_challenge", &challenge)
                .append_pair("code_challenge_method", "S256")
                .append_pair("state", &state)
                .append_pair("resource", &resource);
            if let Some(scope) = &scope {
                query.append_pair("scope", scope);
            }
        }
        say(&format!(
            "正在打开浏览器登录。如果没有自动打开，请访问：\n{authorize}\n"
        ));
        if self.browser {
            open_browser(authorize.as_str());
        }
        let code = tokio::time::timeout(LOGIN_TIMEOUT, callback(&listener, &state))
            .await
            .map_err(|_| failed("sign-in", "5 分钟内没有收到浏览器的回应"))??;
        let mut form = vec![
            ("grant_type", "authorization_code".to_owned()),
            ("code", code),
            ("redirect_uri", redirect),
            ("client_id", client_id.clone()),
            ("code_verifier", verifier),
            ("resource", resource.clone()),
        ];
        if let Some(secret) = &client_secret {
            form.push(("client_secret", secret.clone()));
        }
        let token = self.token(&metadata.token_endpoint, &form).await?;
        self.store(Saved {
            server: server.to_owned(),
            resource,
            token_endpoint: metadata.token_endpoint,
            client_id,
            client_secret,
            access_token: token.access_token,
            refresh_token: token.refresh_token,
            expires_at: token.expires_in.map(|seconds| now() + seconds),
        })
    }
}

/// Waits for the browser's redirect and returns the authorization code.
async fn callback(listener: &tokio::net::TcpListener, state: &str) -> Result<String> {
    loop {
        let (mut socket, _) = listener.accept().await?;
        let mut head = vec![0_u8; 8192];
        let read = socket.read(&mut head).await?;
        let request = String::from_utf8_lossy(&head[..read]).into_owned();
        let target = request.split_whitespace().nth(1).unwrap_or_default();
        let Ok(url) = Url::parse(&format!("http://127.0.0.1{target}")) else {
            continue;
        };
        if url.path() != "/callback" {
            let _ = socket
                .write_all(
                    b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                )
                .await;
            continue;
        }
        let pairs: HashMap<String, String> = url.query_pairs().into_owned().collect();
        let (status, body, outcome) = if pairs.get("state").map(String::as_str) != Some(state) {
            (
                "400 Bad Request",
                "登录状态不匹配；请回到 cove 重试。",
                None,
            )
        } else if let Some(code) = pairs.get("code") {
            (
                "200 OK",
                "登录成功。可以关闭此页面并回到 cove。",
                Some(Ok(code.clone())),
            )
        } else {
            let error = pairs
                .get("error_description")
                .or(pairs.get("error"))
                .cloned()
                .unwrap_or_else(|| "no code returned".into());
            (
                "400 Bad Request",
                "登录失败；原因请查看 cove。",
                Some(Err(failed("authorization", error))),
            )
        };
        let page =
            format!("<!doctype html><meta charset=utf-8><title>cove</title><p>{body}</p>");
        let _ = socket
            .write_all(
                format!(
                    "HTTP/1.1 {status}\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{page}",
                    page.len()
                )
                .as_bytes(),
            )
            .await;
        if let Some(outcome) = outcome {
            return outcome;
        }
    }
}

/// Opens `url` in the default browser without a shell (the URL carries `&`).
fn open_browser(url: &str) {
    let mut command = if cfg!(windows) {
        let mut command = std::process::Command::new("rundll32.exe");
        command.args(["url.dll,FileProtocolHandler", url]);
        command
    } else if cfg!(target_os = "macos") {
        let mut command = std::process::Command::new("open");
        command.arg(url);
        command
    } else {
        let mut command = std::process::Command::new("xdg-open");
        command.arg(url);
        command
    };
    let _ = command
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::null())
        .stderr(std::process::Stdio::null())
        .spawn();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn challenges_and_well_known_locations_parse() {
        let (metadata, scope) = challenge(
            r#"Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/v2/mcp", scope="read write""#,
        );
        assert_eq!(
            metadata.as_deref(),
            Some("https://mcp.example.com/.well-known/oauth-protected-resource/v2/mcp")
        );
        assert_eq!(scope.as_deref(), Some("read write"));
        let issuer = Url::parse("https://auth.example.com/tenant").unwrap();
        assert_eq!(
            well_known(&issuer, "oauth-authorization-server"),
            [
                "https://auth.example.com/.well-known/oauth-authorization-server/tenant",
                "https://auth.example.com/tenant/.well-known/oauth-authorization-server",
                "https://auth.example.com/.well-known/oauth-authorization-server",
            ]
        );
        assert_ne!(random(), random());
    }

    /// A whole sign-in against a local authorization server: discovery from the
    /// 401 challenge, registration, the browser redirect, the code exchange, the
    /// stored token and its refresh.
    #[tokio::test]
    async fn sign_in_registers_exchanges_and_refreshes() {
        use std::sync::Arc;
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let base = format!("http://127.0.0.1:{}", listener.local_addr().unwrap().port());
        let served = base.clone();
        let tokens = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let issued = tokens.clone();
        tokio::spawn(async move {
            loop {
                let Ok((mut socket, _)) = listener.accept().await else {
                    return;
                };
                let mut buffer = vec![0_u8; 16384];
                let read = socket.read(&mut buffer).await.unwrap_or(0);
                let request = String::from_utf8_lossy(&buffer[..read]).into_owned();
                let line = request.lines().next().unwrap_or_default().to_owned();
                let (status, extra, body) = if line.starts_with("POST /mcp") {
                    (
                        "401 Unauthorized",
                        format!("WWW-Authenticate: Bearer resource_metadata=\"{served}/prm\"\r\n"),
                        "{}".to_owned(),
                    )
                } else if line.starts_with("GET /prm") {
                    ("200 OK", String::new(), json!({"resource": format!("{served}/mcp"), "authorization_servers": [format!("{served}/as")], "scopes_supported": ["read", "offline_access"]}).to_string())
                } else if line.starts_with("GET /.well-known/oauth-authorization-server/as") {
                    ("200 OK", String::new(), json!({"authorization_endpoint": format!("{served}/authorize"), "token_endpoint": format!("{served}/token"), "registration_endpoint": format!("{served}/register")}).to_string())
                } else if line.starts_with("POST /register") {
                    (
                        "201 Created",
                        String::new(),
                        json!({"client_id": "client-1"}).to_string(),
                    )
                } else if line.starts_with("POST /token") {
                    assert!(request.contains("client_id=client-1"));
                    let n = issued.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    if n == 0 {
                        assert!(
                            request.contains("grant_type=authorization_code")
                                && request.contains("code=the-code")
                                && request.contains("code_verifier=")
                        );
                    } else {
                        assert!(
                            request.contains("grant_type=refresh_token")
                                && request.contains("refresh_token=refresh-1")
                        );
                    }
                    ("200 OK", String::new(), json!({"access_token": format!("access-{n}"), "refresh_token": "refresh-1", "expires_in": 3600}).to_string())
                } else {
                    ("404 Not Found", String::new(), String::new())
                };
                let _ = socket.write_all(format!("HTTP/1.1 {status}\r\nContent-Type: application/json\r\n{extra}Content-Length: {}\r\nConnection: close\r\n\r\n{body}", body.len()).as_bytes()).await;
            }
        });
        let dir = tempfile::tempdir().unwrap();
        let oauth = OAuth::new(dir.path().join("mcp-oauth")).without_browser();
        let server = format!("{base}/mcp");
        // Play the browser: follow the printed authorization URL to the redirect.
        let browser = |text: &str| {
            let Some(line) = text.lines().find(|line| line.starts_with("http")) else {
                return;
            };
            let authorize = Url::parse(line).unwrap();
            let pairs: HashMap<String, String> = authorize.query_pairs().into_owned().collect();
            assert_eq!(pairs["code_challenge_method"], "S256");
            assert_eq!(pairs["scope"], "read offline_access");
            assert_eq!(pairs["resource"], format!("{base}/mcp"));
            let redirect = format!(
                "{}?code=the-code&state={}",
                pairs["redirect_uri"], pairs["state"]
            );
            tokio::spawn(async move {
                let _ = reqwest::get(redirect).await;
            });
        };
        oauth.login(&server, &browser).await.unwrap();
        assert!(oauth.signed_in(&server));
        assert_eq!(
            oauth.bearer(&server, false).await.unwrap().as_deref(),
            Some("access-0")
        );
        assert_eq!(
            oauth.bearer(&server, true).await.unwrap().as_deref(),
            Some("access-1")
        );
        assert!(oauth.logout(&server).unwrap());
        assert!(oauth.bearer(&server, false).await.unwrap().is_none());
    }
}
