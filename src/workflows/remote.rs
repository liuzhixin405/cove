//! Remote supervision: a small authenticated HTTP control API for the running
//! REPL (status, steer, cancel, pause/resume, approve/deny a pending prompt).
//!
//! - Binds loopback by default; a private LAN address needs `--allow-lan` plus a
//!   TLS certificate and key. Wildcard and public binds are refused.
//! - Every request needs `Authorization: Bearer <token>`; the token is written to
//!   an owner-only file and never printed. Host must be the bound authority and a
//!   present Origin must match it, which defeats DNS rebinding and cross-site posts.
//! - Actions carry a request ID: replays return the stored result, a reused ID
//!   with a different body is rejected.
//! - A remote approval answers only the currently pending prompt, once, within
//!   five minutes; the local prompt stays usable and whichever answers first wins.
//!   It never saves a rule or changes the permission mode.

use std::{
    collections::{HashMap, VecDeque},
    net::{IpAddr, SocketAddr},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use futures_util::future::BoxFuture;
use http_body_util::{BodyExt, Full, Limited};
use hyper::{Request, Response, StatusCode, body::Bytes, header};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::sync::oneshot;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    engine::turn::TurnHook,
    safety::permission::{Authorizer, Decision, Effect, Permissions},
    session::Session,
    tools::ToolOutput,
};

pub const APPROVAL_TTL: Duration = Duration::from_secs(300);
const ACTION_HISTORY: usize = 256;
const BODY_LIMIT: usize = 16 * 1024;

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct Action {
    pub id: String,
    pub kind: ActionKind,
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub approval_id: Option<String>,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum ActionKind {
    Steer,
    Cancel,
    Pause,
    Resume,
    Approve,
    Deny,
}

#[derive(Clone, Debug, Serialize)]
pub struct ActionResult {
    pub id: String,
    pub kind: ActionKind,
    /// `applied` or `rejected`.
    pub state: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
    pub at: u64,
}

struct PendingApproval {
    id: String,
    summary: String,
    created: Instant,
    expires_unix: u64,
    answer: Option<oneshot::Sender<bool>>,
}

#[derive(Default)]
struct State {
    active: bool,
    session: String,
    project: String,
    running: bool,
    task: u64,
    paused: bool,
    cancel: Option<CancellationToken>,
    steer: VecDeque<String>,
    pending: Option<PendingApproval>,
    actions: HashMap<String, (Action, ActionResult)>,
    order: VecDeque<String>,
}

/// Shared between the REPL (task lifecycle, authorizer, steer hook) and the server.
#[derive(Default)]
pub struct Hub {
    state: Mutex<State>,
    resumed: tokio::sync::Notify,
}

fn valid_request_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-' || b == b'_')
}

