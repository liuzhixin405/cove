//! MCP over HTTP: streamable HTTP (POST, JSON or SSE replies, `Mcp-Session-Id`) and the
//! legacy HTTP+SSE transport (GET event stream announcing a POST endpoint). Built on the
//! crate's reqwest so rmcp's own HTTP client (another reqwest major) is not pulled in.

use std::{sync::Arc, time::Duration};

use futures_util::StreamExt;
use rmcp::{
    RoleClient,
    model::{ClientJsonRpcMessage, ServerJsonRpcMessage},
    transport::Transport,
};
use tokio::sync::{Mutex, mpsc, watch};
use tokio_util::sync::CancellationToken;
use url::Url;

use crate::extensions::McpTransport;

/// The sign-in a server's requests carry: the OAuth store, the server URL and the
/// configured name (for the message that asks to sign in).
#[derive(Clone)]
pub struct Auth {
    pub oauth: Arc<super::oauth::OAuth>,
    pub server: String,
    pub name: String,
}

impl Auth {
    async fn header(&self, force: bool) -> Option<String> {
        self.oauth
            .bearer(&self.server, force)
            .await
            .ok()
            .flatten()
            .map(|token| format!("Bearer {token}"))
    }

    fn sign_in(&self) -> HttpError {
        err(format!(
            "the server requires sign-in; run /config mcp login {}",
            self.name
        ))
    }
}

const EVENT_LIMIT: usize = 4 * 1024 * 1024;
const SESSION_HEADER: &str = "mcp-session-id";

#[derive(Debug, thiserror::Error)]
#[error("{0}")]
pub struct HttpError(pub String);

fn err(text: impl std::fmt::Display) -> HttpError {
    HttpError(text.to_string())
}

/// Incremental `text/event-stream` parser: yields (event name, data) pairs.
#[derive(Default)]
struct SseParser {
    buffer: String,
    event: String,
    data: Vec<String>,
}

impl SseParser {
    fn feed(&mut self, chunk: &[u8]) -> Result<Vec<(String, String)>, HttpError> {
        self.buffer.push_str(&String::from_utf8_lossy(chunk));
        if self.buffer.len() > EVENT_LIMIT {
            return Err(err("MCP SSE event exceeds 4 MiB"));
        }
        let mut out = Vec::new();
        while let Some(end) = self.buffer.find('\n') {
            let line: String = self.buffer.drain(..=end).collect();
            let line = line.trim_end_matches(['\n', '\r']);
            if line.is_empty() {
                if !self.data.is_empty() {
                    let event = std::mem::take(&mut self.event);
                    out.push((
                        if event.is_empty() {
                            "message".into()
                        } else {
                            event
                        },
                        std::mem::take(&mut self.data).join("\n"),
                    ));
                }
                self.event.clear();
                continue;
            }
            if line.starts_with(':') {
                continue;
            }
            let (field, value) = line.split_once(':').unwrap_or((line, ""));
            let value = value.strip_prefix(' ').unwrap_or(value);
            match field {
                "event" => self.event = value.to_owned(),
                "data" => self.data.push(value.to_owned()),
                _ => {}
            }
        }
        Ok(out)
    }
}

fn parse_messages(text: &str) -> Vec<ServerJsonRpcMessage> {
    if let Ok(message) = serde_json::from_str::<ServerJsonRpcMessage>(text) {
        return vec![message];
    }
    serde_json::from_str::<Vec<ServerJsonRpcMessage>>(text).unwrap_or_default()
}

pub struct HttpTransport {
    kind: McpTransport,
    client: reqwest::Client,
    auth: Auth,
    url: Url,
    session: Arc<Mutex<Option<String>>>,
    endpoint: watch::Receiver<Option<Url>>,
    sender: mpsc::Sender<ServerJsonRpcMessage>,
    receiver: mpsc::Receiver<ServerJsonRpcMessage>,
    stop: CancellationToken,
}

async fn pump(
    response: reqwest::Response,
    sender: mpsc::Sender<ServerJsonRpcMessage>,
    endpoint: Option<(Url, watch::Sender<Option<Url>>)>,
    stop: CancellationToken,
) -> Result<(), HttpError> {
    let mut stream = response.bytes_stream();
    let mut parser = SseParser::default();
    loop {
        let chunk = tokio::select! {
            _ = stop.cancelled() => return Ok(()),
            chunk = stream.next() => chunk,
        };
        let Some(chunk) = chunk else {
            return Ok(());
        };
        let chunk = chunk.map_err(err)?;
        for (event, data) in parser.feed(&chunk)? {
            if event == "endpoint" {
                if let Some((base, announce)) = &endpoint {
                    let url = base.join(data.trim()).map_err(err)?;
                    if url.origin() != base.origin() {
                        return Err(err("MCP SSE endpoint on another origin refused"));
                    }
                    let _ = announce.send(Some(url));
                }
                continue;
            }
            for message in parse_messages(&data) {
                if sender.send(message).await.is_err() {
                    return Ok(());
                }
            }
        }
    }
}

