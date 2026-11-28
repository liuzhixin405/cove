//! Extension points of the agent loop. Features that steer a task (guardrails, nudges,
//! memories, directory hints) implement `TurnHook` instead of editing the kernel.

use futures_util::future::BoxFuture;
use serde_json::Value;

use crate::{session::Session, tools::ToolOutput};

/// One run of a task (a new task or a `/continue`), as the agent loop counted it.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct TaskStats {
    /// Model calls of this run, compaction and sub-agents included.
    pub model_calls: u32,
    /// The longest run of rounds without progress.
    pub idle_max: u32,
    /// cove stopped the run for lack of progress.
    pub stalled: bool,
    /// The run sent the project context the session recorded in an earlier process,
    /// not this process's different one.
    pub system_reused: bool,
}

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

    /// A shell command that went on in the background has ended;
    /// `output` reads like the result of a `shell` call that ran it to the end.
    fn background_finished(&self, _command: &str, _output: &ToolOutput) {}

    /// Called when the model ends without tool calls. `Some(message)` sends `message`
    /// back as a user notice and continues the task (empty reply, unfinished todo,
    /// done check). First `Some` wins. Hooks bound their own repeats.
    fn finish_nudge<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async { None })
    }

    /// Just before `task_end`: what this run of the task did (the trace records it).
    fn task_stats(&self, _stats: &TaskStats) {}

    /// After a task ends; `completed` is false for errors, limits and cancellation.
    fn task_end<'a>(&'a self, _session: &'a Session, _completed: bool) -> BoxFuture<'a, ()> {
        Box::pin(async {})
    }

    fn task_end_with_verification<'a>(
        &'a self,
        session: &'a Session,
        completed: bool,
        _verification: Option<crate::safety::verify::Outcome>,
    ) -> BoxFuture<'a, ()> {
        self.task_end(session, completed)
    }
}
