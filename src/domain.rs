use futures_util::future::BoxFuture;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

pub type Result<T> = std::result::Result<T, Error>;

#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("输入已关闭")]
    InputClosed,
    #[error("已取消；已完成的文件或命令改动不会回滚（可用 /undo 撤销文件改动）")]
    Cancelled,
    #[error("已达上限：{0}")]
    Limit(String),
    /// cove ended the task itself (call quota used up, no progress) after a closing
    /// summary; the report says what was done.
    #[error("{}", .0.stop.message())]
    Stopped(Box<crate::engine::agent::Stopped>),
    #[error("需要批准或已被拒绝：{0}")]
    Approval(String),
    /// The user said no to a call; the model hears it and goes on.
    #[error("用户拒绝了：{0}")]
    Denied(String),
    /// A call needed approval with nobody to ask (`-p`, a pipe, a background agent):
    /// it was not run; the model hears it and goes on.
    #[error("需要批准，但没有终端可以询问，未执行：{0}")]
    Unattended(String),
    #[error("模型服务：{0}")]
    Provider(String),
    #[error("tool: {0}")]
    Tool(String),
    #[error("事件接收端已断开")]
    ConsumerClosed,
    #[error("{0}")]
    Session(String),
    #[error("工作区：{0}")]
    Workspace(String),
    #[error("副作用不确定：{0}")]
    Uncertain(String),
    #[error(transparent)]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub enum Block {
    Text(String),
    Reasoning(String),
    Thinking(Value),
    ToolUse {
        id: String,
        name: String,
        input: Value,
        extra: Option<Value>,
    },
    ToolResult {
        id: String,
        content: String,
        is_error: bool,
    },
    /// Base64 image data; providers encode it natively or downgrade it to a note.
    Image {
        media_type: String,
        data_base64: String,
    },
}

/// An image prepared for a model request (already resized, base64 encoded).
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct ImagePart {
    pub media_type: String,
    pub data_base64: String,
}

