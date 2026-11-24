//! Reviews of a change, at two points, by one reviewer per point
//! (docs/optimizations/0021, 0028):
//! - at the end of a task, after the automatic verification passed: the project's
//!   review command (`review_commands`), up to `review_max_rounds` rounds, or else
//!   the built-in self-review (`done_self_review`, off by default) once;
//! - before a commit, in the pre-commit gate and `/commit`: the project's review
//!   command when `review_when` includes `commit`, or else the built-in review
//!   (`commit_review`, on by default). A configured project review replaces both
//!   built-in ones.
//!
//! The built-in reviewer is a read-only sub-agent judging real defects and the
//! project's instruction files (AGENTS.md, CLAUDE.md, .cove.md), rules
//! (`.cove/rules.md`, read again for every review) and `review_instructions`.
//! `auto` for either built-in review means at least 40 changed lines.

use std::{
    collections::BTreeSet,
    path::PathBuf,
    sync::{Arc, Mutex},
};

use futures_util::future::BoxFuture;
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use super::{
    Evidence, ReviewRecord, Switch,
    review_command::{self, CommandReview, Finding, Point, Severity},
};
use crate::{
    domain::{Progress, Provider, Result},
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
/// `review_instructions` beyond this are cut, with a note.
pub const MAX_INSTRUCTIONS: usize = 16 * 1024;

/// What counts as a finding, shared by both reviews.
const CRITERIA: &str = "Look for real defects: wrong logic, unhandled errors or edge cases, callers or tests the change breaks, security problems. \
The project's instruction files (\"Project instructions\" in your system prompt) are this repository's rules: a change that breaks one of them is a finding even when it looks like style; quote the rule it breaks. \
Do not comment on style or naming that no project rule covers. \
The \"User preferences\" section is how the user likes to work, not a rule of this repository: never report a finding against it. \
Read the surrounding code with your tools when the diff is not enough.";

/// How the built-in reviewer writes its findings, parsed like a review command's markers.
const FORMAT: &str = "Otherwise list each finding on its own line as: [SEVERITY] file:line - the problem - why it is one, where SEVERITY is CRITICAL, HIGH, MEDIUM or LOW.";

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
    pub rules: Option<Arc<crate::memory::rules::RulesFile>>,
    /// `review_instructions`: the project's review criteria, read when a review starts.
    pub instructions: Option<PathBuf>,
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

/// `task` with the project's review instructions (`review_instructions`), whose own
/// output format and delivery are for CI, not for this review.
fn with_instructions(task: String, name: &str, text: Option<&str>) -> String {
    let Some(text) = text.map(str::trim).filter(|text| !text.is_empty()) else {
        return task;
    };
    let text = crate::text::clip(
        text,
        MAX_INSTRUCTIONS,
        "\n... [review instructions truncated at 16 KB]",
    );
    format!(
        "{task}\n\nThe project's review instructions ({name}) are review criteria too: apply their scope, rules and severity levels. Where they describe an output format, where to write the result or what to print, ignore that and answer in the format asked above.\n<review_instructions>\n{text}\n</review_instructions>"
    )
}

#[derive(Clone, Debug, PartialEq)]
pub enum Verdict {
    Passed,
    /// The findings, every level.
    Issues(Vec<Finding>),
    /// A review command's output follows none of the formats cove reads.
    Unparsed(String),
    /// The review did not run or finish (why): never a pass, never blocks a commit.
    Skipped(String),
}

/// Whether a reviewer's reply says it found nothing; a code fence around the
/// verdict is common and must not turn it into a finding.
fn no_issues(text: &str) -> bool {
    let text = text.trim_matches(|c: char| c.is_whitespace() || c == '`');
    text.is_empty() || text.starts_with(NO_ISSUES)
}

/// The built-in reviewer's findings: its `[HIGH]` lines, or the whole reply as one
/// finding when it wrote none (anything but `NO_ISSUES` is a finding, as before).
fn builtin_findings(text: &str) -> Vec<Finding> {
    match review_command::parse(text) {
        Some(findings) if !findings.is_empty() => findings,
        _ => vec![Finding {
            severity: Severity::High,
            file: None,
            line: None,
            title: text.trim().to_owned(),
            detail: String::new(),
        }],
    }
}

/// The findings, one per line, cut at the verification gate's detail limit.
pub fn findings_text(findings: &[Finding]) -> String {
    let text = findings
        .iter()
        .map(Finding::full)
        .collect::<Vec<_>>()
        .join("\n");
    crate::text::clip(
        &text,
        review_command::DETAIL_LIMIT,
        "\n... [findings truncated]",
    )
}

impl Reviewer {
    fn instructions(&self) -> Option<(String, String)> {
        let path = self.instructions.as_ref()?;
        let data = std::fs::read(path).ok()?;
        let text = String::from_utf8_lossy(&data).into_owned();
        Some((crate::paths::display(path), text))
    }

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
        let agent = match agent {
            Ok(agent) => agent,
            Err(err) => return Verdict::Skipped(format!("审查子 Agent 无法启动：{err}")),
        };
        let agent = agent
            .with_project_context(self.settings.project_context.clone())
            .with_instructions(Some("You are a read-only code reviewer. You cannot edit files or run commands. Report only real findings, with file and line.".into()))
            .with_long_cache(false);
        let task = with_rules(task, self.rules.as_ref().and_then(|rules| rules.current()));
        let task = match self.instructions() {
            Some((name, text)) => with_instructions(task, &name, Some(&text)),
            None => task,
        };
        let (result, spent, text) = agent.run_detached(task, cancel).await;
        self.ledger.record(&spent);
        if let Err(err) = result {
            return Verdict::Skipped(format!("审查子 Agent 没有完成：{err}"));
        }
        let text = text.trim_matches(|c: char| c.is_whitespace() || c == '`');
        if no_issues(text) {
            Verdict::Passed
        } else {
            Verdict::Issues(builtin_findings(text))
        }
    }
}

