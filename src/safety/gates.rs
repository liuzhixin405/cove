//! Completion gates and task guards, implemented as `TurnHook`s and tool wrappers:
//! guardrail and loop detection, finish nudges, the end-of-task review loop, plan
//! mode and the review workflow, the pre-commit gate and the acceptance report.

pub mod acceptance;
pub mod cite;
pub mod commit;
pub mod guard;
pub mod impact;
pub mod nudge;
pub mod plan;
pub mod review;
pub mod review_command;

use std::{collections::HashSet, sync::Mutex};

use serde::Serialize;
use serde_json::Value;

use crate::{
    engine::turn::TurnHook,
    safety::permission::{classify, shell::Dialect},
    tools::ToolOutput,
};

/// Config keys this module reads from the effective config JSON.
pub const CONFIG_KEYS: &[&str] = &[
    "done_check",
    "done_verify_tests",
    "done_self_review",
    "workflow",
    "commit_gate",
    "commit_review",
    "cite_check",
    "change_impact",
    "review_commands",
    "review_when",
    "review_block",
    "review_max_rounds",
    "review_timeout_seconds",
    "review_instructions",
];

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Switch {
    Off,
    On,
    Auto,
}

impl Switch {
    fn parse(value: Option<&Value>, default: Self) -> Self {
        match value {
            Some(Value::Bool(true)) => Self::On,
            Some(Value::Bool(false)) => Self::Off,
            Some(Value::String(text)) => match text.trim().to_ascii_lowercase().as_str() {
                "on" | "true" => Self::On,
                "off" | "false" => Self::Off,
                "auto" => Self::Auto,
                _ => default,
            },
            _ => default,
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub struct GateConfig {
    /// One "is the request fully met?" check per task; `auto` = non-Anthropic providers.
    pub done_check: Switch,
    /// Self-review sub-agent; `auto` = at least 40 changed lines. Default off.
    pub self_review: Switch,
    /// Project tests join the automatic verification gate (default on).
    pub verify_tests: bool,
    /// Pre-commit verification gate (default on); `off` keeps only the end-of-task check.
    pub commit_gate: Switch,
    /// AI review of the staged change before a commit (default on); `auto` = at least
    /// 40 changed lines.
    pub commit_review: Switch,
    /// Local check of the code references in answers that change no files (default
    /// `auto` = on, interactive or not, for every provider).
    pub cite_check: Switch,
    /// The callers of the changed symbols at the end of a task (0031, default `auto`).
    pub change_impact: Switch,
}

impl GateConfig {
    pub fn from_json(file: &Value) -> Self {
        Self {
            done_check: Switch::parse(file.get("done_check"), Switch::Auto),
            self_review: Switch::parse(file.get("done_self_review"), Switch::Off),
            verify_tests: file
                .get("done_verify_tests")
                .and_then(Value::as_bool)
                .unwrap_or(true),
            commit_gate: Switch::parse(file.get("commit_gate"), Switch::On),
            commit_review: Switch::parse(file.get("commit_review"), Switch::On),
            cite_check: Switch::parse(file.get("cite_check"), Switch::Auto),
            change_impact: Switch::parse(file.get("change_impact"), Switch::Auto),
        }
    }
}

/// A tool result built in one place, so new `ToolOutput` fields need one change here.
pub(crate) fn tool_output(content: String, failed: bool) -> ToolOutput {
    ToolOutput {
        content,
        failed,
        ..Default::default()
    }
}

/// Collapses whitespace so `cargo  test` and `cargo test` are the same check.
pub fn normalize_command(command: &str) -> String {
    command.split_whitespace().collect::<Vec<_>>().join(" ")
}

#[derive(Clone, Debug, Serialize, serde::Deserialize)]
pub struct CheckRecord {
    pub command: String,
    /// passed, failed, not_run, timed_out, reused
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    pub duration_ms: u128,
    /// Tail of the output, for failed checks.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub evidence: String,
}

#[derive(Clone, Debug, Serialize, serde::Deserialize)]
pub struct GitEvent {
    /// commit or push
    pub action: String,
    pub command: String,
    pub succeeded: bool,
}

/// How the end-of-task review of a task ended, for the acceptance report.
#[derive(Clone, Debug, Serialize, serde::Deserialize)]
pub struct ReviewRecord {
    /// project (`review_commands`) or builtin (`done_self_review`)
    pub reviewer: String,
    /// passed, failed, not_run, unparsed; handed_back for the built-in review
    pub outcome: String,
    pub rounds: u32,
    /// Findings left: the blocking ones when it failed, the others when it passed.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub open: Vec<String>,
    /// The line shown to the user.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub note: String,
}

#[derive(Default)]
struct EvidenceState {
    /// Shell lines that succeeded after the last change that could affect them.
    passed: HashSet<String>,
    /// A non-read-only tool succeeded this task.
    did_work: bool,
    checks: Vec<CheckRecord>,
    git: Vec<GitEvent>,
    outcome: Option<String>,
    citations: Option<cite::CitationStats>,
    review: Option<ReviewRecord>,
    impact: Option<crate::safety::impact::ImpactRecord>,
    /// The impact's caller list for the built-in reviewer.
    callers: Option<String>,
}

/// What a task actually did, shared by the verifier, nudges and the acceptance report.
#[derive(Default)]
pub struct Evidence {
    state: Mutex<EvidenceState>,
}

/// Tools that never change anything.
pub const READ_ONLY_TOOLS: &[&str] = &[
    "read",
    "glob",
    "grep",
    "repo_map",
    "web_fetch",
    "web_search",
    "skill",
    "todo",
    "ask_user",
    "plan_mode",
];

impl Evidence {
    fn with<T>(&self, f: impl FnOnce(&mut EvidenceState) -> T) -> Option<T> {
        self.state.lock().ok().map(|mut state| f(&mut state))
    }

    pub fn reset(&self) {
        self.with(|state| *state = EvidenceState::default());
    }

    pub fn passed(&self, command: &str) -> bool {
        self.with(|state| state.passed.contains(&normalize_command(command)))
            .unwrap_or(false)
    }

    /// A gate check passed; it stays valid until a later change, like a model-run check.
    pub fn record_pass(&self, command: &str) {
        self.with(|state| state.passed.insert(normalize_command(command)));
    }

    pub fn did_work(&self) -> bool {
        self.with(|state| state.did_work).unwrap_or(false)
    }

    pub fn record_check(&self, check: CheckRecord) {
        self.with(|state| {
            state.checks.retain(|c| c.command != check.command);
            state.checks.push(check);
        });
    }

    pub fn set_citations(&self, stats: cite::CitationStats) {
        self.with(|state| state.citations = Some(stats));
    }

    /// The citation check of this task, when one ran.
    pub fn citations(&self) -> Option<cite::CitationStats> {
        self.with(|state| state.citations.clone()).flatten()
    }

    pub fn set_review(&self, review: ReviewRecord) {
        self.with(|state| state.review = Some(review));
    }

    /// The end-of-task review of this task, when one ran.
    pub fn review(&self) -> Option<ReviewRecord> {
        self.with(|state| state.review.clone()).flatten()
    }

    /// The change impact of this task (0031): the record for the acceptance report and
    /// the caller list for the built-in reviewer.
    pub fn set_impact(&self, record: crate::safety::impact::ImpactRecord, callers: Option<String>) {
        self.with(|state| {
            state.impact = Some(record);
            state.callers = callers;
        });
    }

    pub fn impact(&self) -> Option<crate::safety::impact::ImpactRecord> {
        self.with(|state| state.impact.clone()).flatten()
    }

    pub fn callers(&self) -> Option<String> {
        self.with(|state| state.callers.clone()).flatten()
    }

    pub fn set_outcome(&self, outcome: &str) {
        self.with(|state| state.outcome = Some(outcome.into()));
    }

    pub fn snapshot(&self) -> (Vec<CheckRecord>, Vec<GitEvent>, Option<String>) {
        self.with(|state| {
            (
                state.checks.clone(),
                state.git.clone(),
                state.outcome.clone(),
            )
        })
        .unwrap_or_default()
    }

    /// Updates the evidence from one finished tool call.
    pub fn observe(&self, name: &str, input: &Value, output: &ToolOutput) {
        self.with(|state| {
            if !output.failed && !READ_ONLY_TOOLS.contains(&name) {
                state.did_work = true;
            }
            if name == "shell" {
                let command = normalize_command(
                    input
                        .get("command")
                        .and_then(Value::as_str)
                        .unwrap_or_default(),
                );
                if command.is_empty() {
                    return;
                }
                // A line that may have changed files invalidates earlier passes.
                // Formatters/autofixers are auto-approvable yet rewrite sources; local
                // git bookkeeping (add/commit/tag) never touches the working tree, so
                // it must not invalidate passing checks — or every commit in a row
                // re-runs the whole verification suite for nothing.
                let dialect = Dialect::for_command(&command);
                let may_change_files = !classify::auto_approvable(&command, dialect)
                    && !classify::local_git(&command, dialect)
                    && !classify::remote_git(&command, dialect);
                if may_change_files || classify::may_rewrite_sources(&command, dialect) {
                    state.passed.clear();
                    state.outcome = None;
                }
                let passed = !output.failed
                    && output
                        .content
                        .starts_with("exit_code=Some(0) timed_out=false");
                if passed {
                    state.passed.insert(command.clone());
                } else {
                    state.passed.remove(&command);
                }
                for action in git_actions(&command) {
                    state.git.push(GitEvent {
                        action: action.into(),
                        command: command.clone(),
                        succeeded: passed,
                    });
                }
                return;
            }
            if !READ_ONLY_TOOLS.contains(&name) {
                state.passed.clear();
                state.outcome = None;
            }
        });
    }
}

/// `commit`/`push` subcommands of git in a shell line.
pub fn git_actions(command: &str) -> Vec<&'static str> {
    let line = crate::safety::permission::shell::parse(command, Dialect::for_command(command));
    let mut out = Vec::new();
    for simple in line.commands() {
        let argv: Vec<String> = simple.argv().iter().map(|w| (*w).to_owned()).collect();
        let words = crate::safety::permission::shell::strip_runners(&argv);
        if words
            .first()
            .map(|w| crate::safety::permission::shell::program_name(w))
            .as_deref()
            != Some("git")
        {
            continue;
        }
        match words
            .iter()
            .skip(1)
            .find(|w| !w.starts_with('-'))
            .map(String::as_str)
        {
            Some("commit") => out.push("commit"),
            Some("push") => out.push("push"),
            _ => {}
        }
    }
    out
}

/// Feeds `Evidence` and resets it per task.
pub struct EvidenceHook(pub std::sync::Arc<Evidence>);

impl TurnHook for EvidenceHook {
    fn task_note<'a>(
        &'a self,
        _session: &'a crate::session::Session,
    ) -> futures_util::future::BoxFuture<'a, Option<String>> {
        self.0.reset();
        Box::pin(async { None })
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        self.0.observe(name, input, output);
        None
    }
}