impl Hub {
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state
            .lock()
            .unwrap_or_else(|poison| poison.into_inner())
    }

    pub fn active(&self) -> bool {
        self.lock().active
    }

    fn set_active(&self, active: bool) {
        let mut state = self.lock();
        state.active = active;
        if !active {
            // Stopping withdraws remote approvals; the local prompt keeps waiting.
            state.pending = None;
            state.paused = false;
            state.steer.clear();
            drop(state);
            self.resumed.notify_waiters();
        }
    }

    pub fn set_scope(&self, session: &str, project: &Path) {
        let mut state = self.lock();
        state.session = session.into();
        state.project = crate::workflows::changes::plain(project)
            .display()
            .to_string();
    }

    pub fn task_started(&self, cancel: &CancellationToken) {
        let mut state = self.lock();
        state.running = true;
        state.task += 1;
        state.cancel = Some(cancel.clone());
    }

    pub fn task_ended(&self) {
        let mut state = self.lock();
        state.running = false;
        state.cancel = None;
        state.pending = None;
        state.steer.clear();
    }

    pub fn take_steer(&self) -> Option<String> {
        let mut state = self.lock();
        if state.steer.is_empty() {
            return None;
        }
        Some(state.steer.drain(..).collect::<Vec<_>>().join("\n"))
    }

    /// Waits while paused; cancellation ends the wait.
    pub async fn wait_unpaused(&self, cancel: &CancellationToken) -> Result<()> {
        loop {
            let resumed = self.resumed.notified();
            if !self.lock().paused {
                return Ok(());
            }
            tokio::select! {
                _ = cancel.cancelled() => return Err(Error::Cancelled),
                _ = resumed => {}
                // Re-check periodically in case a notification raced.
                _ = tokio::time::sleep(Duration::from_millis(500)) => {}
            }
        }
    }

    fn offer(&self, summary: String) -> Option<(String, oneshot::Receiver<bool>)> {
        let mut state = self.lock();
        if !state.active {
            return None;
        }
        let (sender, receiver) = oneshot::channel();
        let id = crate::session::new_id();
        state.pending = Some(PendingApproval {
            id: id.clone(),
            summary,
            created: Instant::now(),
            expires_unix: crate::session::now() + APPROVAL_TTL.as_secs(),
            answer: Some(sender),
        });
        Some((id, receiver))
    }

    fn withdraw(&self, id: &str) {
        let mut state = self.lock();
        if state.pending.as_ref().is_some_and(|p| p.id == id) {
            state.pending = None;
        }
    }

    pub fn snapshot(&self) -> Value {
        let mut state = self.lock();
        if state
            .pending
            .as_ref()
            .is_some_and(|p| p.created.elapsed() >= APPROVAL_TTL)
        {
            state.pending = None;
        }
        json!({
            "session": state.session,
            "project": state.project,
            "running": state.running,
            "task": state.task,
            "paused": state.paused,
            "steer_queued": state.steer.len(),
            "pending": state.pending.as_ref().map(|p| json!({
                "id": p.id, "summary": p.summary, "expires_at": p.expires_unix
            })),
        })
    }

    pub fn action(&self, id: &str) -> Option<ActionResult> {
        self.lock()
            .actions
            .get(id)
            .map(|(_, result)| result.clone())
    }

    /// Applies an action once per request ID. `Err` for a reused ID with another body.
    pub fn submit(&self, action: Action) -> std::result::Result<ActionResult, String> {
        if !valid_request_id(&action.id) {
            return Err("request id must be 1..64 of [A-Za-z0-9_-]".into());
        }
        let mut state = self.lock();
        if let Some((previous, result)) = state.actions.get(&action.id) {
            return if *previous == action {
                Ok(result.clone())
            } else {
                Err("request id already used for a different action".into())
            };
        }
        let outcome: std::result::Result<(), String> = match action.kind {
            ActionKind::Steer => match action.text.as_deref().map(str::trim) {
                Some(text) if !text.is_empty() && text.len() <= 4096 => {
                    if !state.running {
                        Err("no task is running".into())
                    } else if state.steer.len() >= 16 {
                        Err("too many queued steer messages".into())
                    } else {
                        state.steer.push_back(text.to_owned());
                        Ok(())
                    }
                }
                _ => Err("steer needs text of 1..4096 bytes".into()),
            },
            ActionKind::Cancel => match &state.cancel {
                Some(cancel) if state.running => {
                    cancel.cancel();
                    Ok(())
                }
                _ => Err("no task is running".into()),
            },
            ActionKind::Pause => {
                state.paused = true;
                Ok(())
            }
            ActionKind::Resume => {
                state.paused = false;
                self.resumed.notify_waiters();
                Ok(())
            }
            ActionKind::Approve | ActionKind::Deny => {
                let matches = state
                    .pending
                    .as_ref()
                    .is_some_and(|p| Some(p.id.as_str()) == action.approval_id.as_deref());
                if !matches {
                    Err("no matching pending approval".into())
                } else if state
                    .pending
                    .as_ref()
                    .is_some_and(|p| p.created.elapsed() >= APPROVAL_TTL)
                {
                    state.pending = None;
                    Err("approval expired".into())
                } else {
                    let mut pending = state.pending.take().expect("checked above");
                    let approved = action.kind == ActionKind::Approve;
                    match pending.answer.take().map(|answer| answer.send(approved)) {
                        Some(Ok(())) => Ok(()),
                        _ => Err("the prompt was already answered".into()),
                    }
                }
            }
        };
        let result = ActionResult {
            id: action.id.clone(),
            kind: action.kind,
            state: if outcome.is_ok() {
                "applied"
            } else {
                "rejected"
            },
            error: outcome.err(),
            at: crate::session::now(),
        };
        state.order.push_back(action.id.clone());
        state
            .actions
            .insert(action.id.clone(), (action, result.clone()));
        while state.order.len() > ACTION_HISTORY {
            if let Some(old) = state.order.pop_front() {
                state.actions.remove(&old);
            }
        }
        Ok(result)
    }
}