/// The review before a commit, shared by the pre-commit gate and `/commit`.
pub enum CommitReview {
    /// `commit_review`: `On` or `Auto` (an `Off` review is not built).
    Builtin { mode: Switch, reviewer: Reviewer },
    /// `review_commands` with `review_when` `commit` or `both`.
    Project(Arc<CommandReview>),
}

impl CommitReview {
    /// Whether this staged diff is reviewed.
    pub fn wanted(&self, diff: &str) -> bool {
        match self {
            Self::Builtin { mode, .. } => wanted(*mode, changed_lines(diff)),
            Self::Project(_) => changed_lines(diff) > 0,
        }
    }

    /// For the progress line: what reviews the change.
    pub fn describe(&self) -> &'static str {
        match self {
            Self::Builtin { .. } => "按项目规则审查暂存的改动",
            Self::Project(_) => "运行项目的审查命令",
        }
    }

    /// The verdict on the staged change; `Issues` holds only what blocks the commit.
    pub async fn review(&self, request: &str, diff: &str, cancel: CancellationToken) -> Verdict {
        match self {
            Self::Builtin { reviewer, .. } => {
                reviewer.review(commit_task(request, diff), cancel).await
            }
            Self::Project(review) => {
                let tampered = review.tampered();
                if !tampered.is_empty() {
                    // A skipped review lets a commit through; one whose command was
                    // changed must not.
                    return Verdict::Issues(vec![Finding {
                        severity: Severity::Critical,
                        file: tampered.first().cloned(),
                        line: None,
                        title: format!(
                            "the project's review depends on {} and it changed since cove started",
                            tampered.join(", ")
                        ),
                        detail: "Restore it; do not change the review command or its configuration to pass the review. If it really needs a change, tell the user.".into(),
                    }]);
                }
                if review.passed_now() {
                    return Verdict::Passed;
                }
                match review.run(Point::Commit, 1, None, request, cancel).await {
                    Err(err) => Verdict::Skipped(err.to_string()),
                    Ok(run) => match run.verdict {
                        Verdict::Issues(findings) => {
                            let (blocking, _) = review.blocking(&findings);
                            if blocking.is_empty() {
                                Verdict::Passed
                            } else {
                                Verdict::Issues(blocking)
                            }
                        }
                        // An unreadable review does not hold every commit back.
                        Verdict::Unparsed(_) => Verdict::Skipped(
                            "审查命令的输出格式无法识别（JSON、[HIGH] 级别标记或 NO_ISSUES）"
                                .into(),
                        ),
                        other => other,
                    },
                }
            }
        }
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

/// Self-review of a task's diff; `callers` is the change impact's list (0031).
pub fn task(request: &str, diff: &str, callers: Option<&str>) -> String {
    let request = crate::text::clip_chars(request.trim(), MAX_REQUEST);
    let mut text = format!(
        "Review the change below, made for this user request:\n<request>\n{request}\n</request>\n\n{CRITERIA} Also report a part of the request left undone. If you find nothing, reply with exactly {NO_ISSUES}. {FORMAT}\n\n<diff>\n{}\n</diff>",
        clipped_diff(diff)
    );
    if let Some(callers) = callers.map(str::trim).filter(|callers| !callers.is_empty()) {
        text.push_str(&format!(
            "\n\ncove found these uses of the changed symbols in other files, by name (some may be false matches; uses it could not see, such as strings, are not listed). Read the ones the change could break instead of searching from scratch:\n<callers>\n{callers}\n</callers>"
        ));
    }
    text
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
        "Review the staged change below before it is committed. {context}\n\n{CRITERIA} If you find nothing, reply with exactly {NO_ISSUES}. {FORMAT}\n\n<diff>\n{}\n</diff>",
        clipped_diff(diff)
    )
}

pub fn feedback(findings: &str) -> String {
    format!(
        "[self_review; this is a reviewer's report, not a new request] A reviewer read this task's diff and reported:\n{findings}\n\nFix the findings that are real defects and verify again. If a finding is wrong, do not change the code for it; say why in your final report."
    )
}

