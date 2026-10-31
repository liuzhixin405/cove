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
    #[error("需要批准或已被拒绝：{0}")]
    Approval(String),
    /// The user said no to a call; the model hears it and goes on.
    #[error("用户拒绝了：{0}")]
    Denied(String),
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
}

impl Message {
    pub fn user(text: impl Into<String>) -> Self {
        Self {
            role: Role::User,
            blocks: vec![Block::Text(text.into())],
        }
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
}

pub struct ModelResponse {
    pub message: Message,
    pub usage: Usage,
    pub complete: bool,
    pub usage_reported: bool,
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

#[derive(Debug)]
pub enum Event {
    Notice(String),
    Text(String),
    Reasoning(String),
    ToolStarted {
        name: String,
        input: serde_json::Value,
    },
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