/// The local authorizer plus remote pause and approvals.
/// Lets a remote supervisor answer the approvals `inner` would ask the local user.
pub struct RemoteAuthorizer {
    pub inner: Arc<Permissions>,
    pub hub: Arc<Hub>,
}

impl Authorizer for RemoteAuthorizer {
    fn authorize(&self, effect: Effect, cancel: CancellationToken) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            // A remote pause holds the task at its next tool boundary.
            self.hub.wait_unpaused(&cancel).await?;
            // Only effects the rules and mode would ask about are offered remotely.
            if matches!(effect, Effect::Read) || self.inner.decide(&effect) != Decision::Ask {
                return self.inner.authorize(effect, cancel).await;
            }
            let summary = effect.describe();
            let Some((id, remote)) = self.hub.offer(summary.clone()) else {
                return self.inner.authorize(effect, cancel).await;
            };
            let remote_answer = async {
                tokio::time::timeout(APPROVAL_TTL, remote)
                    .await
                    .ok()
                    .and_then(|answer| answer.ok())
            };
            let result = if !self.inner.interactive() {
                tokio::select! {
                    _ = cancel.cancelled() => Err(Error::Cancelled),
                    answer = remote_answer => match answer {
                        Some(true) => Ok(()),
                        _ => Err(Error::Approval(format!("{summary}（远程审批被拒绝或已过期）"))),
                    },
                }
            } else {
                let local_cancel = cancel.child_token();
                let local = self.inner.authorize(effect, local_cancel.clone());
                tokio::pin!(local);
                tokio::select! {
                    result = &mut local => result,
                    answer = remote_answer => match answer {
                        Some(approved) => {
                            // Stop the console prompt; it restores the terminal itself.
                            local_cancel.cancel();
                            crate::app::ui::print_text(
                                &format!("\n[远程] {}：{summary}\n", if approved { "已批准" } else { "已拒绝" }),
                                true,
                            )?;
                            if approved { Ok(()) } else { Err(Error::Denied(summary.clone())) }
                        }
                        // Expired or withdrawn: keep waiting for the local answer.
                        None => local.await,
                    },
                }
            };
            self.hub.withdraw(&id);
            result
        })
    }
}

/// Delivers remote steer messages into the running task.
pub struct SteerHook(pub Arc<Hub>);

const STEER_PREFIX: &str = "[operator steer via remote supervision; treat as a user instruction]";

impl TurnHook for SteerHook {
    fn tool_note(&self, _name: &str, _input: &Value, _output: &ToolOutput) -> Option<String> {
        self.0
            .take_steer()
            .map(|text| format!("{STEER_PREFIX}\n{text}"))
    }

    fn finish_nudge<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            self.0
                .take_steer()
                .map(|text| format!("{STEER_PREFIX}\n{text}"))
        })
    }
}

pub struct Options {
    pub bind: String,
    pub token_file: Option<PathBuf>,
    pub allow_lan: bool,
    pub tls_cert: Option<PathBuf>,
    pub tls_key: Option<PathBuf>,
}

impl Options {
    pub fn parse(args: &[&str]) -> Result<Self> {
        let mut options = Self {
            bind: "127.0.0.1:0".into(),
            token_file: None,
            allow_lan: false,
            tls_cert: None,
            tls_key: None,
        };
        let mut iter = args.iter();
        while let Some(arg) = iter.next() {
            let mut value = || {
                iter.next()
                    .map(|v| v.trim_matches('"').to_owned())
                    .ok_or_else(|| Error::Tool(format!("{arg} needs a value")))
            };
            match *arg {
                "--bind" => options.bind = value()?,
                "--token-file" => options.token_file = Some(value()?.into()),
                "--allow-lan" => options.allow_lan = true,
                "--tls-cert" => options.tls_cert = Some(value()?.into()),
                "--tls-key" => options.tls_key = Some(value()?.into()),
                other => return Err(Error::Tool(format!("unknown /remote start option {other}"))),
            }
        }
        Ok(options)
    }
}