/// What `app` needs from this area, built from the settings in one place.
pub struct Assembly {
    pub verifier: Option<std::sync::Arc<crate::safety::verify::CommandVerifier>>,
    /// The tools wrapped with the pre-commit gate (for write-mode sub-agents too).
    pub gated: std::sync::Arc<dyn crate::tools::ToolExecutor>,
    /// `gated` plus the plan-mode tools, for the main agent.
    pub tools: std::sync::Arc<dyn crate::tools::ToolExecutor>,
    pub workflow: std::sync::Arc<plan::Workflow>,
    pub hooks: Vec<std::sync::Arc<dyn TurnHook>>,
    /// The review before a commit, for `/commit` too; `None` when there is none
    /// (`commit_review` off, or a project review that does not run at commits).
    pub commit_review: Option<std::sync::Arc<review::CommitReview>>,
    /// The end-of-task review, run by the agent loop after the verification.
    pub review: Option<std::sync::Arc<review::DoneReview>>,
}

pub struct Parts {
    pub permissions: std::sync::Arc<crate::safety::permission::Permissions>,
    pub input: Option<std::sync::Arc<dyn crate::domain::UserInput>>,
    pub workspace: std::sync::Arc<crate::session::workspace::Workspace>,
    pub authorizer: std::sync::Arc<dyn crate::safety::permission::Authorizer>,
    pub progress: std::sync::Arc<crate::domain::Progress>,
    pub tools: std::sync::Arc<dyn crate::tools::ToolExecutor>,
    pub provider: std::sync::Arc<dyn crate::domain::Provider>,
    /// The fast model when set: the review before a commit runs on it.
    pub background: std::sync::Arc<dyn crate::domain::Provider>,
    /// Read-only tools for the review sub-agents.
    pub readonly: std::sync::Arc<dyn crate::tools::ToolExecutor>,
    pub ledger: std::sync::Arc<crate::engine::agent::Ledger>,
    pub project_context: Option<String>,
    pub trust: std::sync::Arc<dyn crate::safety::verify::ProjectTrust>,
    /// The project's rules, for the reviews.
    pub rules: Option<std::sync::Arc<crate::memory::rules::RulesFile>>,
}

