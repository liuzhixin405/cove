//! AI review of a diff by a read-only sub-agent, judged on real defects and on the
//! project's instruction files (AGENTS.md, CLAUDE.md, .cove.md) and rules
//! (`.cove/rules.md`, read again for every review). Two users:
//! - self-review at the end of a task (`done_self_review`: off by default); findings
//!   go back to the model once;
//! - review of the staged change before a commit (`commit_review`: on by default), in
//!   the pre-commit gate and in `/commit`.
//!
//! `auto` for either means at least 40 changed lines.

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
const MAX_REQUEST: usize = 2000;

/// What counts as a finding, shared by both reviews.
const CRITERIA: &str = "Look for real defects: wrong logic, unhandled errors or edge cases, callers or tests the change breaks, security problems. \
The project's instruction files (\"Project instructions\" in your system prompt) are this repository's rules: a change that breaks one of them is a finding even when it looks like style; quote the rule it breaks. \
Do not comment on style or naming that no project rule covers. Read the surrounding code with your tools when the diff is not enough.";

pub struct ReviewSettings {
    pub max_turns: u32,
    pub max_output_tokens: u32,
    pub prices: Option<(f64, f64)>,
    pub context_tokens: usize,
    pub project_context: Option<String>,
}

/// The read-only sub-agent that reviews a diff.
pub struct Reviewer {
    pub provider: Arc<dyn Provider>,
    /// Read-only tools for the reviewer.
    pub tools: Arc<dyn ToolExecutor>,
    pub settings: ReviewSettings,
    pub ledger: Arc<Ledger>,
    /// The project's rules, read when a review starts: rules added during the session
    /// are not in the system prompt yet.
    pub rules: Option<Arc<crate::memory::rules::ProjectRules>>,
}

/// `task` with the project's rules as they are now.
fn with_rules(task: String, rules: Option<String>) -> String {
    match rules {
        Some(rules) => format!(
            "{task}\n\nThe project's rules file ({}) as it is now; these are project rules too:\n<project_rules>\n{rules}</project_rules>",
            crate::memory::rules::FILE
        ),
        None => task,
    }
}

#[derive(Clone, Debug, PartialEq)]
pub enum Verdict {
    Passed,
    /// The reviewer's findings.
    Issues(String),
    /// The review did not finish (model error, limits); nothing is blocked on it.
    Skipped,
}

/// Whether a reviewer's reply says it found nothing; a code fence around the
/// verdict is common and must not turn it into a finding.
fn no_issues(text: &str) -> bool {
    let text = text.trim_matches(|c: char| c.is_whitespace() || c == '`');
    text.is_empty() || text.starts_with(NO_ISSUES)
}

impl Reviewer {
    pub async fn review(&self, task: String, cancel: CancellationToken) -> Verdict {
        let agent = Agent::new(
            self.provider.clone(),
            self.tools.clone(),
            Limits {
                max_turns: self.settings.max_turns,
                max_output_tokens: self.settings.max_output_tokens,
                prices: self.settings.prices,
            },
        )
        .and_then(|agent| agent.with_context_tokens(self.settings.context_tokens));
        let Ok(agent) = agent else {
            return Verdict::Skipped;
        };
        let agent = agent
            .with_project_context(self.settings.project_context.clone())
            .with_instructions(Some("You are a read-only code reviewer. You cannot edit files or run commands. Report only real findings, with file and line.".into()))
            .with_long_cache(false);
        let task = with_rules(task, self.rules.as_ref().and_then(|rules| rules.current()));
        let (result, spent, text) = agent.run_detached(task, cancel).await;
        self.ledger.record(&spent);
        if result.is_err() {
            return Verdict::Skipped;
        }
        let text = text.trim_matches(|c: char| c.is_whitespace() || c == '`');
        if no_issues(text) {
            Verdict::Passed
        } else {
            Verdict::Issues(text.to_owned())
        }
    }
}

/// The review before a commit, shared by the pre-commit gate and `/commit`.
pub struct CommitReview {
    /// `On` or `Auto`; an `Off` review is not built.
    pub mode: Switch,
    pub reviewer: Reviewer,
}

impl CommitReview {
    /// Whether this staged diff is big enough to review under the mode.
    pub fn wanted(&self, diff: &str) -> bool {
        wanted(self.mode, changed_lines(diff))
    }

    pub async fn review(&self, request: &str, diff: &str, cancel: CancellationToken) -> Verdict {
        self.reviewer
            .review(commit_task(request, diff), cancel)
            .await
    }
}

/// Whether a diff of `lines` changed lines is reviewed under `mode`.
pub fn wanted(mode: Switch, lines: usize) -> bool {
    lines > 0
        && match mode {
            Switch::Off => false,
            Switch::On => true,
            Switch::Auto => lines >= MIN_LINES,
        }
}

/// Added plus removed lines of a unified diff.
pub fn changed_lines(diff: &str) -> usize {
    let mut in_hunk = false;
    let mut count = 0;
    for line in diff.lines() {
        if line.starts_with("diff ") {
            in_hunk = false;
        } else if line.starts_with("@@") {
            in_hunk = true;
        } else if in_hunk && (line.starts_with('+') || line.starts_with('-')) {
            // Inside a hunk every +- line is content, including one that itself
            // starts with "--" or "++".
            count += 1;
        }
    }
    count
}

