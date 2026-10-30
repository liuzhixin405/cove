//! Completion gates and task guards, implemented as `TurnHook`s and tool wrappers:
//! guardrail and loop detection, finish nudges, self-review, plan mode and the
//! review workflow, the pre-commit gate and the acceptance report.

pub mod acceptance;
pub mod commit;
pub mod guard;
pub mod nudge;
pub mod plan;
pub mod review;

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

#[derive(Default)]
struct EvidenceState {
    /// Shell lines that succeeded after the last change that could affect them.
    passed: HashSet<String>,
    /// A non-read-only tool succeeded this task.
    did_work: bool,
    checks: Vec<CheckRecord>,
    git: Vec<GitEvent>,
    outcome: Option<String>,
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

    pub fn did_work(&self) -> bool {
        self.with(|state| state.did_work).unwrap_or(false)
    }

    pub fn record_check(&self, check: CheckRecord) {
        self.with(|state| {
            state.checks.retain(|c| c.command != check.command);
            state.checks.push(check);
        });
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
                if !classify::auto_approvable(&command, Dialect::host()) {
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
    let line = crate::safety::permission::shell::parse(command, Dialect::host());
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
}

pub struct Parts {
    pub permissions: std::sync::Arc<crate::safety::permission::Permissions>,
    pub input: Option<std::sync::Arc<dyn crate::domain::UserInput>>,
    pub workspace: std::sync::Arc<crate::session::workspace::Workspace>,
    pub authorizer: std::sync::Arc<dyn crate::safety::permission::Authorizer>,
    pub progress: std::sync::Arc<crate::domain::Progress>,
    pub tools: std::sync::Arc<dyn crate::tools::ToolExecutor>,
    pub provider: std::sync::Arc<dyn crate::domain::Provider>,
    /// Read-only tools for the self-review sub-agent.
    pub readonly: std::sync::Arc<dyn crate::tools::ToolExecutor>,
    pub ledger: std::sync::Arc<crate::engine::agent::Ledger>,
    pub project_context: Option<String>,
    pub trust: std::sync::Arc<dyn crate::safety::verify::ProjectTrust>,
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
                    Arc::new(move || {
                        trust.trusted(&root)
                            || matches!(
                                permissions.mode(),
                                crate::safety::permission::Mode::Auto
                                    | crate::safety::permission::Mode::Bypass
                            )
                    }),
                );
            }
            Arc::new(verifier)
        });
        let gated: Arc<dyn crate::tools::ToolExecutor> = match &verifier {
            Some(verifier) => Arc::new(commit::WithCommitGate::new(
                parts.tools.clone(),
                verifier.clone(),
                root.clone(),
            )),
            None => parts.tools.clone(),
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
        let hooks: Vec<Arc<dyn TurnHook>> = vec![
            Arc::new(EvidenceHook(evidence.clone())),
            workflow.clone(),
            Arc::new(guard::PolicyGuard {
                permissions: parts.permissions.clone(),
                root: root.clone(),
            }),
            Arc::new(guard::ToolGuard::new(parts.input.clone())),
            Arc::new(nudge::Nudges::new(
                nudge::NudgeLimits {
                    max_turns: settings.max_turns,
                    budget: settings.budget,
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
            Arc::new(review::SelfReview::new(
                config.self_review,
                parts.workspace.clone(),
                parts.provider,
                parts.readonly,
                review::ReviewSettings {
                    max_turns: settings.subagent_max_turns,
                    max_output_tokens: settings.max_output_tokens,
                    prices: settings.prices,
                    context_tokens: settings.context_tokens,
                    project_context: parts.project_context,
                },
                parts.ledger,
                Some(parts.progress),
            )),
            Arc::new(acceptance::Acceptance::new(
                settings.data_dir.clone(),
                root,
                evidence,
            )),
        ];
        Self {
            verifier,
            gated,
            tools,
            workflow,
            hooks,
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
        .map(|message| {
            message
                .blocks
                .iter()
                .filter_map(|block| match block {
                    crate::domain::Block::Text(text) => Some(text.as_str()),
                    _ => None,
                })
                .collect::<String>()
        })
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
    }
}
