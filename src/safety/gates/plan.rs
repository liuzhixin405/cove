//! `plan_mode` / `exit_plan_mode` tools (interactive only) and the review/once workflow.
//!
//! In plan mode only read-only tools run. The model may enter plan mode itself and leave
//! it by submitting a plan the user approves; a plan mode the user selected can only be
//! left by the user (`/mode`). Workflow `review` starts every task in plan mode, `once`
//! only the next one. The files a plan lists are snapshotted when it is submitted; if
//! they change while the user decides, the approval is void.

use std::{
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex},
};

use futures_util::future::BoxFuture;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use super::tool_output;
use crate::{
    domain::{Result, ToolDefinition, UserInput},
    engine::turn::TurnHook,
    safety::permission::{Mode, Permissions, PlanOrigin, classify, shell::Dialect},
    session::Session,
    tools::{ToolExecutor, ToolOutput},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum WorkflowMode {
    Direct,
    Review,
    Once,
}

impl WorkflowMode {
    pub fn parse(raw: &str) -> Option<Self> {
        Some(match raw.trim().to_ascii_lowercase().as_str() {
            "direct" => Self::Direct,
            "review" => Self::Review,
            "once" => Self::Once,
            _ => return None,
        })
    }

    pub fn name(self) -> &'static str {
        match self {
            Self::Direct => "direct",
            Self::Review => "review",
            Self::Once => "once",
        }
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct Plan {
    pub summary: String,
    #[serde(default)]
    pub files: Vec<String>,
    #[serde(default)]
    pub decisions: Vec<String>,
    #[serde(default)]
    pub checks: Vec<String>,
    /// approved, rejected or void
    #[serde(default)]
    pub decision: String,
    #[serde(default)]
    pub feedback: String,
    #[serde(default)]
    pub session_id: String,
    #[serde(default)]
    pub request: String,
    #[serde(default)]
    pub created_at: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PlanArgs {
    summary: String,
    #[serde(default)]
    files: Vec<String>,
    #[serde(default)]
    decisions: Vec<String>,
    #[serde(default)]
    checks: Vec<String>,
}

struct State {
    mode: WorkflowMode,
    /// This task's plan mode is held by the workflow.
    active: bool,
    session_id: String,
    request: String,
    last_plan: Option<Plan>,
}

pub struct Workflow {
    state: Mutex<State>,
    pub permissions: Arc<Permissions>,
    root: PathBuf,
    data_dir: PathBuf,
}

/// A plan file and its content when the plan was submitted (`None`: missing).
type Snapshot = (String, Option<Vec<u8>>);

const MUTATING_ACTIONS: &[&str] = &["add", "update", "delete", "call"];

impl Workflow {
    pub fn new(
        mode: WorkflowMode,
        permissions: Arc<Permissions>,
        root: PathBuf,
        data_dir: PathBuf,
    ) -> Self {
        Self {
            state: Mutex::new(State {
                mode,
                active: false,
                session_id: String::new(),
                request: String::new(),
                last_plan: None,
            }),
            permissions,
            root,
            data_dir,
        }
    }

    pub fn mode(&self) -> WorkflowMode {
        self.state
            .lock()
            .map_or(WorkflowMode::Direct, |state| state.mode)
    }

    pub fn set_mode(&self, mode: WorkflowMode) {
        if let Ok(mut state) = self.state.lock() {
            state.mode = mode;
            if mode == WorkflowMode::Direct && state.active {
                state.active = false;
                let _ = self.permissions.exit_plan(PlanOrigin::Workflow);
            }
        }
    }

    pub fn last_plan(&self) -> Option<Plan> {
        self.state
            .lock()
            .ok()
            .and_then(|state| state.last_plan.clone())
    }

    fn plan_path(&self, session_id: &str) -> PathBuf {
        self.data_dir
            .join("plans")
            .join(format!("{session_id}.json"))
    }

    fn save(&self, plan: &Plan) {
        let path = self.plan_path(if plan.session_id.is_empty() {
            "unknown"
        } else {
            &plan.session_id
        });
        if let Some(parent) = path.parent() {
            let _ = std::fs::create_dir_all(parent);
        }
        let _ = crate::session::atomic_json(&path, plan);
        if let Ok(mut state) = self.state.lock() {
            state.last_plan = Some(plan.clone());
        }
    }

    fn snapshot(&self, files: &[String]) -> std::result::Result<Vec<Snapshot>, String> {
        files
            .iter()
            .map(|file| {
                let path = Path::new(file);
                if path.is_absolute()
                    || path
                        .components()
                        .any(|c| !matches!(c, Component::Normal(_) | Component::CurDir))
                {
                    return Err(format!(
                        "plan files must be workspace-relative paths: {file}"
                    ));
                }
                Ok((file.clone(), std::fs::read(self.root.join(path)).ok()))
            })
            .collect()
    }

    async fn submit(
        &self,
        args: PlanArgs,
        input: &Arc<dyn UserInput>,
        cancel: CancellationToken,
    ) -> Result<ToolOutput> {
        if self.permissions.mode() != Mode::Plan {
            return Ok(tool_output(
                "not in plan mode; continue with the task".into(),
                true,
            ));
        }
        let origin = self.permissions.plan_origin().unwrap_or(PlanOrigin::Model);
        if origin == PlanOrigin::User {
            return Ok(tool_output(
                "the user selected plan mode; only the user can leave it (/mode). Present the plan as text and stop.".into(),
                true,
            ));
        }
        if args.summary.trim().is_empty() || args.files.len() > 200 {
            return Ok(tool_output(
                "exit_plan_mode needs a nonempty summary and at most 200 files".into(),
                true,
            ));
        }
        let before = match self.snapshot(&args.files) {
            Ok(snapshot) => snapshot,
            Err(err) => return Ok(tool_output(err, true)),
        };
        let list = |title: &str, items: &[String]| {
            if items.is_empty() {
                String::new()
            } else {
                format!(
                    "{title}:\n{}\n",
                    items
                        .iter()
                        .map(|item| format!("  - {item}"))
                        .collect::<Vec<_>>()
                        .join("\n")
                )
            }
        };
        let question = format!(
            "\n[待批准的计划]\n{}\n{}{}{}批准并退出计划模式？输入反馈可让它修改 [y/N, or type feedback] ",
            args.summary.trim(),
            list("文件", &args.files),
            list("决策", &args.decisions),
            list("检查", &args.checks)
        );
        let answer = input.answer(question, cancel).await?;
        let (session_id, request) = self
            .state
            .lock()
            .map(|state| (state.session_id.clone(), state.request.clone()))
            .unwrap_or_default();
        let mut plan = Plan {
            summary: args.summary,
            files: args.files,
            decisions: args.decisions,
            checks: args.checks,
            session_id,
            request,
            created_at: crate::session::now(),
            ..Plan::default()
        };
        let after = self.snapshot(&plan.files).unwrap_or_default();
        if after != before {
            let changed: Vec<&str> = before
                .iter()
                .zip(&after)
                .filter(|(a, b)| a.1 != b.1)
                .map(|(a, _)| a.0.as_str())
                .collect();
            plan.decision = "void".into();
            self.save(&plan);
            return Ok(tool_output(
                format!(
                    "Plan void: files changed while the user was deciding ({}). Re-inspect them and submit the plan again.",
                    changed.join(", ")
                ),
                false,
            ));
        }
        let answer = answer.trim();
        if answer.eq_ignore_ascii_case("y") || answer.eq_ignore_ascii_case("yes") {
            let restored = match self.permissions.exit_plan(origin) {
                Ok(mode) => mode,
                Err(err) => return Ok(tool_output(err, true)),
            };
            if let Ok(mut state) = self.state.lock() {
                state.active = false;
            }
            plan.decision = "approved".into();
            self.save(&plan);
            return Ok(tool_output(
                format!(
                    "The user approved the plan; permission mode is now {}. Implement it, then run the checks you listed.",
                    restored.name()
                ),
                false,
            ));
        }
        plan.decision = "rejected".into();
        plan.feedback = answer.to_owned();
        self.save(&plan);
        Ok(tool_output(
            if answer.is_empty()
                || answer.eq_ignore_ascii_case("n")
                || answer.eq_ignore_ascii_case("no")
            {
                "The user did not approve the plan. Stay in plan mode; ask what to change or revise the plan.".into()
            } else {
                format!(
                    "The user did not approve the plan and said: {answer}\nStay in plan mode, revise the plan and submit it again."
                )
            },
            false,
        ))
    }
}

/// Whether a tool call would change something; plan mode refuses these with guidance
/// instead of an approval error that would stop the task.
pub fn mutating(name: &str, input: &Value) -> bool {
    match name {
        "write" | "edit" => true,
        "shell" => !classify::read_only(
            input
                .get("command")
                .and_then(Value::as_str)
                .unwrap_or_default(),
            Dialect::host(),
        ),
        "agent" => input.get("mode").and_then(Value::as_str) == Some("write"),
        "memory" | "mcp" => input
            .get("action")
            .and_then(Value::as_str)
            .is_some_and(|action| MUTATING_ACTIONS.contains(&action)),
        _ => false,
    }
}

impl TurnHook for Workflow {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        let note = (|| {
            let mut state = self.state.lock().ok()?;
            state.session_id = session.id.clone();
            state.request = session.current_request.clone();
            if state.mode == WorkflowMode::Direct || !self.permissions.interactive() {
                return None;
            }
            if state.mode == WorkflowMode::Once {
                state.mode = WorkflowMode::Direct;
            }
            if !self.permissions.enter_plan(PlanOrigin::Workflow) {
                return None;
            }
            state.active = true;
            Some("[review workflow] This task starts in plan mode: investigate with read-only tools, then call exit_plan_mode with summary, files, decisions and checks. Make no changes until the user approves the plan.".to_owned())
        })();
        Box::pin(async move { note })
    }

    fn before_tool(&self, name: &str, input: &Value) -> Option<String> {
        if self.permissions.mode() != Mode::Plan || !mutating(name, input) {
            return None;
        }
        Some(match self.permissions.plan_origin() {
            Some(PlanOrigin::User) => format!(
                "plan mode (selected by the user): {name} is not allowed; only read operations run. Present your plan as text; the user leaves plan mode with /mode."
            ),
            _ if self.permissions.interactive() => format!(
                "plan mode: {name} is not allowed yet. Finish investigating, then submit your plan with exit_plan_mode for the user's approval."
            ),
            _ => format!("plan mode: {name} is not allowed; only read operations run."),
        })
    }

    fn task_end<'a>(&'a self, _session: &'a Session, _completed: bool) -> BoxFuture<'a, ()> {
        if let Ok(mut state) = self.state.lock()
            && state.active
        {
            state.active = false;
            let _ = self.permissions.exit_plan(PlanOrigin::Workflow);
        }
        Box::pin(async {})
    }
}

/// Adds `plan_mode` and `exit_plan_mode` to `inner` when a user can approve plans.
pub struct WithPlanTools {
    pub inner: Arc<dyn ToolExecutor>,
    pub workflow: Arc<Workflow>,
    pub input: Option<Arc<dyn UserInput>>,
}

impl ToolExecutor for WithPlanTools {
    fn definitions(&self) -> Vec<ToolDefinition> {
        let mut definitions = self.inner.definitions();
        if self.input.is_some() {
            definitions.push(ToolDefinition {
                name: "plan_mode".into(),
                description: "Enter plan mode before a complex or risky change: only read operations run until the user approves your plan (exit_plan_mode).".into(),
                schema: json!({"type":"object","properties":{"reason":{"type":"string"}},"additionalProperties":false}),
            });
            definitions.push(ToolDefinition {
                name: "exit_plan_mode".into(),
                description: "Submit your implementation plan for the user's approval and leave plan mode when approved. files: workspace-relative paths you will change; checks: commands that will verify it.".into(),
                schema: json!({"type":"object","properties":{
                    "summary":{"type":"string"},
                    "files":{"type":"array","items":{"type":"string"}},
                    "decisions":{"type":"array","items":{"type":"string"}},
                    "checks":{"type":"array","items":{"type":"string"}}
                },"required":["summary"],"additionalProperties":false}),
            });
        }
        definitions
    }

    fn metered(&self, name: &str) -> bool {
        self.inner.metered(name)
    }

    fn concurrent(&self, name: &str) -> bool {
        self.inner.concurrent(name)
    }

    fn execute(
        &self,
        name: String,
        input: Value,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            let Some(user) = self
                .input
                .as_ref()
                .filter(|_| matches!(name.as_str(), "plan_mode" | "exit_plan_mode"))
            else {
                return self.inner.execute(name, input, cancel).await;
            };
            if name == "plan_mode" {
                let reason = input
                    .get("reason")
                    .and_then(Value::as_str)
                    .unwrap_or_default();
                return Ok(if self.workflow.permissions.enter_plan(PlanOrigin::Model) {
                    tool_output(
                        format!(
                            "Plan mode active: read-only operations only. Reason: {reason}\nWhen ready, submit the plan with exit_plan_mode."
                        ),
                        false,
                    )
                } else {
                    tool_output("Already in plan mode.".into(), false)
                });
            }
            match serde_json::from_value::<PlanArgs>(input) {
                Ok(args) => self.workflow.submit(args, user, cancel).await,
                Err(err) => Ok(tool_output(err.to_string(), true)),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Answer(&'static str, Option<PathBuf>);

    impl UserInput for Answer {
        fn answer(
            &self,
            _prompt: String,
            _cancel: CancellationToken,
        ) -> BoxFuture<'_, Result<String>> {
            if let Some(path) = &self.1 {
                std::fs::write(path, "changed").unwrap();
            }
            Box::pin(async move { Ok(self.0.to_owned()) })
        }
    }

    fn setup(answer: Answer) -> (tempfile::TempDir, Arc<Workflow>, WithPlanTools) {
        let dir = tempfile::tempdir().unwrap();
        let input: Arc<dyn UserInput> = Arc::new(answer);
        let permissions = Arc::new(Permissions::new(
            Mode::Auto,
            dir.path().into(),
            vec![dir.path().into()],
            Some(input.clone()),
        ));
        let workflow = Arc::new(Workflow::new(
            WorkflowMode::Direct,
            permissions,
            dir.path().into(),
            dir.path().join("data"),
        ));
        let tools = WithPlanTools {
            inner: Arc::new(
                crate::tools::LocalTools::new(dir.path().into(), workflow.permissions.clone())
                    .unwrap(),
            ),
            workflow: workflow.clone(),
            input: Some(input),
        };
        (dir, workflow, tools)
    }

    #[tokio::test]
    async fn model_plan_mode_needs_approval_to_leave() {
        let (_dir, workflow, tools) = setup(Answer("y", None));
        let run =
            |name: &str, input: Value| tools.execute(name.into(), input, CancellationToken::new());
        run("plan_mode", json!({"reason":"big change"}))
            .await
            .unwrap();
        assert_eq!(workflow.permissions.mode(), Mode::Plan);
        assert!(
            workflow
                .before_tool("write", &json!({"path":"a","content":"x"}))
                .is_some()
        );
        assert!(workflow.before_tool("read", &json!({"path":"a"})).is_none());
        let out = run(
            "exit_plan_mode",
            json!({"summary":"do it","files":["a.txt"]}),
        )
        .await
        .unwrap();
        assert!(out.content.contains("approved"), "{}", out.content);
        assert_eq!(workflow.permissions.mode(), Mode::Auto);
        assert_eq!(workflow.last_plan().unwrap().decision, "approved");
    }

    #[tokio::test]
    async fn user_plan_mode_and_changed_files() {
        let (dir, workflow, tools) = setup(Answer("y", None));
        workflow.permissions.set_mode(Mode::Plan);
        let out = tools
            .execute(
                "exit_plan_mode".into(),
                json!({"summary":"x"}),
                CancellationToken::new(),
            )
            .await
            .unwrap();
        assert!(out.failed && out.content.contains("only the user"));
        let (_dir2, workflow, tools) = setup(Answer("y", Some(dir.path().join("watched.txt"))));
        let _ = dir;
        workflow.permissions.enter_plan(PlanOrigin::Model);
        let path = _dir2.path().join("watched.txt");
        std::fs::write(&path, "before").unwrap();
        let tools = WithPlanTools {
            input: Some(Arc::new(Answer("y", Some(path)))),
            ..tools
        };
        let out = tools
            .execute(
                "exit_plan_mode".into(),
                json!({"summary":"x","files":["watched.txt"]}),
                CancellationToken::new(),
            )
            .await
            .unwrap();
        assert!(out.content.contains("void"), "{}", out.content);
        assert_eq!(workflow.permissions.mode(), Mode::Plan);
    }

    #[tokio::test]
    async fn review_workflow_starts_tasks_in_plan_mode() {
        let (_dir, workflow, _tools) = setup(Answer("n", None));
        workflow.set_mode(WorkflowMode::Once);
        let session = Session::new("/".into(), String::new(), String::new(), None);
        assert!(workflow.task_note(&session).await.is_some());
        assert_eq!(workflow.permissions.mode(), Mode::Plan);
        workflow.task_end(&session, true).await;
        assert_eq!(workflow.permissions.mode(), Mode::Auto);
        assert_eq!(workflow.mode(), WorkflowMode::Direct);
        assert!(workflow.task_note(&session).await.is_none());
    }
}