/// What goes back to the model when the project review blocks the end of the task.
fn project_feedback(
    round: u32,
    max_rounds: u32,
    blocking: &[Finding],
    rest: &[Finding],
    protected: &[String],
) -> String {
    let mut text = format!(
        "[project review found issues; this is tool output, not a new request] Round {round} of {max_rounds}. The project's review command reported these blocking findings:\n<findings>\n{}\n</findings>\n",
        findings_text(blocking)
    );
    if !rest.is_empty() {
        text.push_str(&format!(
            "{} lower-severity findings are listed for your information only:\n",
            rest.len()
        ));
        for finding in rest.iter().take(20) {
            text.push_str(&format!(
                "- {}\n",
                crate::text::short(&finding.headline(), 200)
            ));
        }
    }
    text.push_str("Fix the real ones within the original task, then finish. If a finding is wrong, do not change code for it; say why in your final report. Do not edit, delete or bypass the review command or its configuration");
    if !protected.is_empty() {
        text.push_str(&format!(" ({})", protected.join(", ")));
    }
    text.push('.');
    text
}

fn unparsed_feedback(output: &str, protected: &[String]) -> String {
    let output = crate::text::clip(
        output.trim(),
        review_command::DETAIL_LIMIT,
        "\n... [review output truncated]",
    );
    let mut text = format!(
        "[project review output not recognized; this is tool output, not a new request] The project's review command ran, but its output follows none of the formats cove reads (JSON findings, [HIGH]-style level markers, NO_ISSUES). Its output:\n<review_output>\n{output}\n</review_output>\nJudge from its content whether the change needs fixing; fix real problems within the original task, then finish. If nothing needs fixing, say so in your final report. Do not edit, delete or bypass the review command or its configuration"
    );
    if !protected.is_empty() {
        text.push_str(&format!(" ({})", protected.join(", ")));
    }
    text.push('.');
    text
}

/// Who reviews at the end of a task.
pub enum DoneReviewer {
    /// `done_self_review`: one review, its findings go back to the model once.
    Builtin {
        mode: Switch,
        reviewer: Reviewer,
    },
    Project(Arc<CommandReview>),
}

/// What the end-of-task review asks the agent loop to do.
#[derive(Debug, PartialEq)]
pub enum ReviewStep {
    /// Nothing to review, or nothing new to say.
    Quiet,
    /// The review is over for this task: the line for the user.
    Concluded(String),
    /// Send `message` to the model and go on; `digest` identifies the findings, so a
    /// round that brings different ones counts as progress (0032).
    HandBack { message: String, digest: String },
}

#[derive(Default)]
struct LoopState {
    /// Reviews run in this task.
    rounds: u32,
    /// The workspace fingerprint of the last review.
    reviewed: Option<u64>,
    /// The last blocking findings handed back, and their identity.
    blocking: Vec<Finding>,
    identity: Option<BTreeSet<(Severity, Option<String>)>>,
    /// The last round's output, for `COVE_REVIEW_PREVIOUS`.
    output: Option<String>,
    unparsed_sent: bool,
    /// The review of this task is over.
    done: bool,
}

/// The end-of-task review loop, run by the agent loop after the automatic verification.
pub struct DoneReview {
    reviewer: DoneReviewer,
    workspace: Arc<Workspace>,
    evidence: Arc<Evidence>,
    progress: Option<Arc<Progress>>,
    state: Mutex<LoopState>,
}

impl DoneReview {
    pub fn new(
        reviewer: DoneReviewer,
        workspace: Arc<Workspace>,
        evidence: Arc<Evidence>,
        progress: Option<Arc<Progress>>,
    ) -> Self {
        Self {
            reviewer,
            workspace,
            evidence,
            progress,
            state: Mutex::new(LoopState::default()),
        }
    }

    async fn say(&self, text: String) {
        if let Some(progress) = &self.progress {
            progress.line(text, false).await;
        }
    }