pub struct Server {
    pub url: String,
    pub token_file: PathBuf,
    shutdown: CancellationToken,
    task: tokio::task::JoinHandle<()>,
    hub: Arc<Hub>,
}

fn validate_bind(options: &Options) -> Result<(SocketAddr, bool)> {
    let address: SocketAddr = options
        .bind
        .parse()
        .map_err(|_| Error::Tool("--bind needs a literal IP:port".into()))?;
    let ip = address.ip();
    let private = match ip {
        IpAddr::V4(v4) => v4.is_private(),
        IpAddr::V6(v6) => (v6.segments()[0] & 0xfe00) == 0xfc00,
    };
    let tls = match (&options.tls_cert, &options.tls_key) {
        (Some(_), Some(_)) => true,
        (None, None) => false,
        _ => return Err(Error::Tool("--tls-cert and --tls-key go together".into())),
    };
    if ip.is_unspecified() {
        return Err(Error::Tool("wildcard binds are refused".into()));
    }
    if !ip.is_loopback() {
        if !(private && options.allow_lan) {
            return Err(Error::Tool(
                "only loopback, or a private LAN address with --allow-lan, may be bound".into(),
            ));
        }
        if !tls {
            return Err(Error::Tool(
                "a LAN bind requires --tls-cert and --tls-key".into(),
            ));
        }
    }
    Ok((address, tls))
}

fn tls_acceptor(cert: &Path, key: &Path) -> Result<tokio_rustls::TlsAcceptor> {
    use rustls_pki_types::{CertificateDer, PrivateKeyDer, pem::PemObject};
    use tokio_rustls::rustls;
    let certs = CertificateDer::pem_file_iter(cert)
        .and_then(|iter| iter.collect::<std::result::Result<Vec<_>, _>>())
        .map_err(|_| Error::Tool("cannot read the TLS certificate".into()))?;
    let key = PrivateKeyDer::from_pem_file(key)
        .map_err(|_| Error::Tool("cannot read the TLS key".into()))?;
    let config = rustls::ServerConfig::builder_with_provider(Arc::new(
        rustls::crypto::ring::default_provider(),
    ))
    .with_safe_default_protocol_versions()
    .map_err(|err| Error::Tool(format!("TLS setup failed: {err}")))?
    .with_no_client_auth()
    .with_single_cert(certs, key)
    .map_err(|err| Error::Tool(format!("TLS certificate rejected: {err}")))?;
    Ok(tokio_rustls::TlsAcceptor::from(Arc::new(config)))
}

fn new_token() -> String {
    format!(
        "{}{}",
        uuid::Uuid::new_v4().simple(),
        uuid::Uuid::new_v4().simple()
    )
}

fn write_token(path: &Path, token: &str) -> Result<()> {
    use std::io::Write;
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let _ = std::fs::remove_file(path);
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        options.mode(0o600);
    }
    let mut file = options.open(path)?;
    file.write_all(token.as_bytes())?;
    file.sync_all()?;
    Ok(())
}

fn constant_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && a.iter().zip(b).fold(0_u8, |acc, (x, y)| acc | (x ^ y)) == 0
}

struct Gate {
    token: String,
    hosts: Vec<String>,
    origins: Vec<String>,
}

type Body = Full<Bytes>;

fn reply(status: StatusCode, value: Value) -> Response<Body> {
    Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::CACHE_CONTROL, "no-store")
        .header("X-Content-Type-Options", "nosniff")
        .body(Full::new(Bytes::from(value.to_string())))
        .unwrap_or_else(|_| Response::new(Full::new(Bytes::new())))
}

