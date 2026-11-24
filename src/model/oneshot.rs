//! One-off model calls: a tool-free request, a bounded wait, the text of the reply.
//! Memory extraction, session titles, web page answers, `/init`, the setup probe,
//! compaction and the like all go through here instead of each re-implementing it.

use std::{collections::HashSet, future::Future, sync::Mutex, time::Duration};

use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::domain::{Block, Error, Message, ModelRequest, ModelResponse, Provider, Result};

/// A tool-free request with one user message.
pub fn text_request(
    system: impl Into<String>,
    user: impl Into<String>,
    max_output_tokens: u32,
) -> ModelRequest {
    ModelRequest {
        system: system.into(),
        messages: vec![Message::user(user)],
        tools: Vec::new(),
        max_output_tokens,
        estimated_tokens: None,
        model: None,
        cache: crate::domain::Cache::Short,
    }
}

/// The text blocks of a reply, joined.
pub fn response_text(message: &Message) -> String {
    message
        .blocks
        .iter()
        .filter_map(|block| match block {
            Block::Text(text) => Some(text.as_str()),
            _ => None,
        })
        .collect()
}

/// Why a reply cannot be used as text.
#[derive(Debug, PartialEq, Eq)]
pub enum Incomplete {
    /// Cut off (the output limit, a dropped stream).
    Truncated,
    /// It asked for a tool, which a tool-free call cannot run.
    ToolUse,
    Empty,
}

impl std::fmt::Display for Incomplete {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::Truncated => "模型回答被截断",
            Self::ToolUse => "模型回答里有工具调用",
            Self::Empty => "模型回答为空",
        })
    }
}

/// The reply's text when it is complete, has no tool call and is not blank.
pub fn require_text(response: &ModelResponse) -> std::result::Result<String, Incomplete> {
    if !response.complete {
        return Err(Incomplete::Truncated);
    }
    if response
        .message
        .blocks
        .iter()
        .any(|block| matches!(block, Block::ToolUse { .. }))
    {
        return Err(Incomplete::ToolUse);
    }
    let text = response_text(&response.message);
    if text.trim().is_empty() {
        return Err(Incomplete::Empty);
    }
    Ok(text)
}

/// Sends `request` and waits at most `timeout`; streamed events are discarded. On timeout
/// or when `cancel` fires, the request is cancelled.
pub async fn call(
    provider: &dyn Provider,
    request: ModelRequest,
    timeout: Duration,
    cancel: CancellationToken,
) -> Result<ModelResponse> {
    let (events, mut receiver) = mpsc::channel(64);
    let drain = async move { while receiver.recv().await.is_some() {} };
    let stream = provider.stream(request, events, cancel.clone());
    let outcome = tokio::select! {
        (response, ()) = async { tokio::join!(stream, drain) } => response,
        () = tokio::time::sleep(timeout) => {
            cancel.cancel();
            Err(Error::Provider(format!("模型调用超时（{} 秒）", timeout.as_secs())))
        }
        () = cancel.cancelled() => Err(Error::Cancelled),
    };
    outcome
}

/// Background one-off work started from hooks: each key runs once per process, and
/// shutdown waits for what is still running.
#[derive(Default)]
pub struct Background {
    claimed: Mutex<HashSet<String>>,
    tasks: Mutex<Vec<tokio::task::JoinHandle<()>>>,
}

impl Background {
    /// True the first time `key` is claimed in this process.
    pub fn claim(&self, key: impl Into<String>) -> bool {
        self.claimed
            .lock()
            .is_ok_and(|mut claimed| claimed.insert(key.into()))
    }

    /// Runs `work` in the background.
    pub fn spawn(&self, work: impl Future<Output = ()> + Send + 'static) {
        let handle = tokio::spawn(work);
        if let Ok(mut tasks) = self.tasks.lock() {
            tasks.retain(|task| !task.is_finished());
            tasks.push(handle);
        }
    }

    /// Lets running work finish, waiting at most `limit`.
    pub async fn wait(&self, limit: Duration) {
        let tasks: Vec<_> = self
            .tasks
            .lock()
            .map(|mut tasks| tasks.drain(..).collect())
            .unwrap_or_default();
        let _ = tokio::time::timeout(limit, futures_util::future::join_all(tasks)).await;
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use futures_util::future::BoxFuture;

    fn response(blocks: Vec<Block>, complete: bool) -> ModelResponse {
        ModelResponse {
            message: Message {
                role: crate::domain::Role::Assistant,
                blocks,
                origin: crate::domain::Origin::Unknown,
            },
            usage: Default::default(),
            complete,
            usage_reported: false,
            reported_cost: None,
            model: None,
        }
    }

    #[test]
    fn only_complete_tool_free_non_blank_replies_are_text() {
        let text = |t: &str| Block::Text(t.into());
        assert_eq!(
            require_text(&response(vec![text("a"), text("b")], true)),
            Ok("ab".into())
        );
        assert_eq!(
            require_text(&response(vec![text("a")], false)),
            Err(Incomplete::Truncated)
        );
        assert_eq!(
            require_text(&response(vec![text("  ")], true)),
            Err(Incomplete::Empty)
        );
        let tool = Block::ToolUse {
            id: "1".into(),
            name: "read".into(),
            input: serde_json::json!({}),
            extra: None,
        };
        assert_eq!(
            require_text(&response(vec![text("a"), tool], true)),
            Err(Incomplete::ToolUse)
        );
    }

    struct Slow;

    impl Provider for Slow {
        fn stream(
            &self,
            _request: ModelRequest,
            events: mpsc::Sender<crate::domain::Event>,
            cancel: CancellationToken,
        ) -> BoxFuture<'_, Result<ModelResponse>> {
            Box::pin(async move {
                let _ = events.send(crate::domain::Event::Notice("x".into())).await;
                cancel.cancelled().await;
                Err(Error::Cancelled)
            })
        }
    }

    #[tokio::test]
    async fn a_call_times_out_and_cancels_the_request() {
        let cancel = CancellationToken::new();
        let outcome = call(
            &Slow,
            text_request("s", "u", 10),
            Duration::from_millis(50),
            cancel.clone(),
        )
        .await;
        let Err(error) = outcome else {
            panic!("a slow call must time out");
        };
        assert!(error.to_string().contains("超时"), "{error}");
        assert!(cancel.is_cancelled());
    }

    #[tokio::test]
    async fn background_work_runs_once_per_key_and_is_waited_for() {
        let background = Background::default();
        assert!(background.claim("a"));
        assert!(!background.claim("a"));
        let done = std::sync::Arc::new(std::sync::atomic::AtomicBool::new(false));
        let flag = done.clone();
        background.spawn(async move {
            tokio::time::sleep(Duration::from_millis(20)).await;
            flag.store(true, std::sync::atomic::Ordering::SeqCst);
        });
        background.wait(Duration::from_secs(5)).await;
        assert!(done.load(std::sync::atomic::Ordering::SeqCst));
        // A limit bounds the wait for work that does not finish.
        background.spawn(std::future::pending());
        let started = std::time::Instant::now();
        background.wait(Duration::from_millis(50)).await;
        assert!(started.elapsed() < Duration::from_secs(2));
    }
}