impl HttpTransport {
    pub async fn connect(
        kind: McpTransport,
        raw: &str,
        timeout: Duration,
        auth: Auth,
    ) -> Result<Self, HttpError> {
        let url = Url::parse(raw).map_err(err)?;
        if !matches!(url.scheme(), "http" | "https") {
            return Err(err("MCP URL must be http(s)"));
        }
        let client = crate::model::net::apply(reqwest::Client::builder())
            .connect_timeout(Duration::from_secs(10))
            .redirect(reqwest::redirect::Policy::limited(3))
            .user_agent(concat!("cove/", env!("CARGO_PKG_VERSION")))
            .build()
            .map_err(err)?;
        let (sender, receiver) = mpsc::channel(64);
        let (announce, endpoint) = watch::channel(None);
        let stop = CancellationToken::new();
        if kind == McpTransport::Sse {
            let mut request = client
                .get(url.clone())
                .header(reqwest::header::ACCEPT, "text/event-stream");
            if let Some(header) = auth.header(false).await {
                request = request.header(reqwest::header::AUTHORIZATION, header);
            }
            let response = tokio::time::timeout(timeout, request.send())
                .await
                .map_err(|_| err("MCP SSE connect timed out"))?
                .map_err(err)?;
            if response.status() == reqwest::StatusCode::UNAUTHORIZED {
                return Err(auth.sign_in());
            }
            if !response.status().is_success() {
                return Err(err(format!("MCP SSE GET: HTTP {}", response.status())));
            }
            let (tx, base, stop) = (sender.clone(), url.clone(), stop.clone());
            tokio::spawn(async move {
                if let Err(error) = pump(response, tx, Some((base, announce)), stop).await {
                    let _ =
                        crate::app::ui::print_text(&format!("[MCP] SSE 流已结束：{error}\n"), true);
                }
            });
        } else {
            let _ = announce.send(Some(url.clone()));
        }
        Ok(Self {
            kind,
            client,
            auth,
            url,
            session: Arc::new(Mutex::new(None)),
            endpoint,
            sender,
            receiver,
            stop,
        })
    }
}

impl Transport<RoleClient> for HttpTransport {
    type Error = HttpError;

    fn send(
        &mut self,
        item: ClientJsonRpcMessage,
    ) -> impl Future<Output = Result<(), Self::Error>> + Send + 'static {
        let (client, session, sender, stop, kind, auth) = (
            self.client.clone(),
            self.session.clone(),
            self.sender.clone(),
            self.stop.clone(),
            self.kind,
            self.auth.clone(),
        );
        let mut endpoint = self.endpoint.clone();
        async move {
            let url = tokio::time::timeout(Duration::from_secs(15), async {
                loop {
                    if let Some(url) = endpoint.borrow().clone() {
                        return Ok(url);
                    }
                    endpoint
                        .changed()
                        .await
                        .map_err(|_| err("MCP SSE stream closed"))?;
                }
            })
            .await
            .map_err(|_| err("MCP SSE endpoint not announced"))??;
            let body = serde_json::to_vec(&item).map_err(err)?;
            let session_id = session.lock().await.clone();
            let post = |authorization: Option<String>| {
                let mut request = client
                    .post(url.clone())
                    .header(reqwest::header::CONTENT_TYPE, "application/json")
                    .header(
                        reqwest::header::ACCEPT,
                        "application/json, text/event-stream",
                    )
                    .body(body.clone());
                if let Some(id) = &session_id {
                    request = request.header(SESSION_HEADER, id);
                }
                if let Some(header) = authorization {
                    request = request.header(reqwest::header::AUTHORIZATION, header);
                }
                request.send()
            };
            let mut response = post(auth.header(false).await).await.map_err(err)?;
            if response.status() == reqwest::StatusCode::UNAUTHORIZED {
                // An expired or revoked token: refresh once, else ask for a sign-in.
                match auth.header(true).await {
                    Some(header) => response = post(Some(header)).await.map_err(err)?,
                    None => return Err(auth.sign_in()),
                }
                if response.status() == reqwest::StatusCode::UNAUTHORIZED {
                    return Err(auth.sign_in());
                }
            }
            let status = response.status();
            if !status.is_success() {
                let text = response.text().await.unwrap_or_default();
                return Err(err(format!(
                    "MCP POST: HTTP {status}: {}",
                    crate::extensions::skills::clip(&text, 500)
                )));
            }
            if kind == McpTransport::Sse {
                return Ok(()); // replies arrive on the event stream
            }
            if let Some(id) = response
                .headers()
                .get(SESSION_HEADER)
                .and_then(|v| v.to_str().ok())
            {
                *session.lock().await = Some(id.to_owned());
            }
            let kind = response
                .headers()
                .get(reqwest::header::CONTENT_TYPE)
                .and_then(|v| v.to_str().ok())
                .unwrap_or("")
                .to_ascii_lowercase();
            if kind.starts_with("text/event-stream") {
                return pump(response, sender, None, stop).await;
            }
            let bytes = response.bytes().await.map_err(err)?;
            if bytes.len() > EVENT_LIMIT {
                return Err(err("MCP response exceeds 4 MiB"));
            }
            if !bytes.is_empty() {
                for message in parse_messages(&String::from_utf8_lossy(&bytes)) {
                    let _ = sender.send(message).await;
                }
            }
            Ok(())
        }
    }

    fn receive(&mut self) -> impl Future<Output = Option<ServerJsonRpcMessage>> + Send {
        self.receiver.recv()
    }

    async fn close(&mut self) -> Result<(), Self::Error> {
        self.stop.cancel();
        if self.kind == McpTransport::Http
            && let Some(id) = self.session.lock().await.take()
        {
            let _ = tokio::time::timeout(
                Duration::from_secs(3),
                self.client
                    .delete(self.url.clone())
                    .header(SESSION_HEADER, id)
                    .send(),
            )
            .await;
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sse_parser_handles_split_chunks_and_events() {
        let mut parser = SseParser::default();
        assert!(
            parser
                .feed(b"event: endpoint\r\ndata: /messages?s=1")
                .unwrap()
                .is_empty()
        );
        let events = parser
            .feed(b"\r\n\r\n: comment\ndata: {\"a\":\ndata: 1}\n\n")
            .unwrap();
        assert_eq!(events[0], ("endpoint".into(), "/messages?s=1".into()));
        assert_eq!(events[1], ("message".into(), "{\"a\":\n1}".into()));
    }
}