async fn handle(
    request: Request<hyper::body::Incoming>,
    gate: Arc<Gate>,
    hub: Arc<Hub>,
) -> Response<Body> {
    let header = |name: header::HeaderName| {
        request
            .headers()
            .get(name)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
    };
    let host = header(header::HOST).unwrap_or_default();
    if !gate
        .hosts
        .iter()
        .any(|allowed| allowed.eq_ignore_ascii_case(&host))
    {
        return reply(
            StatusCode::MISDIRECTED_REQUEST,
            json!({"error":"unexpected Host"}),
        );
    }
    if let Some(origin) = header(header::ORIGIN)
        && !gate
            .origins
            .iter()
            .any(|allowed| allowed.eq_ignore_ascii_case(&origin))
    {
        return reply(
            StatusCode::FORBIDDEN,
            json!({"error":"cross-origin request refused"}),
        );
    }
    let authorized = header(header::AUTHORIZATION)
        .and_then(|value| value.strip_prefix("Bearer ").map(str::to_owned))
        .is_some_and(|token| constant_eq(token.trim().as_bytes(), gate.token.as_bytes()));
    if !authorized {
        return reply(
            StatusCode::UNAUTHORIZED,
            json!({"error":"bearer token required"}),
        );
    }
    let method = request.method().clone();
    let path = request.uri().path().to_owned();
    match (method.as_str(), path.as_str()) {
        ("GET", "/v1/status") => reply(StatusCode::OK, hub.snapshot()),
        ("POST", "/v1/actions") => {
            let json_body = header(header::CONTENT_TYPE)
                .is_some_and(|kind| kind.to_ascii_lowercase().starts_with("application/json"));
            if !json_body {
                return reply(
                    StatusCode::UNSUPPORTED_MEDIA_TYPE,
                    json!({"error":"Content-Type must be application/json"}),
                );
            }
            let body = match Limited::new(request.into_body(), BODY_LIMIT)
                .collect()
                .await
            {
                Ok(body) => body.to_bytes(),
                Err(_) => {
                    return reply(
                        StatusCode::PAYLOAD_TOO_LARGE,
                        json!({"error":"body too large"}),
                    );
                }
            };
            let action: Action = match serde_json::from_slice(&body) {
                Ok(action) => action,
                Err(err) => {
                    return reply(StatusCode::BAD_REQUEST, json!({"error": err.to_string()}));
                }
            };
            match hub.submit(action) {
                Ok(result) => reply(StatusCode::OK, json!(result)),
                Err(error) => reply(StatusCode::CONFLICT, json!({"error": error})),
            }
        }
        ("GET", _) if path.starts_with("/v1/actions/") => {
            match hub.action(&path["/v1/actions/".len()..]) {
                Some(result) => reply(StatusCode::OK, json!(result)),
                None => reply(StatusCode::NOT_FOUND, json!({"error":"unknown action id"})),
            }
        }
        _ => reply(StatusCode::NOT_FOUND, json!({"error":"not found"})),
    }
}

impl Server {
    pub async fn start(options: Options, default_token: PathBuf, hub: Arc<Hub>) -> Result<Self> {
        let (address, tls) = validate_bind(&options)?;
        let acceptor = match (&options.tls_cert, &options.tls_key) {
            (Some(cert), Some(key)) => Some(tls_acceptor(cert, key)?),
            _ => None,
        };
        let listener = tokio::net::TcpListener::bind(address).await?;
        let bound = listener.local_addr()?;
        let scheme = if tls { "https" } else { "http" };
        let mut hosts = vec![bound.to_string()];
        if bound.ip().is_loopback() {
            hosts.push(format!("localhost:{}", bound.port()));
        }
        let origins = hosts
            .iter()
            .map(|host| format!("{scheme}://{host}"))
            .collect();
        let token = new_token();
        let token_file = options.token_file.unwrap_or(default_token);
        write_token(&token_file, &token)?;
        let gate = Arc::new(Gate {
            token,
            hosts,
            origins,
        });
        let shutdown = CancellationToken::new();
        let limit = Arc::new(tokio::sync::Semaphore::new(32));
        let task = {
            let shutdown = shutdown.clone();
            let hub = hub.clone();
            tokio::spawn(async move {
                loop {
                    let (stream, _) = tokio::select! {
                        _ = shutdown.cancelled() => break,
                        accepted = listener.accept() => match accepted {
                            Ok(accepted) => accepted,
                            Err(_) => continue,
                        },
                    };
                    let Ok(permit) = limit.clone().try_acquire_owned() else {
                        continue;
                    };
                    let gate = gate.clone();
                    let hub = hub.clone();
                    let acceptor = acceptor.clone();
                    let shutdown = shutdown.clone();
                    tokio::spawn(async move {
                        let _permit = permit;
                        let service = hyper::service::service_fn(move |request| {
                            let gate = gate.clone();
                            let hub = hub.clone();
                            async move {
                                Ok::<_, std::convert::Infallible>(handle(request, gate, hub).await)
                            }
                        });
                        let builder = {
                            let mut builder = hyper::server::conn::http1::Builder::new();
                            builder
                                .timer(hyper_util::rt::TokioTimer::new())
                                .header_read_timeout(Duration::from_secs(3))
                                .max_buf_size(64 * 1024);
                            builder
                        };
                        let serve = async {
                            match acceptor {
                                Some(acceptor) => {
                                    if let Ok(Ok(stream)) = tokio::time::timeout(
                                        Duration::from_secs(5),
                                        acceptor.accept(stream),
                                    )
                                    .await
                                    {
                                        let _ = builder
                                            .serve_connection(
                                                hyper_util::rt::TokioIo::new(stream),
                                                service,
                                            )
                                            .await;
                                    }
                                }
                                None => {
                                    let _ = builder
                                        .serve_connection(
                                            hyper_util::rt::TokioIo::new(stream),
                                            service,
                                        )
                                        .await;
                                }
                            }
                        };
                        tokio::select! {
                            _ = shutdown.cancelled() => {}
                            _ = tokio::time::timeout(Duration::from_secs(60), serve) => {}
                        }
                    });
                }
            })
        };
        hub.set_active(true);
        Ok(Self {
            url: format!("{scheme}://{bound}"),
            token_file,
            shutdown,
            task,
            hub,
        })
    }

