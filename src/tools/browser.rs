//! Headless Chrome/Edge over a lean CDP client (tokio-tungstenite), used by the
//! `browser` tool and by browser verification workflows.
//!
//! Every request of every target (pages, frames, workers) is paused through the
//! CDP Fetch domain and checked before it leaves the browser: only public
//! addresses pass (plus loopback when the operator explicitly allows it);
//! `file:`, `chrome:` and other schemes are refused, WebSockets are blocked and
//! downloads denied. The browser runs in a throwaway profile inside the process
//! tree guard and is killed when the session ends.
//!
//! Residual risk: the address check resolves the host itself, so a DNS answer
//! that changes between the check and Chrome's own lookup is not covered.

use std::{
    collections::{HashMap, HashSet},
    net::IpAddr,
    path::{Path, PathBuf},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering},
    },
    time::Duration,
};

use base64::{Engine, engine::general_purpose::STANDARD as BASE64};
use futures_util::{SinkExt, StreamExt, stream::SplitSink};
use serde_json::{Value, json};
use tokio::{net::TcpStream, sync::oneshot};
use tokio_tungstenite::{MaybeTlsStream, WebSocketStream, tungstenite::Message};
use tokio_util::sync::CancellationToken;
use url::Url;

use crate::{
    domain::{Error, Result},
    process::ManagedChild,
};

const CALL_TIMEOUT: Duration = Duration::from_secs(30);

/// Installed Chrome or Edge: `COVE_BROWSER`, then the configured path, then well-known locations.
pub fn detect(configured: Option<&Path>) -> Option<PathBuf> {
    if let Some(path) = std::env::var_os("COVE_BROWSER").map(PathBuf::from) {
        return path.is_file().then_some(path);
    }
    if let Some(path) = configured {
        return path.is_file().then(|| path.to_path_buf());
    }
    let mut candidates: Vec<PathBuf> = Vec::new();
    if cfg!(windows) {
        for base in ["ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"] {
            if let Some(base) = std::env::var_os(base).map(PathBuf::from) {
                candidates.push(base.join(r"Google\Chrome\Application\chrome.exe"));
                candidates.push(base.join(r"Microsoft\Edge\Application\msedge.exe"));
                candidates.push(base.join(r"Chromium\Application\chrome.exe"));
            }
        }
    } else if cfg!(target_os = "macos") {
        candidates.push("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome".into());
        candidates.push("/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge".into());
        candidates.push("/Applications/Chromium.app/Contents/MacOS/Chromium".into());
    } else if let Some(path) = std::env::var_os("PATH") {
        for dir in std::env::split_paths(&path) {
            for name in [
                "google-chrome",
                "google-chrome-stable",
                "chromium",
                "chromium-browser",
                "microsoft-edge",
            ] {
                candidates.push(dir.join(name));
            }
        }
    }
    candidates.into_iter().find(|path| path.is_file())
}

/// Which hosts the browser may reach.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Reach {
    PublicOnly,
    /// Public plus loopback (`--allow-local` in verification). Never private LAN.
    PublicAndLoopback,
}

fn address_allowed(ip: IpAddr, reach: Reach) -> bool {
    crate::tools::web::is_public(ip) || (reach == Reach::PublicAndLoopback && ip.is_loopback())
}

/// `Ok(())` when a page request may proceed; `Err(reason)` otherwise.
pub async fn request_allowed(raw: &str, reach: Reach) -> std::result::Result<(), String> {
    let url = Url::parse(raw.trim()).map_err(|_| "unparsable URL".to_owned())?;
    match url.scheme() {
        "data" | "blob" | "about" => return Ok(()),
        "http" | "https" | "ws" | "wss" => {}
        other => return Err(format!("scheme {other}: is not allowed")),
    }
    if !url.username().is_empty() || url.password().is_some() {
        return Err("URLs with credentials are refused".into());
    }
    let port = url.port_or_known_default().unwrap_or(443);
    let addresses: Vec<IpAddr> = match url.host() {
        Some(url::Host::Ipv4(ip)) => vec![ip.into()],
        Some(url::Host::Ipv6(ip)) => vec![ip.into()],
        Some(url::Host::Domain(domain)) => {
            let domain = domain.trim_end_matches('.').to_ascii_lowercase();
            if domain == "localhost" || domain.ends_with(".localhost") {
                vec![IpAddr::from([127, 0, 0, 1])]
            } else {
                tokio::time::timeout(
                    Duration::from_secs(10),
                    tokio::net::lookup_host((domain.as_str(), port)),
                )
                .await
                .map_err(|_| "DNS lookup timed out".to_owned())?
                .map_err(|_| "network_unavailable: DNS lookup failed".to_owned())?
                .map(|address| address.ip())
                .collect()
            }
        }
        None => return Err("URL has no host".into()),
    };
    if addresses.is_empty() || !addresses.iter().all(|ip| address_allowed(*ip, reach)) {
        return Err(format!(
            "{} resolves to a non-public address",
            url.host_str().unwrap_or("host")
        ));
    }
    Ok(())
}