    fn kind(&self) -> &'static str {
        match self.reviewer {
            DoneReviewer::Builtin { .. } => "builtin",
            DoneReviewer::Project(_) => "project",
        }
    }

    fn label(&self) -> &'static str {
        match self.reviewer {
            DoneReviewer::Builtin { .. } => "[自审]",
            DoneReviewer::Project(_) => "[审查]",
        }
    }

    /// Ends the review of this task: the record for the acceptance report, the line
    /// for the user.
    fn conclude(
        &self,
        state: &mut LoopState,
        outcome: &str,
        open: &[Finding],
        line: String,
    ) -> ReviewStep {
        state.done = true;
        self.evidence.set_review(ReviewRecord {
            reviewer: self.kind().into(),
            outcome: outcome.into(),
            rounds: state.rounds,
            open: open.iter().map(Finding::headline).collect(),
            note: line.clone(),
        });
        ReviewStep::Concluded(line)
    }

    fn headlines(findings: &[Finding]) -> String {
        let text = findings
            .iter()
            .take(3)
            .map(|finding| crate::text::short(&finding.headline(), 120))
            .collect::<Vec<_>>()
            .join("；");
        if findings.len() > 3 {
            format!("{text} 等")
        } else {
            text
        }
    }

    /// The task ends with checks that failed or did not run: the change is not reviewed
    /// (fix the checks first); the line for the user, when there was a change to review.
    /// The built-in review stays quiet, as it did before it moved after the checks.
    pub fn unchecked(&self) -> Option<String> {
        if let DoneReviewer::Builtin { .. } = self.reviewer {
            return None;
        }
        let mut state = self.state.lock().ok()?;
        if state.done || self.workspace.changed_paths().ok()?.is_empty() {
            return None;
        }
        let line = format!("{} 未运行：自动验证没有通过", self.label());
        match self.conclude(&mut state, "not_run", &[], line) {
            ReviewStep::Concluded(line) => Some(line),
            _ => None,
        }
    }

    /// The model is about to finish and the checks passed (or none applied).
    /// `near_limit`: too few model calls left to fix anything. Boxed: the built-in
    /// reviewer is an agent run inside the agent loop that calls this.
    pub fn review<'a>(
        &'a self,
        session: &'a Session,
        near_limit: bool,
        cancel: CancellationToken,
    ) -> BoxFuture<'a, Result<ReviewStep>> {
        Box::pin(async move {
            let label = self.label();
            let mut state = match self.state.lock() {
                Ok(mut state) => std::mem::take(&mut *state),
                Err(_) => return Ok(ReviewStep::Quiet),
            };
            let step = self
                .step(&mut state, session, near_limit, cancel, label)
                .await;
            if let Ok(mut slot) = self.state.lock() {
                *slot = state;
            }
            step
        })
    }

    async fn step(
        &self,
        state: &mut LoopState,
        session: &Session,
        near_limit: bool,
        cancel: CancellationToken,
        label: &str,
    ) -> Result<ReviewStep> {
        if state.done || self.workspace.changed_paths()?.is_empty() {
            return Ok(ReviewStep::Quiet);
        }
        let fingerprint = self.workspace.fingerprint()?;
        if state.rounds > 0 && fingerprint == state.reviewed {
            // Handed back, and the model changed nothing: it does not accept the
            // findings; reviewing again would only roll the dice again.
            let blocking = std::mem::take(&mut state.blocking);
            let line = if blocking.is_empty() {
                format!("{label} 交回模型后没有新的改动；审查输出格式无法识别，不算通过")
            } else {
                format!(
                    "{label} 第 {} 轮后仍有 {}：{}；交回后模型没有修改，原因见上面的回复",
                    state.rounds,
                    review_command::counts(&blocking),
                    Self::headlines(&blocking)
                )
            };
            let outcome = if blocking.is_empty() {
                "unparsed"
            } else {
                "failed"
            };
            return Ok(self.conclude(state, outcome, &blocking, line));
        }
        if near_limit {
            let line = format!("{label} 未运行：快用完模型调用次数，没有余量修改");
            return Ok(self.conclude(state, "not_run", &[], line));
        }
        let round = state.rounds + 1;
        let (verdict, max_rounds, block) = match &self.reviewer {
            DoneReviewer::Builtin { mode, reviewer } => {
                let diff = self.workspace.diff()?;
                let lines = changed_lines(&diff);
                if !wanted(*mode, lines) {
                    return Ok(ReviewStep::Quiet);
                }
                self.say(format!(
                    "{label} 审查本任务的改动（{} 个文件，{lines} 行）…",
                    self.workspace.changed_paths()?.len()
                ))
                .await;
                let verdict = reviewer
                    .review(
                        task(
                            &session.current_request,
                            &diff,
                            self.evidence.callers().as_deref(),
                        ),
                        cancel,
                    )
                    .await;
                (verdict, 1, Severity::Low)
            }
            DoneReviewer::Project(review) => {
                let run = review
                    .run(
                        Point::Done,
                        round,
                        state.output.as_deref(),
                        &session.current_request,
                        cancel,
                    )
                    .await?;
                if !run.output.is_empty() {
                    state.output = Some(run.output);
                }
                (run.verdict, review.config.max_rounds, review.config.block)
            }
        };
        state.rounds = round;
        state.reviewed = fingerprint;
        let protected = match &self.reviewer {
            DoneReviewer::Project(review) => review.protected(),
            DoneReviewer::Builtin { .. } => Vec::new(),
        };
        let passed = |state: &mut LoopState, rest: &[Finding]| {
            if let DoneReviewer::Project(review) = &self.reviewer {
                review.note_passed(fingerprint);
            }
            let mut line = format!("{label} 第 {round} 轮通过");
            if !rest.is_empty() {
                line.push_str(&format!(
                    "（另有 {}，未要求处理：{}）",
                    review_command::counts(rest),
                    Self::headlines(rest)
                ));
            }
            self.conclude(state, "passed", rest, line)
        };
        Ok(match verdict {
            Verdict::Skipped(reason) => {
                let line = format!("{label} 未运行：{reason}");
                self.conclude(state, "not_run", &[], line)
            }
            Verdict::Passed => passed(state, &[]),
            Verdict::Issues(findings) => {
                let (blocking, rest): (Vec<Finding>, Vec<Finding>) = findings
                    .into_iter()
                    .partition(|finding| finding.severity >= block);
                if blocking.is_empty() {
                    return Ok(passed(state, &rest));
                }
                if let DoneReviewer::Builtin { .. } = self.reviewer {
                    // The built-in review hands its findings back once, as it always did.
                    state.done = true;
                    self.evidence.set_review(ReviewRecord {
                        reviewer: self.kind().into(),
                        outcome: "handed_back".into(),
                        rounds: round,
                        open: blocking.iter().map(Finding::headline).collect(),
                        note: String::new(),
                    });
                    self.say(format!("{label} 发现问题，已交回模型处理")).await;
                    return Ok(ReviewStep::HandBack {
                        message: feedback(&findings_text(&blocking)),
                        digest: format!("builtin:{:?}", review_command::identity(&blocking)),
                    });
                }
                let identity = review_command::identity(&blocking);
                if round >= max_rounds {
                    let line = format!(
                        "{label} {round} 轮后仍有 {}：{}；没修的原因见上面的回复",
                        review_command::counts(&blocking),
                        Self::headlines(&blocking)
                    );
                    return Ok(self.conclude(state, "failed", &blocking, line));
                }
                if state.identity.as_ref() == Some(&identity) {
                    let line = format!(
                        "{label} 第 {round} 轮的阻断问题和上一轮相同（{}：{}），不再重审；原因见上面的回复",
                        review_command::counts(&blocking),
                        Self::headlines(&blocking)
                    );
                    return Ok(self.conclude(state, "failed", &blocking, line));
                }
                self.say(format!(
                    "{label} 第 {round} 轮发现 {}，交回模型修改（最多 {max_rounds} 轮）",
                    review_command::counts(&blocking)
                ))
                .await;
                let message = project_feedback(round, max_rounds, &blocking, &rest, &protected);
                let digest = format!("project:{identity:?}");
                state.identity = Some(identity);
                state.blocking = blocking;
                ReviewStep::HandBack { message, digest }
            }
            Verdict::Unparsed(output) => {
                if round == 1 && !state.unparsed_sent {
                    state.unparsed_sent = true;
                    state.blocking.clear();
                    self.say(format!("{label} 输出格式无法识别，把输出交给模型判断一次"))
                        .await;
                    ReviewStep::HandBack {
                        message: unparsed_feedback(&output, &protected),
                        digest: format!("unparsed:{}", crate::text::short(&output, 200)),
                    }
                } else {
                    let line = format!(
                        "{label} 输出格式无法识别，不算通过；请按约定调整审查命令的输出（JSON、[HIGH] 级别标记或首行 NO_ISSUES）"
                    );
                    self.conclude(state, "unparsed", &[], line)
                }
            }
        })
    }
}

