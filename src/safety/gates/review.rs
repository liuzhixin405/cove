//! Self-review: a read-only sub-agent reads the task's diff and reports real defects,
//! which go back to the model once. `done_self_review`: off (default), on, auto (at
//! least 40 changed lines).

use std::sync::{Arc, Mutex};

use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;

use super::Switch;
use crate::{
    domain::{Progress, Provider},
    engine::agent::{Agent, Ledger, Limits},
    engine::turn::TurnHook,
    session::Session,
    session::workspace::Workspace,
    tools::ToolExecutor,
};

pub const MIN_LINES: usize = 40;
pub const MAX_DIFF: usize = 40 * 1024;
pub const NO_ISSUES: &str = "NO_ISSUES";

pub struct ReviewSettings {
    pub max_turns: u32,
    pub max_output_tokens: u32,
    pub prices: Option<(f64, f64)>,
    pub context_tokens: usize,
    pub project_context: Option<String>,
}

pub struct SelfReview {
    pub mode: Switch,
    pub workspace: Arc<Workspace>,
    pub provider: Arc<dyn Provider>,
    /// Read-only tools for the reviewer.
    pub tools: Arc<dyn ToolExecutor>,
    pub settings: ReviewSettings,
    pub ledger: Arc<Ledger>,
    pub progress: Option<Arc<Progress>>,
    reviewed: Mutex<bool>,
}

impl SelfReview {
    pub fn new(
        mode: Switch,
        workspace: Arc<Workspace>,
        provider: Arc<dyn Provider>,
        tools: Arc<dyn ToolExecutor>,
        settings: ReviewSettings,
        ledger: Arc<Ledger>,
        progress: Option<Arc<Progress>>,
    ) -> Self {
        Self {
            mode,
            workspace,
            provider,
            tools,
            settings,
            ledger,
            progress,
            reviewed: Mutex::new(false),
        }
    }

    async fn say(&self, text: &str) {
        if let Some(progress) = &self.progress {
            progress.line(text.to_owned(), false).await;
        }
    }
}

/// Added plus removed lines of a unified diff.
pub fn changed_lines(diff: &str) -> usize {
    diff.lines()
        .filter(|line| {
            (line.starts_with('+') && !line.starts_with("+++"))
                || (line.starts_with('-') && !line.starts_with("---"))
        })
        .count()
}

pub fn clip(text: &str, limit: usize) -> String {
    if text.len() <= limit {
        return text.to_owned();
    }
    let mut end = limit;
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    format!(
        "{}\n... [diff truncated; read the files for the rest]",
        &text[..end]
    )
}

pub fn task(request: &str, diff: &str) -> String {
    let request: String = request.trim().chars().take(2000).collect();
    format!(
        "Review the change below, made for this user request:\n<request>\n{request}\n</request>\n\nLook only for real defects: wrong logic, unhandled errors or edge cases, callers or tests the change breaks, security problems, a part of the request left undone. Read the surrounding code with your tools when the diff is not enough. Do not comment on style or naming. If you find no real defect, reply with exactly {NO_ISSUES}. Otherwise list each defect as: file:line - the problem - why it is one.\n\n<diff>\n{}\n</diff>",
        clip(diff, MAX_DIFF)
    )
}

pub fn feedback(findings: &str) -> String {
    format!(
        "[self_review; this is a reviewer's report, not a new request] A reviewer read this task's diff and reported:\n{findings}\n\nFix the findings that are real defects and verify again. If a finding is wrong, do not change the code for it; say why in your final report."
    )
}

impl TurnHook for SelfReview {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut reviewed) = self.reviewed.lock() {
            *reviewed = false;
        }
        Box::pin(async { None })
    }

    fn finish_nudge<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async move {
            if self.mode == Switch::Off || self.reviewed.lock().map_or(true, |reviewed| *reviewed) {
                return None;
            }
            let files = self.workspace.changed_paths().ok()?;
            if files.is_empty() {
                return None;
            }
            let diff = self.workspace.diff().ok()?;
            let lines = changed_lines(&diff);
            if lines == 0 || (self.mode == Switch::Auto && lines < MIN_LINES) {
                return None;
            }
            if let Ok(mut reviewed) = self.reviewed.lock() {
                *reviewed = true;
            }
            let budget = self.ledger.remaining_budget();
            if budget.is_some_and(|budget| budget <= 0.0) {
                return None;
            }
            self.say(&format!(
                "[self-review] reviewing this task's changes ({} files, {lines} lines)…",
                files.len()
            ))
            .await;
            let agent = Agent::new(
                self.provider.clone(),
                self.tools.clone(),
                Limits {
                    max_turns: self.settings.max_turns,
                    max_output_tokens: self.settings.max_output_tokens,
                    budget,
                    prices: self.settings.prices,
                },
            )
            .and_then(|agent| agent.with_context_tokens(self.settings.context_tokens))
            .ok()?
            .with_project_context(self.settings.project_context.clone())
            .with_instructions(Some("You are a read-only code reviewer. You cannot edit files or run commands. Report only real defects, with file and line.".into()));
            let (result, spent, text) = agent
                .run_detached(
                    task(&session.current_request, &diff),
                    CancellationToken::new(),
                )
                .await;
            self.ledger.record(&spent);
            let text = text.trim();
            if result.is_err() {
                self.say("[self-review] did not finish; skipped").await;
                return None;
            }
            if text.is_empty() || text.trim_matches('`').starts_with(NO_ISSUES) {
                self.say("[self-review] passed").await;
                return None;
            }
            self.say("[self-review] found issues; handed back to the model")
                .await;
            Some(feedback(text))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn diff_helpers() {
        assert_eq!(changed_lines("--- a\n+++ b\n+x\n-y\n z\n"), 2);
        assert!(clip(&"界".repeat(100), 10).contains("truncated"));
        assert!(task("fix it", "+x").contains(NO_ISSUES));
    }
}