type Sink = SplitSink<WebSocketStream<MaybeTlsStream<TcpStream>>, Message>;
type Pending = Mutex<HashMap<u64, oneshot::Sender<std::result::Result<Value, String>>>>;

/// Minimal CDP connection: numbered calls with replies, events to a channel.
struct Cdp {
    sink: tokio::sync::Mutex<Sink>,
    pending: Arc<Pending>,
    next: AtomicU64,
    reader: tokio::task::JoinHandle<()>,
}

impl Cdp {
    async fn connect(
        url: &str,
    ) -> Result<(Arc<Self>, tokio::sync::mpsc::UnboundedReceiver<Value>)> {
        let (socket, _) = tokio::time::timeout(
            Duration::from_secs(10),
            tokio_tungstenite::connect_async(url),
        )
        .await
        .map_err(|_| Error::Tool("browser DevTools connection timed out".into()))?
        .map_err(|err| Error::Tool(format!("browser DevTools connection failed: {err}")))?;
        let (sink, mut stream) = socket.split();
        let pending: Arc<Pending> = Arc::default();
        let (events, receiver) = tokio::sync::mpsc::unbounded_channel();
        let replies = pending.clone();
        let reader = tokio::spawn(async move {
            while let Some(Ok(message)) = stream.next().await {
                let Message::Text(text) = message else {
                    continue;
                };
                let Ok(value) = serde_json::from_str::<Value>(&text) else {
                    continue;
                };
                match value.get("id").and_then(Value::as_u64) {
                    Some(id) => {
                        let sender = replies.lock().ok().and_then(|mut map| map.remove(&id));
                        if let Some(sender) = sender {
                            let reply = match value.get("error") {
                                Some(error) => Err(error
                                    .get("message")
                                    .and_then(Value::as_str)
                                    .unwrap_or("CDP error")
                                    .to_owned()),
                                None => Ok(value.get("result").cloned().unwrap_or(Value::Null)),
                            };
                            let _ = sender.send(reply);
                        }
                    }
                    None => {
                        let _ = events.send(value);
                    }
                }
            }
            if let Ok(mut map) = replies.lock() {
                map.clear();
            }
        });
        Ok((
            Arc::new(Self {
                sink: tokio::sync::Mutex::new(sink),
                pending,
                next: AtomicU64::new(1),
                reader,
            }),
            receiver,
        ))
    }

    async fn call(&self, method: &str, params: Value, session: Option<&str>) -> Result<Value> {
        let id = self.next.fetch_add(1, Ordering::Relaxed);
        let mut message = json!({"id": id, "method": method, "params": params});
        if let Some(session) = session {
            message["sessionId"] = json!(session);
        }
        let (sender, receiver) = oneshot::channel();
        self.pending
            .lock()
            .map_err(|_| Error::Tool("CDP state poisoned".into()))?
            .insert(id, sender);
        self.sink
            .lock()
            .await
            .send(Message::Text(message.to_string().into()))
            .await
            .map_err(|err| Error::Tool(format!("CDP send failed: {err}")))?;
        match tokio::time::timeout(CALL_TIMEOUT, receiver).await {
            Ok(Ok(Ok(value))) => Ok(value),
            Ok(Ok(Err(message))) => Err(Error::Tool(format!("{method}: {message}"))),
            Ok(Err(_)) => Err(Error::Tool("browser connection closed".into())),
            Err(_) => {
                if let Ok(mut map) = self.pending.lock() {
                    map.remove(&id);
                }
                Err(Error::Tool(format!("{method} timed out")))
            }
        }
    }
}

impl Drop for Cdp {
    fn drop(&mut self) {
        self.reader.abort();
    }
}

