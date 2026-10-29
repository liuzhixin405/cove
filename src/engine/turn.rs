//! Extension points of the agent loop. Features that steer a task (guardrails, nudges,
//! memories, directory hints) implement `TurnHook` instead of editing the kernel.

use futures_util::future::BoxFuture;
use serde_json::Value;

use crate::{session::Session, tools::ToolOutput};

/// Every method has a no-op default; implement only what a feature needs.
/// Hooks run in registration order; the first `Some` wins where noted.
pub trait TurnHook: Send + Sync {
    /// Extra context for a new task's first message (memories, an unfinished plan).
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async { None })
    }

    /// `Some(reason)` refuses the call before it runs; the model receives `reason`
    /// as a failed result (loop detection, plan mode). First `Some` wins.
    fn before_tool(&self, _name: &str, _input: &Value) -> Option<String> {
        None
    }

    /// Text appended to a finished tool result the model sees (guardrail warnings,
    /// directory instruction hints). All notes are appended.
    fn tool_note(&self, _name: &str, _input: &Value, _output: &ToolOutput) -> Option<String> {
        None
    }

    /// Called when the model ends without tool calls. `Some(message)` sends `message`
    /// back as a user notice and continues the task (empty reply, unfinished todo,
    /// done check). First `Some` wins. Hooks bound their own repeats.
    fn finish_nudge<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async { None })
    }

    /// After a task ends; `completed` is false for errors, limits and cancellation.
    fn task_end<'a>(&'a self, _session: &'a Session, _completed: bool) -> BoxFuture<'a, ()> {
        Box::pin(async {})
    }
}