impl TurnHook for DoneReview {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = LoopState::default();
        }
        Box::pin(async { None })
    }
}

/// Refuses `write`/`edit` of the files the project review runs (0028 P8); shell
/// changes are caught when the next review finds the files changed.
pub struct ReviewGuard(pub Arc<CommandReview>);

impl TurnHook for ReviewGuard {
    fn before_tool(&self, name: &str, input: &Value) -> Option<String> {
        if !matches!(name, "write" | "edit") {
            return None;
        }
        let path = ["path", "file_path", "filePath"]
            .iter()
            .find_map(|key| input.get(*key).and_then(Value::as_str))?;
        self.0.protects(path).then(|| {
            format!(
                "[project review] {path} is part of the project's review (review_commands and its configuration); it cannot be changed to pass the review. If it really needs a change, tell the user and let them make it."
            )
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
        assert!(task("fix it", "+x", None).contains(NO_ISSUES));
        assert!(!task("fix it", "+x", None).contains("<callers>"));
        let with = task(
            "fix it",
            "+x",
            Some("- b.py:3 uses price (signature changed in a.py:1)"),
        );
        assert!(
            with.ends_with(
                "<callers>\n- b.py:3 uses price (signature changed in a.py:1)\n</callers>"
            ),
            "{with}"
        );
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
    fn builtin_findings_use_markers_and_keep_any_other_reply() {
        let findings =
            builtin_findings("[HIGH] src/a.rs:3 - unwrap on None - panics\n[LOW] b.rs:1 - typo");
        assert_eq!(findings.len(), 2);
        assert_eq!(findings[0].file.as_deref(), Some("src/a.rs"));
        let findings = builtin_findings("src/a.rs:3 - unwrap on None");
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].severity, Severity::High);
        assert!(findings_text(&findings).contains("unwrap on None"));
    }

    #[test]
    fn both_reviews_hold_the_change_to_the_project_rules() {
        for prompt in [task("fix it", "+x", None), commit_task("修一下", "+x")] {
            assert!(prompt.contains("Project instructions"), "{prompt}");
            assert!(prompt.contains("quote the rule"), "{prompt}");
            assert!(prompt.contains(NO_ISSUES), "{prompt}");
            assert!(prompt.contains("[SEVERITY] file:line"), "{prompt}");
            // The user's preferences are working style, never a finding (0030).
            assert!(
                prompt.contains("\"User preferences\" section is how the user likes to work"),
                "{prompt}"
            );
        }
        assert!(commit_task("修一下", "+x").contains("<request>\n修一下\n</request>"));
        assert!(commit_task("", "+x").contains("language of the project instructions"));
        let task = with_rules("review".into(), Some("- 不要用 print 调试\n".into()));
        assert!(
            task.contains("<project_rules>\n- 不要用 print 调试\n</project_rules>"),
            "{task}"
        );
        assert_eq!(with_rules("review".into(), None), "review");
        let task = with_instructions("review".into(), "eng/SKILL.md", Some("最多 8 条"));
        assert!(task.contains("<review_instructions>\n最多 8 条\n</review_instructions>"));
        assert!(task.contains("ignore that and answer in the format asked above"));
        let long = "x".repeat(MAX_INSTRUCTIONS + 10);
        assert!(with_instructions("r".into(), "a", Some(&long)).contains("truncated at 16 KB"));
        assert_eq!(with_instructions("r".into(), "a", Some("  ")), "r");
    }

    #[test]
    fn project_feedback_lists_blocking_findings_in_full_and_others_by_title() {
        let blocking =
            review_command::parse("[HIGH] src/a.py:12 - 空列表时除零 - items 为空").unwrap();
        let rest = review_command::parse("[LOW] src/a.py:3 - 命名").unwrap();
        let text = project_feedback(1, 3, &blocking, &rest, &["tools/review.py".into()]);
        assert!(text.starts_with("[project review found issues; this is tool output, not a new request] Round 1 of 3."), "{text}");
        assert!(
            text.contains("<findings>\n[HIGH] src/a.py:12 空列表时除零 - items 为空\n</findings>"),
            "{text}"
        );
        assert!(text.contains("1 lower-severity findings"), "{text}");
        assert!(text.contains("(tools/review.py)."), "{text}");
    }

    /// A workspace whose review command prints `out.txt`, exits with `code.txt` (0 when
    /// absent) and logs its `COVE_REVIEW_*` variables to `env.log`.
    struct Fixture {
        _dirs: (tempfile::TempDir, tempfile::TempDir),
        root: std::path::PathBuf,
        workspace: Arc<Workspace>,
        evidence: Arc<Evidence>,
        session: Session,
    }

    const SCRIPT: &str = "import os, sys\nwith open('env.log', 'a') as log:\n    log.write('%s %s %s %s\\n' % (os.environ.get('COVE_REVIEW_POINT'), os.environ.get('COVE_REVIEW_ROUND'), 'previous' if os.path.isfile(os.environ.get('COVE_REVIEW_PREVIOUS', '')) else '-', open(os.environ['COVE_REVIEW_REQUEST'], encoding='utf-8').read()))\nsys.stdout.write(open('out.txt', encoding='utf-8').read())\nsys.exit(int(open('code.txt').read()) if os.path.exists('code.txt') else 0)\n";

    fn python() -> &'static str {
        if cfg!(windows) { "python" } else { "python3" }
    }