/// What the request guard saw during a session.
#[derive(Default)]
pub struct Guard {
    blocked: AtomicUsize,
    document_blocked: AtomicBool,
    network_failed: AtomicBool,
    reasons: Mutex<Vec<String>>,
    /// Browser context whose refusals are counted (the page under test);
    /// refusals of other targets (built-in components) are blocked silently.
    focus: Mutex<Option<String>>,
    contexts: Mutex<HashMap<String, String>>,
}

impl Guard {
    pub fn blocked(&self) -> usize {
        self.blocked.load(Ordering::Relaxed)
    }

    pub fn reset(&self) {
        self.blocked.store(0, Ordering::Relaxed);
        self.document_blocked.store(false, Ordering::Relaxed);
        self.network_failed.store(false, Ordering::Relaxed);
        if let Ok(mut reasons) = self.reasons.lock() {
            reasons.clear();
        }
    }

    pub fn network_failed(&self) -> bool {
        self.network_failed.load(Ordering::Relaxed)
    }

    pub fn reasons(&self) -> Vec<String> {
        self.reasons.lock().map(|r| r.clone()).unwrap_or_default()
    }

    fn counts(&self, session: Option<&str>) -> bool {
        let focus = self.focus.lock().ok().and_then(|focus| focus.clone());
        let context = session.and_then(|session| {
            self.contexts
                .lock()
                .ok()
                .and_then(|map| map.get(session).cloned())
        });
        focus.is_none() || focus == context
    }

    fn note(&self, reason: String, document: bool) {
        self.blocked.fetch_add(1, Ordering::Relaxed);
        if document {
            self.document_blocked.store(true, Ordering::Relaxed);
        }
        if reason.starts_with("network_unavailable") {
            self.network_failed.store(true, Ordering::Relaxed);
        }
        if let Ok(mut reasons) = self.reasons.lock()
            && reasons.len() < 8
        {
            reasons.push(reason);
        }
    }
}

/// A running headless browser with the request guard attached to every target.
pub struct Browser {
    cdp: Arc<Cdp>,
    child: tokio::sync::Mutex<ManagedChild>,
    _profile: tempfile::TempDir,
    sessions: Arc<Mutex<HashMap<String, String>>>,
    pub guard: Arc<Guard>,
    interceptor: tokio::task::JoinHandle<()>,
}

pub struct Page<'a> {
    browser: &'a Browser,
    session: String,
    context: String,
}