fn clipped_diff(diff: &str) -> String {
    crate::text::clip(
        diff,
        MAX_DIFF,
        "\n... [diff truncated; read the files for the rest]",
    )
}

/// Self-review of a task's diff.
pub fn task(request: &str, diff: &str) -> String {
    let request = crate::text::clip_chars(request.trim(), MAX_REQUEST);
    format!(
        "Review the change below, made for this user request:\n<request>\n{request}\n</request>\n\n{CRITERIA} Also report a part of the request left undone. If you find nothing, reply with exactly {NO_ISSUES}. Otherwise list each finding as: file:line - the problem - why it is one.\n\n<diff>\n{}\n</diff>",
        clipped_diff(diff)
    )
}

/// Review of the staged change before it is committed; `request` is the session's
/// current request, empty when there is none.
pub fn commit_task(request: &str, diff: &str) -> String {
    let request = crate::text::clip_chars(request.trim(), MAX_REQUEST);
    let context = if request.is_empty() {
        "Write the findings in the language of the project instructions, or English when there are none.".to_owned()
    } else {
        format!(
            "It was made for this request; write the findings in its language:\n<request>\n{request}\n</request>"
        )
    };
    format!(
        "Review the staged change below before it is committed. {context}\n\n{CRITERIA} If you find nothing, reply with exactly {NO_ISSUES}. Otherwise list each finding as: file:line - the problem - why it is one.\n\n<diff>\n{}\n</diff>",
        clipped_diff(diff)
    )
}

pub fn feedback(findings: &str) -> String {
    format!(
        "[self_review; this is a reviewer's report, not a new request] A reviewer read this task's diff and reported:\n{findings}\n\nFix the findings that are real defects and verify again. If a finding is wrong, do not change the code for it; say why in your final report."
    )
}

pub struct SelfReview {
    pub mode: Switch,
    pub workspace: Arc<Workspace>,
    pub reviewer: Reviewer,
    pub progress: Option<Arc<Progress>>,
    reviewed: Mutex<bool>,
}

impl SelfReview {
    pub fn new(
        mode: Switch,
        workspace: Arc<Workspace>,
        reviewer: Reviewer,
        progress: Option<Arc<Progress>>,
    ) -> Self {
        Self {
            mode,
            workspace,
            reviewer,
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
            if !wanted(self.mode, lines) {
                return None;
            }
            if let Ok(mut reviewed) = self.reviewed.lock() {
                *reviewed = true;
            }
            self.say(&format!(
                "[self-review] reviewing this task's changes ({} files, {lines} lines)…",
                files.len()
            ))
            .await;
            match self
                .reviewer
                .review(
                    task(&session.current_request, &diff),
                    CancellationToken::new(),
                )
                .await
            {
                Verdict::Passed => {
                    self.say("[self-review] passed").await;
                    None
                }
                Verdict::Skipped => {
                    self.say("[self-review] did not finish; skipped").await;
                    None
                }
                Verdict::Issues(findings) => {
                    self.say("[self-review] found issues; handed back to the model")
                        .await;
                    Some(feedback(&findings))
                }
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn diff_helpers() {
        assert_eq!(changed_lines("--- a\n+++ b\n@@ -1 +1 @@\n+x\n-y\n z\n"), 2);
        assert_eq!(
            changed_lines(
                "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n--- SQL comment\n+++ SQL comment\n"
            ),
            2
        );
        assert!(task("fix it", "+x").contains(NO_ISSUES));
        assert!(!wanted(Switch::On, 0));
        assert!(wanted(Switch::On, 1));
        assert!(!wanted(Switch::Auto, MIN_LINES - 1));
        assert!(wanted(Switch::Auto, MIN_LINES));
        assert!(!wanted(Switch::Off, 500));
    }

    #[test]
    fn a_fenced_no_issues_reply_passes() {
        assert!(no_issues("NO_ISSUES"));
        assert!(no_issues("```\nNO_ISSUES\n```"));
        assert!(no_issues("  `NO_ISSUES`\n"));
        assert!(no_issues(""));
        assert!(!no_issues("```\nfile:1 - problem\n```"));
    }

    #[test]
    fn both_reviews_hold_the_change_to_the_project_rules() {
        for prompt in [task("fix it", "+x"), commit_task("修一下", "+x")] {
            assert!(prompt.contains("Project instructions"), "{prompt}");
            assert!(prompt.contains("quote the rule"), "{prompt}");
            assert!(prompt.contains(NO_ISSUES), "{prompt}");
        }
        assert!(commit_task("修一下", "+x").contains("<request>\n修一下\n</request>"));
        assert!(commit_task("", "+x").contains("language of the project instructions"));
        let task = with_rules("review".into(), Some("- 不要用 print 调试\n".into()));
        assert!(
            task.contains("<project_rules>\n- 不要用 print 调试\n</project_rules>"),
            "{task}"
        );
        assert_eq!(with_rules("review".into(), None), "review");
    }
}