    impl Fixture {
        fn new() -> Self {
            let data = tempfile::tempdir().unwrap();
            let dir = tempfile::tempdir().unwrap();
            let root = dir.path().canonicalize().unwrap();
            std::fs::write(root.join("review.py"), SCRIPT).unwrap();
            std::fs::write(root.join("a.py"), "x = 1\n").unwrap();
            let store = crate::session::Store::open(data.path()).unwrap();
            let workspace = Arc::new(Workspace::new(root.clone(), store).unwrap());
            workspace.begin().unwrap();
            let mut session = Session::new(root.clone(), String::new(), String::new(), None);
            session.current_request = "fix a.py".into();
            Self {
                _dirs: (data, dir),
                root,
                workspace,
                evidence: Arc::new(Evidence::default()),
                session,
            }
        }

        fn review(&self, config: serde_json::Value) -> (DoneReview, Arc<CommandReview>) {
            let mut config = config;
            config["review_commands"] = serde_json::json!([format!("{} review.py", python())]);
            let project = Arc::new(CommandReview::new(
                self.root.clone(),
                review_command::ReviewConfig::from_json(&config),
                self.workspace.clone(),
            ));
            let review = DoneReview::new(
                DoneReviewer::Project(project.clone()),
                self.workspace.clone(),
                self.evidence.clone(),
                None,
            );
            (review, project)
        }

        /// A file-tool edit of a.py, as the model makes it.
        fn edit(&self, text: &str) {
            let path = self.root.join("a.py");
            let old = std::fs::read_to_string(&path).unwrap();
            self.workspace.replace(&path, Some(&old), text).unwrap();
        }

        fn output(&self, text: &str) {
            std::fs::write(self.root.join("out.txt"), text).unwrap();
        }

        async fn step(&self, review: &DoneReview) -> ReviewStep {
            review
                .review(&self.session, false, CancellationToken::new())
                .await
                .unwrap()
        }

        fn runs(&self) -> Vec<String> {
            std::fs::read_to_string(self.root.join("env.log"))
                .unwrap_or_default()
                .lines()
                .map(str::to_owned)
                .collect()
        }
    }

    const HIGH_A: &str = "[HIGH] a.py:1 - divides by zero - empty list\n[LOW] a.py:1 - naming\n";