impl Browser {
    pub async fn launch(
        executable: &Path,
        reach: Reach,
        cancel: &CancellationToken,
    ) -> Result<Self> {
        let profile = tempfile::Builder::new()
            .prefix("cove-browser-")
            .tempdir()?;
        let args: Vec<String> = [
            "--headless=new",
            "--remote-debugging-port=0",
            "--no-first-run",
            "--no-default-browser-check",
            "--disable-extensions",
            "--disable-background-networking",
            "--disable-component-update",
            "--disable-sync",
            "--disable-default-apps",
            "--disable-features=Translate,MediaRouter,OptimizationHints",
            "--mute-audio",
            "--hide-scrollbars",
            "--disable-gpu",
            "--password-store=basic",
        ]
        .into_iter()
        .map(String::from)
        .chain([
            format!(
                "--user-data-dir={}",
                crate::workflows::changes::plain(profile.path()).display()
            ),
            "about:blank".into(),
        ])
        .collect();
        let program = executable
            .to_str()
            .ok_or_else(|| Error::Tool("browser path is not valid UTF-8".into()))?;
        let child = ManagedChild::spawn(
            &std::env::current_exe()?,
            profile.path(),
            program,
            &args,
            &[],
            cancel,
        )
        .await?;
        let port_file = profile.path().join("DevToolsActivePort");
        let deadline = tokio::time::Instant::now() + Duration::from_secs(20);
        let endpoint = loop {
            if cancel.is_cancelled() {
                return Err(Error::Cancelled);
            }
            if let Ok(text) = std::fs::read_to_string(&port_file) {
                let mut lines = text.lines();
                if let (Some(port), Some(path)) = (lines.next(), lines.next())
                    && port.trim().parse::<u16>().is_ok()
                {
                    break format!("ws://127.0.0.1:{}{}", port.trim(), path.trim());
                }
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(Error::Tool("headless browser did not start in 20 s".into()));
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        };
        let (cdp, mut events) = Cdp::connect(&endpoint).await?;
        let sessions: Arc<Mutex<HashMap<String, String>>> = Arc::default();
        let guard = Arc::new(Guard::default());
        let interceptor = {
            let cdp = cdp.clone();
            let sessions = sessions.clone();
            let guard = guard.clone();
            tokio::spawn(async move {
                let mut seen = HashSet::new();
                while let Some(event) = events.recv().await {
                    let method = event.get("method").and_then(Value::as_str).unwrap_or("");
                    let params = event.get("params").cloned().unwrap_or(Value::Null);
                    let session = event
                        .get("sessionId")
                        .and_then(Value::as_str)
                        .map(str::to_owned);
                    match method {
                        "Target.attachedToTarget" => {
                            let Some(child) = params.get("sessionId").and_then(Value::as_str)
                            else {
                                continue;
                            };
                            if !seen.insert(child.to_owned()) {
                                continue;
                            }
                            let child = child.to_owned();
                            let kind = params
                                .pointer("/targetInfo/type")
                                .and_then(Value::as_str)
                                .unwrap_or("")
                                .to_owned();
                            let target = params
                                .pointer("/targetInfo/targetId")
                                .and_then(Value::as_str)
                                .unwrap_or("")
                                .to_owned();
                            if let (Some(context), Ok(mut map)) = (
                                params
                                    .pointer("/targetInfo/browserContextId")
                                    .and_then(Value::as_str),
                                guard.contexts.lock(),
                            ) {
                                map.insert(child.clone(), context.to_owned());
                            }
                            let cdp = cdp.clone();
                            let sessions = sessions.clone();
                            tokio::spawn(async move {
                                attach(&cdp, &child, &kind, &target).await;
                                if let Ok(mut map) = sessions.lock() {
                                    map.insert(target, child);
                                }
                            });
                        }
                        "Fetch.requestPaused" => {
                            let cdp = cdp.clone();
                            let guard = guard.clone();
                            tokio::spawn(async move {
                                let id = params
                                    .get("requestId")
                                    .and_then(Value::as_str)
                                    .unwrap_or("")
                                    .to_owned();
                                let url = params
                                    .pointer("/request/url")
                                    .and_then(Value::as_str)
                                    .unwrap_or("");
                                let document = params.get("resourceType").and_then(Value::as_str)
                                    == Some("Document");
                                let session = session.as_deref();
                                match request_allowed(url, reach).await {
                                    Ok(()) => {
                                        let _ = cdp
                                            .call(
                                                "Fetch.continueRequest",
                                                json!({"requestId": id}),
                                                session,
                                            )
                                            .await;
                                    }
                                    Err(reason) => {
                                        if guard.counts(session) {
                                            guard.note(reason, document);
                                        }
                                        let _ = cdp
                                            .call(
                                                "Fetch.failRequest",
                                                json!({"requestId": id, "errorReason": "BlockedByClient"}),
                                                session,
                                            )
                                            .await;
                                    }
                                }
                            });
                        }
                        _ => {}
                    }
                }
            })
        };
        let browser = Self {
            cdp,
            child: tokio::sync::Mutex::new(child),
            _profile: profile,
            sessions,
            guard,
            interceptor,
        };
        browser
            .cdp
            .call(
                "Target.setAutoAttach",
                json!({"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}),
                None,
            )
            .await?;
        browser
            .cdp
            .call(
                "Browser.setDownloadBehavior",
                json!({"behavior": "deny"}),
                None,
            )
            .await?;
        Ok(browser)
    }

    /// A fresh incognito-like context with one page at `width`x`height`.
    pub async fn page(&self, width: u32, height: u32, mobile: bool) -> Result<Page<'_>> {
        let context = self
            .cdp
            .call(
                "Target.createBrowserContext",
                json!({"disposeOnDetach": true}),
                None,
            )
            .await?["browserContextId"]
            .as_str()
            .unwrap_or("")
            .to_owned();
        if let Ok(mut focus) = self.guard.focus.lock() {
            *focus = Some(context.clone());
        }
        let target = self
            .cdp
            .call(
                "Target.createTarget",
                json!({"url": "about:blank", "browserContextId": context}),
                None,
            )
            .await?["targetId"]
            .as_str()
            .unwrap_or("")
            .to_owned();
        let deadline = tokio::time::Instant::now() + Duration::from_secs(10);
        let session = loop {
            if let Some(session) = self
                .sessions
                .lock()
                .ok()
                .and_then(|map| map.get(&target).cloned())
            {
                break session;
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(Error::Tool("browser page did not attach".into()));
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        };
        let page = Page {
            browser: self,
            session,
            context,
        };
        page.call(
            "Emulation.setDeviceMetricsOverride",
            json!({"width": width, "height": height, "deviceScaleFactor": 1, "mobile": mobile}),
        )
        .await?;
        if mobile {
            page.call(
                "Emulation.setTouchEmulationEnabled",
                json!({"enabled": true}),
            )
            .await?;
        }
        Ok(page)
    }

    pub async fn close(self) {
        self.interceptor.abort();
        let _ = tokio::time::timeout(
            Duration::from_secs(3),
            self.cdp.call("Browser.close", json!({}), None),
        )
        .await;
        let _ = self.child.lock().await.stop().await;
    }
}

/// Fetch interception, WebSocket blocking and nested auto-attach for a new target,
/// then lets it run. A page or frame whose interception fails is closed.
async fn attach(cdp: &Cdp, session: &str, kind: &str, target: &str) {
    let fetch = cdp
        .call(
            "Fetch.enable",
            json!({"patterns": [{"urlPattern": "*", "requestStage": "Request"}]}),
            Some(session),
        )
        .await;
    if matches!(kind, "page" | "iframe") {
        if fetch.is_err() {
            let _ = cdp
                .call("Target.closeTarget", json!({"targetId": target}), None)
                .await;
            return;
        }
        let _ = cdp.call("Network.enable", json!({}), Some(session)).await;
        let _ = cdp
            .call(
                "Network.setBlockedURLs",
                json!({"urls": ["ws://*", "wss://*"]}),
                Some(session),
            )
            .await;
    }
    let _ = cdp
        .call(
            "Target.setAutoAttach",
            json!({"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true}),
            Some(session),
        )
        .await;
    let _ = cdp
        .call("Runtime.runIfWaitingForDebugger", json!({}), Some(session))
        .await;
}

impl Page<'_> {
    async fn call(&self, method: &str, params: Value) -> Result<Value> {
        self.browser
            .cdp
            .call(method, params, Some(&self.session))
            .await
    }

    /// Evaluates `expression` and returns its JSON value; page exceptions are errors.
    pub async fn eval(&self, expression: &str) -> Result<Value> {
        let result = self
            .call(
                "Runtime.evaluate",
                json!({"expression": expression, "returnByValue": true, "awaitPromise": true}),
            )
            .await?;
        if let Some(exception) = result.get("exceptionDetails") {
            return Err(Error::Tool(format!(
                "page script failed: {}",
                exception
                    .pointer("/exception/description")
                    .and_then(Value::as_str)
                    .unwrap_or("exception")
                    .lines()
                    .next()
                    .unwrap_or("")
            )));
        }
        Ok(result
            .pointer("/result/value")
            .cloned()
            .unwrap_or(Value::Null))
    }

    /// Navigates and waits for `document.readyState == "complete"` (bounded).
    pub async fn navigate(&self, url: &str, timeout: Duration) -> Result<()> {
        let result = self.call("Page.navigate", json!({"url": url})).await?;
        if let Some(error) = result.get("errorText").and_then(Value::as_str) {
            if self.browser.guard.document_blocked.load(Ordering::Relaxed) {
                return Err(Error::Approval(format!(
                    "请求防护拒绝了这次浏览器导航：{}",
                    self.browser.guard.reasons().join("; ")
                )));
            }
            return Err(Error::Tool(format!(
                "network_unavailable: navigation failed: {error}"
            )));
        }
        self.settle(timeout).await
    }

    pub async fn settle(&self, timeout: Duration) -> Result<()> {
        let deadline = tokio::time::Instant::now() + timeout;
        loop {
            if self
                .eval("document.readyState")
                .await
                .ok()
                .as_ref()
                .and_then(Value::as_str)
                == Some("complete")
            {
                // A short pause lets late scripts render.
                tokio::time::sleep(Duration::from_millis(150)).await;
                return Ok(());
            }
            if tokio::time::Instant::now() >= deadline {
                return Err(Error::Tool("page did not finish loading in time".into()));
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }

    pub async fn html(&self) -> Result<String> {
        Ok(self
            .eval("document.documentElement ? document.documentElement.outerHTML : ''")
            .await?
            .as_str()
            .unwrap_or("")
            .to_owned())
    }

    /// PNG of the page, up to `max_height` CSS pixels tall.
    pub async fn screenshot(&self, max_height: u32) -> Result<Vec<u8>> {
        let metrics = self.call("Page.getLayoutMetrics", json!({})).await?;
        let width = metrics
            .pointer("/cssLayoutViewport/clientWidth")
            .and_then(Value::as_f64)
            .unwrap_or(1280.0);
        let height = metrics
            .pointer("/cssContentSize/height")
            .and_then(Value::as_f64)
            .unwrap_or(800.0)
            .clamp(1.0, f64::from(max_height));
        let result = self
            .call(
                "Page.captureScreenshot",
                json!({"format": "png", "captureBeyondViewport": true,
                    "clip": {"x": 0, "y": 0, "width": width, "height": height, "scale": 1}}),
            )
            .await?;
        BASE64
            .decode(result["data"].as_str().unwrap_or(""))
            .map_err(|_| Error::Tool("invalid screenshot data".into()))
    }

    pub async fn close(self) {
        let _ = self
            .browser
            .cdp
            .call(
                "Target.disposeBrowserContext",
                json!({"browserContextId": self.context}),
                None,
            )
            .await;
    }
}

/// HTML to Markdown-ish text: headings, links, list items and code survive.
pub fn html_to_markdown(html: &str) -> String {
    use regex::Regex;
    use std::sync::LazyLock;
    static HEADING: LazyLock<Regex> =
        LazyLock::new(|| Regex::new(r"(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]\s*>").unwrap());
    static LINK: LazyLock<Regex> = LazyLock::new(|| {
        Regex::new(r#"(?is)<a\b[^>]*?href\s*=\s*["']([^"']*)["'][^>]*>(.*?)</a\s*>"#).unwrap()
    });
    static STRONG: LazyLock<Regex> =
        LazyLock::new(|| Regex::new(r"(?is)<(strong|b)\b[^>]*>(.*?)</(strong|b)\s*>").unwrap());
    static CODE: LazyLock<Regex> =
        LazyLock::new(|| Regex::new(r"(?is)<code\b[^>]*>(.*?)</code\s*>").unwrap());
    static TAGS: LazyLock<Regex> = LazyLock::new(|| Regex::new(r"(?s)<[^>]*>").unwrap());
    let text = HEADING.replace_all(html, |caps: &regex::Captures| {
        let level: usize = caps[1].parse().unwrap_or(1);
        let inner = TAGS.replace_all(&caps[2], "");
        format!("<p>{} {}</p>", "#".repeat(level), inner.trim())
    });
    let text = LINK.replace_all(&text, |caps: &regex::Captures| {
        let inner = TAGS.replace_all(&caps[2], "");
        let href = &caps[1];
        if href.starts_with("javascript:") || inner.trim().is_empty() {
            inner.trim().to_owned()
        } else {
            format!("[{}]({href})", inner.trim())
        }
    });
    let text = STRONG.replace_all(&text, "**$2**");
    let text = CODE.replace_all(&text, "`$1`");
    crate::tools::web::html_to_text(&text)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn guard_refuses_private_loopback_and_local_schemes() {
        for url in [
            "http://127.0.0.1:8080/",
            "http://localhost/",
            "http://10.0.0.1/",
            "http://169.254.169.254/latest/meta-data",
            "http://[::1]/",
            "file:///etc/passwd",
            "chrome://settings",
            "https://user:pw@example.com/",
            "ws://192.168.1.1/",
        ] {
            assert!(
                request_allowed(url, Reach::PublicOnly).await.is_err(),
                "{url}"
            );
        }
        for url in ["data:text/html,hi", "about:blank", "https://93.184.215.14/"] {
            assert!(
                request_allowed(url, Reach::PublicOnly).await.is_ok(),
                "{url}"
            );
        }
        assert!(
            request_allowed("http://127.0.0.1:3000/", Reach::PublicAndLoopback)
                .await
                .is_ok()
        );
        assert!(
            request_allowed("http://localhost:3000/", Reach::PublicAndLoopback)
                .await
                .is_ok()
        );
        // Explicit local access never extends to the private LAN.
        assert!(
            request_allowed("http://192.168.0.10/", Reach::PublicAndLoopback)
                .await
                .is_err()
        );
    }

    #[test]
    fn markdown_keeps_headings_links_and_lists() {
        let md = html_to_markdown(
            r#"<h1>Title</h1><p>See <a href="https://x.test/a">the <b>docs</b></a></p><ul><li>one</li></ul><script>evil()</script>"#,
        );
        assert!(md.contains("# Title"), "{md}");
        assert!(md.contains("[the docs](https://x.test/a)"), "{md}");
        assert!(md.contains("- one") && !md.contains("evil"), "{md}");
    }
}