impl From<ImagePart> for Block {
    fn from(part: ImagePart) -> Self {
        Block::Image {
            media_type: part.media_type,
            data_base64: part.data_base64,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub enum Role {
    User,
    Assistant,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Message {
    pub role: Role,
    pub blocks: Vec<Block>,
    /// Who wrote a user-role message, recorded when it is written so history views
    /// need not guess from the text. Never sent to the model.
    #[serde(default, skip_serializing_if = "Origin::is_unknown")]
    pub origin: Origin,
}

/// Where a user-role message came from. Sessions written before this was recorded
/// load as `Unknown`, which history views classify by the text instead.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Origin {
    #[default]
    Unknown,
    /// A request the user typed (its first block); later blocks may be injected context.
    Human,
    /// Text the user typed while a task ran, delivered mid-task.
    Steer,
    /// The summary that replaced earlier messages at compaction.
    Summary,
    /// A notice the program added (verification failed, operator actions, limits).
    Notice,
}

impl Origin {
    pub fn is_unknown(&self) -> bool {
        *self == Self::Unknown
    }
}

impl Message {
    /// A user-role text message of unknown origin (one-off requests, tests).
    pub fn user(text: impl Into<String>) -> Self {
        Self {
            role: Role::User,
            blocks: vec![Block::Text(text.into())],
            origin: Origin::Unknown,
        }
    }

    pub fn human(text: impl Into<String>) -> Self {
        Self::user(text).with_origin(Origin::Human)
    }

    pub fn steer(text: impl Into<String>) -> Self {
        Self::user(text).with_origin(Origin::Steer)
    }

    pub fn notice(text: impl Into<String>) -> Self {
        Self::user(text).with_origin(Origin::Notice)
    }

    pub fn with_origin(mut self, origin: Origin) -> Self {
        self.origin = origin;
        self
    }
}

#[derive(Clone, Debug)]
pub struct ToolDefinition {
    pub name: String,
    pub description: String,
    pub schema: Value,
}

#[derive(Clone)]
pub struct ModelRequest {
    pub system: String,
    pub messages: Vec<Message>,
    pub tools: Vec<ToolDefinition>,
    pub max_output_tokens: u32,
    /// Reuses an estimate already computed by the agent loop for limit checks.
    pub estimated_tokens: Option<usize>,
    /// Model for this request; `None` lets the provider (or router) choose.
    pub model: Option<String>,
    /// The prompt-cache marks the request carries (providers that cache on their own,
    /// such as OpenAI-compatible ones, ignore them).
    pub cache: Cache,
}

/// Prompt-cache marks of a request.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Cache {
    /// No marks: a one-off prompt nothing reads again (a standalone compaction), so
    /// writing it to the cache would only cost more than plain input.
    Off,
    /// The provider's default lifetime (five minutes).
    #[default]
    Short,
    /// An hour where the model supports it: a conversation turn the user may pause after.
    Hour,
}

impl Cache {
    /// A conversation turn's marks: an hour when `long`, else the default.
    pub fn turn(long: bool) -> Self {
        if long { Self::Hour } else { Self::Short }
    }
}

/// `input` counts every prompt token; `cache_read` and `cache_write` are the parts
/// of it served from or written to the provider's prompt cache.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Usage {
    pub input: u64,
    pub output: u64,
    #[serde(default)]
    pub cache_read: u64,
    #[serde(default)]
    pub cache_write: u64,
    /// The part of `cache_write` kept an hour (billed higher).
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cache_write_1h: u64,
}

fn is_zero(value: &u64) -> bool {
    *value == 0
}

pub struct ModelResponse {
    pub message: Message,
    pub usage: Usage,
    pub complete: bool,
    pub usage_reported: bool,
    pub reported_cost: Option<f64>,
    /// Model that actually served the request, when the provider knows it.
    pub model: Option<String>,
}

pub trait Provider: Send + Sync {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>>;
}

pub trait UserInput: Send + Sync {
    fn answer(&self, prompt: String, cancel: CancellationToken) -> BoxFuture<'_, Result<String>>;
}

/// Guidance typed while a task runs. The agent waits on it during a model call; when
/// it fires, the in-flight call is aborted and the text is answered at once, instead
/// of waiting for the next call boundary.
pub trait SteerSource: Send + Sync {
    /// Resolves when steering text may be waiting; must not fire spuriously in a loop.
    fn wake(&self) -> BoxFuture<'_, ()>;
    /// Drains the waiting text, already prefixed for the model; `None` on a lost race.
    fn take(&self) -> Option<String>;
}

#[derive(Debug)]
pub enum Event {
    Notice(String),
    Text(String),
    Reasoning(String),
    ToolStarted {
        name: String,
        input: serde_json::Value,
    },
    /// What the task does now, for the status row, while neither a reply nor a tool
    /// call shows it (the checks after a reply).
    Activity(String),
    /// One line of live command output; the final result still arrives in `ToolFinished`.
    Output {
        line: String,
        stderr: bool,
    },
    ToolFinished {
        name: String,
        failed: bool,
        output: String,
    },
    Usage {
        tokens: Usage,
        cost: Option<f64>,
    },
}

/// Forwards live tool output to the running task's event stream, when one is attached.
#[derive(Default)]
pub struct Progress {
    sender: std::sync::Mutex<Option<mpsc::Sender<Event>>>,
}

impl Progress {
    /// Attach for the duration of a task; detach before awaiting the consumer, or it never ends.
    pub fn attach(&self, sender: Option<mpsc::Sender<Event>>) {
        if let Ok(mut current) = self.sender.lock() {
            *current = sender;
        }
    }

    pub async fn line(&self, line: String, stderr: bool) {
        let sender = self.sender.lock().ok().and_then(|current| current.clone());
        if let Some(sender) = sender {
            // A closed display must not fail the command itself.
            let _ = sender.send(Event::Output { line, stderr }).await;
        }
    }

    /// The status-row text (shown even when live tool output is off).
    pub async fn activity(&self, text: String) {
        let sender = self.sender.lock().ok().and_then(|current| current.clone());
        if let Some(sender) = sender {
            let _ = sender.send(Event::Activity(text)).await;
        }
    }

    /// A notice for the user (shown even when live tool output is off).
    pub async fn notice(&self, text: String) {
        let sender = self.sender.lock().ok().and_then(|current| current.clone());
        if let Some(sender) = sender {
            let _ = sender.send(Event::Notice(text)).await;
        }
    }
}

pub async fn emit(
    events: &mpsc::Sender<Event>,
    cancel: &CancellationToken,
    event: Event,
) -> Result<()> {
    tokio::select! {
        biased;
        _ = cancel.cancelled() => Err(Error::Cancelled),
        result = events.send(event) => result.map_err(|_| Error::ConsumerClosed),
    }
}