    #[tokio::test]
    async fn the_loop_hands_back_until_the_review_passes() {
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        // No change yet: nothing to review.
        fx.output(HIGH_A);
        assert_eq!(fx.step(&review).await, ReviewStep::Quiet);
        assert!(fx.runs().is_empty());
        fx.edit("x = 2\n");
        let ReviewStep::HandBack { message, .. } = fx.step(&review).await else {
            panic!("expected a hand-back");
        };
        assert!(message.contains("Round 1 of 3"), "{message}");
        assert!(
            message.contains("[HIGH] a.py:1 divides by zero"),
            "{message}"
        );
        assert!(message.contains("- [LOW] a.py:1 naming"), "{message}");
        assert!(message.contains("(review.py)"), "{message}");
        fx.edit("x = 3\n");
        fx.output("NO_ISSUES\n");
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert_eq!(line, "[审查] 第 2 轮通过");
        // Over for this task: no third run.
        assert_eq!(fx.step(&review).await, ReviewStep::Quiet);
        let runs = fx.runs();
        assert_eq!(runs, ["done 1 - fix a.py", "done 2 previous fix a.py"]);
        let record = fx.evidence.review().unwrap();
        assert_eq!((record.outcome.as_str(), record.rounds), ("passed", 2));
        // A new task starts over.
        review.task_note(&fx.session).await;
        fx.edit("x = 4\n");
        assert!(matches!(fx.step(&review).await, ReviewStep::Concluded(_)));
        assert_eq!(fx.runs().len(), 3);
    }

    #[tokio::test]
    async fn the_loop_stops_at_the_round_limit_on_the_same_findings_or_without_a_change() {
        // Same level and file twice: stop, whatever the wording.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        fx.output(HIGH_A);
        fx.edit("x = 2\n");
        assert!(matches!(
            fx.step(&review).await,
            ReviewStep::HandBack { .. }
        ));
        fx.edit("x = 3\n");
        fx.output("**[HIGH] Zero quantity**: `a.py:7`\n");
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(line.contains("和上一轮相同"), "{line}");
        assert_eq!(fx.evidence.review().unwrap().outcome, "failed");

        // The round limit.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({"review_max_rounds": 2}));
        fx.output(HIGH_A);
        fx.edit("x = 2\n");
        assert!(matches!(
            fx.step(&review).await,
            ReviewStep::HandBack { .. }
        ));
        fx.edit("x = 3\n");
        fx.output("[HIGH] b.py:1 - another\n");
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(line.starts_with("[审查] 2 轮后仍有 1 条 HIGH"), "{line}");
        assert_eq!(fx.runs().len(), 2);

        // Handed back and nothing changed: no second run.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        fx.output(HIGH_A);
        fx.edit("x = 2\n");
        assert!(matches!(
            fx.step(&review).await,
            ReviewStep::HandBack { .. }
        ));
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(line.contains("模型没有修改"), "{line}");
        assert_eq!(fx.runs().len(), 1);

        // Below `review_block`: passed, the rest listed for the user only.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        fx.output("[MEDIUM] a.py:1 - wording\n[LOW] a.py:2 - naming\n");
        fx.edit("x = 2\n");
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(
            line.starts_with("[审查] 第 1 轮通过（另有 1 条 MEDIUM、1 条 LOW，未要求处理"),
            "{line}"
        );
        // A stricter `review_block` hands the same output back.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({"review_block": "medium"}));
        fx.output("[MEDIUM] a.py:1 - wording\n");
        fx.edit("x = 2\n");
        assert!(matches!(
            fx.step(&review).await,
            ReviewStep::HandBack { .. }
        ));
    }

    #[tokio::test]
    async fn a_review_that_did_not_run_or_cannot_be_read_is_not_a_pass() {
        // A non-zero exit: not run, nothing handed back.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        fx.output("NO_ISSUES\n");
        std::fs::write(fx.root.join("code.txt"), "1").unwrap();
        fx.edit("x = 2\n");
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(
            line.starts_with("[审查] 未运行：") && line.contains("退出码 1"),
            "{line}"
        );
        assert_eq!(fx.evidence.review().unwrap().outcome, "not_run");

        // Unreadable output: handed back once, then not a pass.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        fx.output("Looks fine overall, a few nits.\n");
        fx.edit("x = 2\n");
        let ReviewStep::HandBack { message, .. } = fx.step(&review).await else {
            panic!("expected a hand-back");
        };
        assert!(message.contains("Looks fine overall"), "{message}");
        fx.edit("x = 3\n");
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(line.contains("输出格式无法识别，不算通过"), "{line}");
        assert_eq!(fx.evidence.review().unwrap().outcome, "unparsed");

        // A changed review script: not run at all.
        let fx = Fixture::new();
        let (review, project) = fx.review(serde_json::json!({}));
        fx.output(HIGH_A);
        fx.edit("x = 2\n");
        std::fs::write(fx.root.join("review.py"), "print('NO_ISSUES')\n").unwrap();
        let ReviewStep::Concluded(line) = fx.step(&review).await else {
            panic!("expected a conclusion");
        };
        assert!(line.contains("被改动或删除：review.py"), "{line}");
        assert!(fx.runs().is_empty());
        assert!(project.protects("review.py"));

        // Near the call limit: not run.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        fx.output(HIGH_A);
        fx.edit("x = 2\n");
        let step = review
            .review(&fx.session, true, CancellationToken::new())
            .await
            .unwrap();
        assert!(matches!(step, ReviewStep::Concluded(line) if line.contains("未运行")));
        assert!(fx.runs().is_empty());

        // Checks that did not pass: no review, said once.
        let fx = Fixture::new();
        let (review, _) = fx.review(serde_json::json!({}));
        assert_eq!(review.unchecked(), None);
        fx.output(HIGH_A);
        fx.edit("x = 2\n");
        assert_eq!(
            review.unchecked().as_deref(),
            Some("[审查] 未运行：自动验证没有通过")
        );
        assert_eq!(review.unchecked(), None);
        assert_eq!(fx.step(&review).await, ReviewStep::Quiet);
        assert!(fx.runs().is_empty());
        assert_eq!(fx.evidence.review().unwrap().outcome, "not_run");
    }