fn verification_preapproved(
    mode: crate::safety::permission::Mode,
    trusted: bool,
    command: &str,
) -> bool {
    if trusted {
        return true;
    }
    match mode {
        crate::safety::permission::Mode::Auto | crate::safety::permission::Mode::Bypass => true,
        crate::safety::permission::Mode::Default => {
            let dialect = crate::safety::permission::shell::Dialect::for_command(command);
            crate::safety::permission::risk::reasons(command, dialect).is_empty()
        }
        crate::safety::permission::Mode::Plan => false,
    }
}

impl Assembly {
    pub fn new(settings: &crate::config::Settings, parts: Parts) -> Self {
        use std::sync::Arc;
        let config = GateConfig::from_json(&settings.file);
        let evidence = Arc::new(Evidence::default());
        let root = settings.cwd.clone();
        let detected = settings.file.get("done_verify_commands").is_none()
            && settings
                .file
                .get("done_verify_auto")
                .and_then(Value::as_bool)
                != Some(false);
        let tests = detected && config.verify_tests;
        let verifier = (!settings.verify_commands.is_empty() || tests).then(|| {
            let mut verifier = crate::safety::verify::CommandVerifier::new(
                root.clone(),
                settings.verify_commands.clone(),
                settings.verify_timeout,
                parts.workspace.clone(),
                parts.authorizer.clone(),
            )
            .with_progress(parts.progress.clone())
            .with_evidence(evidence.clone())
            .with_ledger(settings.data_dir.join("verify_ledger.jsonl"));
            if detected {
                let (permissions, trust, root) =
                    (parts.permissions.clone(), parts.trust.clone(), root.clone());
                verifier = verifier.with_detection(
                    tests,
                    Arc::new(move |command| {
                        verification_preapproved(permissions.mode(), trust.trusted(&root), command)
                    }),
                );
            }
            Arc::new(verifier)
        });
        let review_settings = || review::ReviewSettings {
            max_turns: settings.subagent_max_turns,
            max_output_tokens: settings.max_output_tokens,
            prices: settings.prices,
            context_tokens: settings.context_tokens,
            project_context: parts.project_context.clone(),
        };
        let review_config = review_command::ReviewConfig::from_json(&settings.file);
        let instructions = review_config
            .instructions
            .as_ref()
            .map(|path| root.join(path));
        let reviewer = |provider: &Arc<dyn crate::domain::Provider>| review::Reviewer {
            provider: provider.clone(),
            tools: parts.readonly.clone(),
            settings: review_settings(),
            ledger: parts.ledger.clone(),
            rules: parts.rules.clone(),
            instructions: instructions.clone(),
        };
        // One reviewer per review point: a project review command replaces both
        // built-in reviews (0028).
        let project = (!review_config.commands.is_empty()).then(|| {
            Arc::new(
                review_command::CommandReview::new(
                    root.clone(),
                    review_config.clone(),
                    parts.workspace.clone(),
                )
                .with_ledger(settings.data_dir.join("verify_ledger.jsonl"))
                .with_progress(parts.progress.clone()),
            )
        });
        let commit_review = match &project {
            Some(project) => review_config
                .at_commit
                .then(|| Arc::new(review::CommitReview::Project(project.clone()))),
            None => (config.commit_review != Switch::Off).then(|| {
                Arc::new(review::CommitReview::Builtin {
                    mode: config.commit_review,
                    reviewer: reviewer(&parts.background),
                })
            }),
        };
        let done_reviewer = match &project {
            Some(project) => review_config
                .at_done
                .then(|| review::DoneReviewer::Project(project.clone())),
            None => (config.self_review != Switch::Off).then(|| review::DoneReviewer::Builtin {
                mode: config.self_review,
                reviewer: reviewer(&parts.provider),
            }),
        };
        let done_review = done_reviewer.map(|reviewer| {
            Arc::new(review::DoneReview::new(
                reviewer,
                parts.workspace.clone(),
                evidence.clone(),
                Some(parts.progress.clone()),
            ))
        });
        // commit_gate=off leaves the end-of-task verification but no per-commit run.
        let commit_checks = verifier
            .clone()
            .filter(|_| config.commit_gate != Switch::Off);
        let gated: Arc<dyn crate::tools::ToolExecutor> =
            if commit_checks.is_some() || commit_review.is_some() {
                Arc::new(
                    commit::WithCommitGate::new(
                        parts.tools.clone(),
                        commit_checks,
                        commit_review.clone(),
                        root.clone(),
                    )
                    .with_progress(parts.progress.clone()),
                )
            } else {
                parts.tools.clone()
            };
        let workflow_mode = settings
            .file
            .get("workflow")
            .and_then(Value::as_str)
            .and_then(plan::WorkflowMode::parse)
            .unwrap_or(plan::WorkflowMode::Direct);
        let workflow = Arc::new(plan::Workflow::new(
            workflow_mode,
            parts.permissions.clone(),
            root.clone(),
            settings.data_dir.clone(),
        ));
        let tools: Arc<dyn crate::tools::ToolExecutor> = Arc::new(plan::WithPlanTools {
            inner: gated.clone(),
            workflow: workflow.clone(),
            input: parts.input.clone(),
        });
        let interactive = parts.input.is_some();
        let anthropic = matches!(settings.protocol, crate::config::Protocol::Anthropic);
        let mut hooks: Vec<Arc<dyn TurnHook>> = vec![
            Arc::new(EvidenceHook(evidence.clone())),
            workflow.clone(),
            Arc::new(guard::PolicyGuard {
                permissions: parts.permissions.clone(),
                root: root.clone(),
            }),
            Arc::new(guard::ToolGuard::default()),
            Arc::new(nudge::Nudges::new(
                nudge::NudgeLimits {
                    max_turns: settings.max_turns,
                    done_check: nudge::NudgeLimits::done_check(
                        config.done_check,
                        anthropic,
                        interactive,
                    ),
                    heuristics: interactive,
                },
                Some(parts.workspace.clone()),
                evidence.clone(),
            )),
            Arc::new(impact::ChangeImpact::new(
                config.change_impact,
                settings.max_turns,
                parts.workspace.clone(),
                evidence.clone(),
                Some(parts.progress.clone()),
            )),
            Arc::new(cite::Citations::new(
                config.cite_check != Switch::Off,
                settings.max_turns,
                parts.workspace.clone(),
                evidence.clone(),
                Some(parts.progress.clone()),
            )),
            Arc::new(acceptance::Acceptance::new(
                settings.data_dir.clone(),
                root,
                evidence,
            )),
        ];
        if let Some(review) = &done_review {
            hooks.push(review.clone());
        }
        if let Some(project) = project {
            hooks.push(Arc::new(review::ReviewGuard(project)));
        }
        Self {
            verifier,
            gated,
            tools,
            workflow,
            hooks,
            commit_review,
            review: done_review,
        }
    }
}