    pub async fn stop(self) {
        self.hub.set_active(false);
        self.shutdown.cancel();
        let _ = tokio::time::timeout(Duration::from_secs(3), self.task).await;
        let _ = std::fs::remove_file(&self.token_file);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn action(id: &str, kind: ActionKind) -> Action {
        Action {
            id: id.into(),
            kind,
            text: None,
            approval_id: None,
        }
    }

    #[test]
    fn actions_are_idempotent_and_ids_cannot_be_reused() {
        let hub = Hub::default();
        let first = hub.submit(action("r1", ActionKind::Pause)).unwrap();
        assert_eq!(first.state, "applied");
        assert_eq!(
            hub.submit(action("r1", ActionKind::Pause)).unwrap().at,
            first.at
        );
        assert!(hub.submit(action("r1", ActionKind::Resume)).is_err());
        assert!(hub.submit(action("bad id", ActionKind::Pause)).is_err());
        assert_eq!(
            hub.submit(action("r2", ActionKind::Cancel)).unwrap().state,
            "rejected"
        );
        assert!(hub.action("r2").is_some());
    }

    #[test]
    fn cancel_and_steer_reach_the_running_task() {
        let hub = Hub::default();
        let cancel = CancellationToken::new();
        hub.task_started(&cancel);
        let mut steer = action("s", ActionKind::Steer);
        steer.text = Some("use the other API".into());
        assert_eq!(hub.submit(steer).unwrap().state, "applied");
        assert_eq!(hub.take_steer().as_deref(), Some("use the other API"));
        assert!(hub.take_steer().is_none());
        hub.submit(action("c", ActionKind::Cancel)).unwrap();
        assert!(cancel.is_cancelled());
        hub.task_ended();
        assert_eq!(hub.snapshot()["running"], json!(false));
    }

    #[tokio::test]
    async fn remote_approval_answers_a_headless_prompt_once() {
        let hub = Arc::new(Hub::default());
        hub.set_active(true);
        let authorizer = Arc::new(RemoteAuthorizer {
            inner: Arc::new(Permissions::new(
                crate::safety::permission::Mode::Ask,
                std::path::PathBuf::from("."),
                Vec::new(),
                None,
            )),
            hub: hub.clone(),
        });
        // A write needs approval in the default mode; a read-only `echo` would not.
        let pending = {
            let authorizer = authorizer.clone();
            tokio::spawn(async move {
                authorizer
                    .authorize(
                        Effect::Write(std::path::PathBuf::from("notes.txt")),
                        CancellationToken::new(),
                    )
                    .await
            })
        };
        let approval = loop {
            if let Some(id) = hub.snapshot()["pending"]["id"].as_str() {
                break id.to_owned();
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
        };
        let mut wrong = action("a0", ActionKind::Approve);
        wrong.approval_id = Some("other".into());
        assert_eq!(hub.submit(wrong).unwrap().state, "rejected");
        let mut approve = action("a1", ActionKind::Approve);
        approve.approval_id = Some(approval.clone());
        assert_eq!(hub.submit(approve).unwrap().state, "applied");
        assert!(pending.await.unwrap().is_ok());
        let mut again = action("a2", ActionKind::Approve);
        again.approval_id = Some(approval);
        assert_eq!(hub.submit(again).unwrap().state, "rejected");
    }

    #[tokio::test]
    async fn pause_holds_tools_until_resumed_or_cancelled() {
        let hub = Arc::new(Hub::default());
        hub.submit(action("p", ActionKind::Pause)).unwrap();
        let cancel = CancellationToken::new();
        let waiter = {
            let hub = hub.clone();
            let cancel = cancel.clone();
            tokio::spawn(async move { hub.wait_unpaused(&cancel).await })
        };
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!waiter.is_finished());
        hub.submit(action("r", ActionKind::Resume)).unwrap();
        assert!(waiter.await.unwrap().is_ok());
        hub.submit(action("p2", ActionKind::Pause)).unwrap();
        cancel.cancel();
        assert!(matches!(
            hub.wait_unpaused(&cancel).await,
            Err(Error::Cancelled)
        ));
    }

    #[test]
    fn binds_must_be_loopback_or_tls_lan() {
        let options = |bind: &str, lan: bool, tls: bool| Options {
            bind: bind.into(),
            token_file: None,
            allow_lan: lan,
            tls_cert: tls.then(|| "c".into()),
            tls_key: tls.then(|| "k".into()),
        };
        assert!(validate_bind(&options("127.0.0.1:0", false, false)).is_ok());
        assert!(validate_bind(&options("[::1]:0", false, false)).is_ok());
        assert!(validate_bind(&options("0.0.0.0:0", true, true)).is_err());
        assert!(validate_bind(&options("192.168.1.5:0", false, true)).is_err());
        assert!(validate_bind(&options("192.168.1.5:0", true, false)).is_err());
        assert!(validate_bind(&options("192.168.1.5:0", true, true)).is_ok());
        assert!(validate_bind(&options("8.8.8.8:0", true, true)).is_err());
        assert!(validate_bind(&options("localhost:0", false, false)).is_err());
    }

    #[tokio::test]
    async fn server_requires_token_host_and_origin() {
        let data = tempfile::tempdir().unwrap();
        let hub = Arc::new(Hub::default());
        let server = Server::start(
            Options::parse(&[]).unwrap(),
            data.path().join("token"),
            hub.clone(),
        )
        .await
        .unwrap();
        let token = std::fs::read_to_string(&server.token_file).unwrap();
        let client = reqwest::Client::builder().no_proxy().build().unwrap();
        let status = format!("{}/v1/status", server.url);
        assert_eq!(client.get(&status).send().await.unwrap().status(), 401);
        let ok = client
            .get(&status)
            .bearer_auth(&token)
            .send()
            .await
            .unwrap();
        assert_eq!(ok.status(), 200);
        let bad_origin = client
            .get(&status)
            .bearer_auth(&token)
            .header("Origin", "https://evil.example")
            .send()
            .await
            .unwrap();
        assert_eq!(bad_origin.status(), 403);
        let bad_host = client
            .get(&status)
            .bearer_auth(&token)
            .header("Host", "attacker.example")
            .send()
            .await
            .unwrap();
        assert_eq!(bad_host.status(), 421);
        let posted = client
            .post(format!("{}/v1/actions", server.url))
            .bearer_auth(&token)
            .json(&json!({"id":"x1","kind":"pause"}))
            .send()
            .await
            .unwrap();
        assert_eq!(posted.status(), 200);
        let stored = client
            .get(format!("{}/v1/actions/x1", server.url))
            .bearer_auth(&token)
            .send()
            .await
            .unwrap()
            .json::<Value>()
            .await
            .unwrap();
        assert_eq!(stored["state"], "applied");
        let token_file = server.token_file.clone();
        server.stop().await;
        assert!(!token_file.exists());
        assert!(!hub.active());
    }
}