    #[tokio::test]
    async fn the_command_is_bounded_and_cancellable() {
        let fx = Fixture::new();
        // Not found.
        let mut config = review_command::ReviewConfig::from_json(
            &serde_json::json!({"review_timeout_seconds": 1}),
        );
        config.commands = vec!["cove-no-such-review-command --x".into()];
        let review = CommandReview::new(fx.root.clone(), config.clone(), fx.workspace.clone());
        let run = review
            .run(Point::Done, 1, None, "", CancellationToken::new())
            .await
            .unwrap();
        assert!(
            matches!(run.verdict, Verdict::Skipped(_)),
            "{:?}",
            run.verdict
        );
        // Timed out.
        config.commands = vec![format!("{} -c \"import time; time.sleep(30)\"", python())];
        let review = CommandReview::new(fx.root.clone(), config.clone(), fx.workspace.clone());
        let started = std::time::Instant::now();
        let run = review
            .run(Point::Done, 1, None, "", CancellationToken::new())
            .await
            .unwrap();
        assert!(
            matches!(&run.verdict, Verdict::Skipped(reason) if reason.contains("超过 1 秒")),
            "{:?}",
            run.verdict
        );
        assert!(started.elapsed() < std::time::Duration::from_secs(20));
        // Cancelled.
        config.timeout = std::time::Duration::from_secs(60);
        let review = CommandReview::new(fx.root.clone(), config, fx.workspace.clone());
        let cancel = CancellationToken::new();
        let stop = cancel.clone();
        tokio::spawn(async move {
            tokio::time::sleep(std::time::Duration::from_millis(500)).await;
            stop.cancel();
        });
        let started = std::time::Instant::now();
        let result = review.run(Point::Done, 1, None, "", cancel).await;
        assert!(matches!(result, Err(crate::domain::Error::Cancelled)));
        assert!(started.elapsed() < std::time::Duration::from_secs(20));
    }

    #[tokio::test]
    async fn the_commit_review_reuses_a_passed_end_of_task_review() {
        let fx = Fixture::new();
        let (review, project) = fx.review(serde_json::json!({"review_when": "both"}));
        let commit = CommitReview::Project(project.clone());
        fx.output("NO_ISSUES\n");
        fx.edit("x = 2\n");
        assert!(matches!(fx.step(&review).await, ReviewStep::Concluded(_)));
        assert!(project.passed_now());
        assert_eq!(
            commit.review("", "+x", CancellationToken::new()).await,
            Verdict::Passed
        );
        assert_eq!(fx.runs().len(), 1);
        // A different content runs it, at the commit point; only blocking findings block.
        fx.edit("x = 3\n");
        fx.output(HIGH_A);
        let Verdict::Issues(findings) = commit.review("", "+x", CancellationToken::new()).await
        else {
            panic!("expected issues");
        };
        assert_eq!(findings.len(), 1);
        assert_eq!(findings[0].severity, Severity::High);
        assert!(fx.runs()[1].starts_with("commit 1"), "{:?}", fx.runs());
        // A changed review command blocks the commit instead of skipping it.
        std::fs::write(fx.root.join("review.py"), "print('NO_ISSUES')\n").unwrap();
        let Verdict::Issues(findings) = commit.review("", "+x", CancellationToken::new()).await
        else {
            panic!("expected issues");
        };
        assert_eq!(findings[0].severity, Severity::Critical);
        // The guard refuses file-tool writes to it.
        let guard = ReviewGuard(project);
        assert!(
            guard
                .before_tool("edit", &serde_json::json!({"path": "review.py"}))
                .is_some()
        );
        assert!(
            guard
                .before_tool("write", &serde_json::json!({"file_path": "./review.py"}))
                .is_some()
        );
        assert!(
            guard
                .before_tool("edit", &serde_json::json!({"path": "a.py"}))
                .is_none()
        );
        assert!(
            guard
                .before_tool("read", &serde_json::json!({"path": "review.py"}))
                .is_none()
        );
    }
}