/// Text of the last assistant message.
pub fn last_reply(session: &crate::session::Session) -> String {
    session
        .messages
        .iter()
        .rev()
        .find(|message| message.role == crate::domain::Role::Assistant)
        .map(crate::model::oneshot::response_text)
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn out(content: &str, failed: bool) -> ToolOutput {
        ToolOutput {
            content: content.into(),
            failed,
            ..Default::default()
        }
    }

    #[test]
    fn default_verification_preapproval_matches_shell_build_policy() {
        use crate::safety::permission::Mode;
        assert!(verification_preapproved(
            Mode::Default,
            false,
            "cargo check"
        ));
        assert!(verification_preapproved(Mode::Default, false, "cargo test"));
        assert!(!verification_preapproved(Mode::Default, false, "cargo fmt"));
        assert!(!verification_preapproved(
            Mode::Default,
            false,
            "cargo install x"
        ));
        assert!(!verification_preapproved(Mode::Plan, false, "cargo check"));
        assert!(verification_preapproved(Mode::Default, true, "cargo fmt"));
    }

    #[test]
    fn passed_commands_are_invalidated_by_later_changes() {
        let evidence = Evidence::default();
        let ok = "exit_code=Some(0) timed_out=false truncated=false";
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"cargo  test"}),
            &out(ok, false),
        );
        assert!(evidence.passed("cargo test"));
        assert!(evidence.did_work());
        evidence.observe("read", &serde_json::json!({"path":"a"}), &out("", false));
        assert!(evidence.passed("cargo test"));
        evidence.observe(
            "edit",
            &serde_json::json!({"path":"a"}),
            &out("edited a", false),
        );
        assert!(!evidence.passed("cargo test"));
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"cargo test"}),
            &out(ok, false),
        );
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"echo x > a.txt"}),
            &out(ok, false),
        );
        assert!(!evidence.passed("cargo test"));
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"git commit -m x"}),
            &out(ok, false),
        );
        assert_eq!(evidence.snapshot().1[0].action, "commit");
        // Local git bookkeeping never touches the working tree: evidence survives
        // add/commit, so a row of commits does not re-run the checks each time.
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"cargo test"}),
            &out(ok, false),
        );
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"git add a.txt"}),
            &out(ok, false),
        );
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"git commit -m y"}),
            &out(ok, false),
        );
        assert!(evidence.passed("cargo test"));
        for command in [
            "git push -u origin main 2>&1 | tail -n 8",
            "git fetch origin",
        ] {
            evidence.observe(
                "shell",
                &serde_json::json!({"command":command}),
                &out(ok, false),
            );
            assert!(evidence.passed("cargo test"), "{command}");
        }
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"git pull"}),
            &out(ok, false),
        );
        assert!(!evidence.passed("cargo test"));
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"cargo test"}),
            &out(ok, false),
        );
        // A formatter rewrites sources: evidence must go.
        evidence.observe(
            "shell",
            &serde_json::json!({"command":"cargo fmt"}),
            &out(ok, false),
        );
        assert!(!evidence.passed("cargo test"));
    }

    #[test]
    fn switches_parse_config_values() {
        let config = GateConfig::from_json(
            &serde_json::json!({"done_check":"off","done_self_review":"auto","done_verify_tests":false}),
        );
        assert_eq!(config.done_check, Switch::Off);
        assert_eq!(config.self_review, Switch::Auto);
        assert!(!config.verify_tests);
        let config = GateConfig::from_json(&serde_json::json!({}));
        assert_eq!(config.done_check, Switch::Auto);
        assert_eq!(config.self_review, Switch::Off);
        assert!(config.verify_tests);
        assert_eq!(config.cite_check, Switch::Auto);
        assert_eq!(config.change_impact, Switch::Auto);
        let config = GateConfig::from_json(&serde_json::json!({"cite_check":"off"}));
        assert_eq!(config.cite_check, Switch::Off);
    }
}
