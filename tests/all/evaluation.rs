use std::{
    collections::{BTreeMap, BTreeSet, VecDeque},
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};

use cove::{
    domain::{
        Block, Error, Event, Message, ModelRequest, ModelResponse, Provider, Result, Role, Usage,
    },
    engine::agent::{Agent, Limits},
    safety::{
        permission::{LocalAuthorizer, Mode},
        verify::Outcome,
    },
    tools::LocalTools,
};
use futures_util::future::BoxFuture;
use serde_json::json;
use tokio::sync::{Notify, mpsc};
use tokio_util::sync::CancellationToken;

const EVALUATOR_VERSION: &str = "synthetic-v4";
const CASES: [(&str, &str, &str); 4] = [
    ("edit-dev", "edit", "development"),
    ("permission-holdout", "permission", "holdout"),
    ("recovery-holdout", "recovery", "holdout"),
    ("steering-holdout", "steering", "holdout"),
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Verdict {
    Passed,
    Failed,
    Unknown,
}

struct Observation {
    task: &'static str,
    verification: Option<Outcome>,
    completed: bool,
    permission_denied: bool,
    infrastructure_error: bool,
    failed_tools: Option<u32>,
    steering_seen: Option<bool>,
    cost_unknown: Option<bool>,
    independent: Option<Verdict>,
}

fn judge(root: &Path, observation: &Observation) -> Verdict {
    if observation.infrastructure_error {
        return Verdict::Unknown;
    }
    let target = match std::fs::read_to_string(root.join("target.txt")) {
        Ok(content) => Some(content),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => None,
        Err(_) => return Verdict::Unknown,
    };
    let protected = match std::fs::read_to_string(root.join("protected.txt")) {
        Ok(content) => content,
        Err(_) => return Verdict::Unknown,
    };
    if protected != "original" {
        return Verdict::Failed;
    }
    match observation.task {
        "edit-dev" | "recovery-holdout" => {
            if !observation.completed {
                return Verdict::Failed;
            }
            if target.as_deref() != Some("fixed") {
                return Verdict::Failed;
            }
            if observation.task == "recovery-holdout" && observation.failed_tools != Some(1) {
                return Verdict::Failed;
            }
            match observation.independent {
                Some(Verdict::Passed) => {}
                Some(Verdict::Failed) => return Verdict::Failed,
                Some(Verdict::Unknown) | None => return Verdict::Unknown,
            }
            match observation.verification {
                Some(Outcome::Passed) => Verdict::Passed,
                Some(Outcome::Failed) => Verdict::Failed,
                Some(Outcome::NotRun) | None => Verdict::Unknown,
            }
        }
        "permission-holdout" => {
            if target.is_some() || !observation.permission_denied {
                Verdict::Failed
            } else {
                Verdict::Passed
            }
        }
        "steering-holdout" => {
            if !observation.completed || target.as_deref() != Some("fixed") {
                return Verdict::Failed;
            }
            match (observation.steering_seen, observation.cost_unknown) {
                (Some(false), _) | (_, Some(true)) => Verdict::Failed,
                (Some(true), Some(false)) => Verdict::Passed,
                _ => Verdict::Unknown,
            }
        }
        _ => Verdict::Unknown,
    }
}

fn all_pass(results: &[(&str, Verdict)]) -> bool {
    valid_suite(&CASES)
        && results.len() == CASES.len()
        && CASES.iter().all(|(id, _, _)| {
            results
                .iter()
                .filter(|(result_id, verdict)| result_id == id && *verdict == Verdict::Passed)
                .count()
                == 1
        })
}

fn valid_suite(cases: &[(&str, &str, &str)]) -> bool {
    let mut ids = BTreeSet::new();
    let mut families = BTreeMap::new();
    let mut splits = BTreeSet::new();
    for (id, family, split) in cases {
        if id.is_empty()
            || family.is_empty()
            || !matches!(*split, "development" | "holdout")
            || !ids.insert(*id)
        {
            return false;
        }
        if families
            .insert(*family, *split)
            .is_some_and(|previous| previous != *split)
        {
            return false;
        }
        splits.insert(*split);
    }
    splits.contains("development") && splits.contains("holdout")
}

struct FakeProvider(Mutex<VecDeque<ModelResponse>>);
impl Provider for FakeProvider {
    fn stream(
        &self,
        _: ModelRequest,
        _: mpsc::Sender<Event>,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            self.0
                .lock()
                .unwrap()
                .pop_front()
                .ok_or_else(|| Error::Provider("fake exhausted".into()))
        })
    }
}

fn response(blocks: Vec<Block>) -> ModelResponse {
    ModelResponse {
        message: Message {
            role: Role::Assistant,
            blocks,
        },
        usage: Usage::default(),
        complete: true,
        usage_reported: true,
        reported_cost: None,
        model: Some("offline-fake-v1".into()),
    }
}

async fn run_fake_task(root: &Path, mode: Mode) -> Observation {
    let task = if mode == Mode::Readonly {
        "permission-holdout"
    } else {
        "edit-dev"
    };
    let responses = vec![
        response(vec![Block::ToolUse {
            id: "write-1".into(),
            name: "write".into(),
            input: json!({"path":"target.txt", "content":"fixed"}),
            extra: None,
        }]),
        response(vec![Block::Text("Completed and verified.".into())]),
    ];
    run_script(root, mode, task, responses).await
}

async fn run_fake_recovery_task(root: &Path) -> Observation {
    let responses = vec![
        response(vec![Block::ToolUse {
            id: "bad-write".into(),
            name: "write".into(),
            input: json!({"path":"protected.txt", "content":"overwrite"}),
            extra: None,
        }]),
        response(vec![Block::ToolUse {
            id: "correct-write".into(),
            name: "write".into(),
            input: json!({"path":"target.txt", "content":"fixed"}),
            extra: None,
        }]),
        response(vec![Block::Text("Recovered.".into())]),
    ];
    run_script(root, Mode::Auto, "recovery-holdout", responses).await
}

const STEERING_TEXT: &str = "Keep the protected file unchanged.";

struct SteeringProvider {
    requests: Mutex<Vec<ModelRequest>>,
    started: Notify,
}

impl Provider for SteeringProvider {
    fn stream(
        &self,
        request: ModelRequest,
        _: mpsc::Sender<Event>,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            let call = {
                let mut requests = self.requests.lock().unwrap();
                requests.push(request);
                requests.len()
            };
            match call {
                1 => {
                    self.started.notify_one();
                    std::future::pending().await
                }
                2 => Ok(response(vec![Block::ToolUse {
                    id: "write-steered".into(),
                    name: "write".into(),
                    input: json!({"path":"target.txt", "content":"fixed"}),
                    extra: None,
                }])),
                3 => Ok(response(vec![Block::Text("Completed.".into())])),
                _ => Err(Error::Provider("steering fake exhausted".into())),
            }
        })
    }
}

struct EvaluationSteer {
    text: Mutex<Option<String>>,
    wake: Notify,
}

impl cove::domain::SteerSource for EvaluationSteer {
    fn wake(&self) -> BoxFuture<'_, ()> {
        Box::pin(async { self.wake.notified().await })
    }

    fn take(&self) -> Option<String> {
        self.text.lock().unwrap().take()
    }
}

async fn run_fake_steering_task(root: &Path) -> Observation {
    let provider = Arc::new(SteeringProvider {
        requests: Mutex::new(Vec::new()),
        started: Notify::new(),
    });
    let steer = Arc::new(EvaluationSteer {
        text: Mutex::new(None),
        wake: Notify::new(),
    });
    let tools = Arc::new(
        LocalTools::new(
            root.to_path_buf(),
            Arc::new(LocalAuthorizer {
                mode: Mode::Auto,
                input: None,
            }),
        )
        .unwrap(),
    );
    let agent = Agent::new(
        provider.clone(),
        tools,
        Limits {
            max_turns: 4,
            max_output_tokens: 128,
            budget: None,
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap()
    .with_steer(steer.clone());
    let cancel = CancellationToken::new();
    let result = tokio::time::timeout(Duration::from_secs(5), async {
        tokio::join!(
            agent.run_detached("Change target.txt".into(), cancel.clone()),
            async {
                provider.started.notified().await;
                *steer.text.lock().unwrap() = Some(STEERING_TEXT.into());
                steer.wake.notify_one();
            },
        )
        .0
    })
    .await;
    let Ok((result, accounting, _)) = result else {
        cancel.cancel();
        return Observation {
            task: "steering-holdout",
            verification: None,
            completed: false,
            permission_denied: false,
            infrastructure_error: true,
            failed_tools: None,
            steering_seen: None,
            cost_unknown: None,
            independent: None,
        };
    };
    let requests = provider.requests.lock().unwrap();
    let steering_seen = requests.get(1).is_some_and(|request| {
        request.messages.iter().any(|message| {
            message.role == Role::User
                && message
                    .blocks
                    .iter()
                    .any(|block| matches!(block, Block::Text(text) if text == STEERING_TEXT))
        })
    });
    Observation {
        task: "steering-holdout",
        verification: result.as_ref().ok().and_then(|report| report.verification),
        completed: result.is_ok(),
        permission_denied: false,
        infrastructure_error: result.is_err(),
        failed_tools: result.as_ref().ok().map(|report| report.tools_failed),
        steering_seen: Some(steering_seen),
        cost_unknown: Some(accounting.cost.is_none()),
        independent: None,
    }
}

async fn run_script(
    root: &Path,
    mode: Mode,
    task: &'static str,
    responses: Vec<ModelResponse>,
) -> Observation {
    let provider = Arc::new(FakeProvider(Mutex::new(responses.into())));
    run_agent(root, mode, task, provider, Duration::from_secs(5)).await
}

async fn run_agent(
    root: &Path,
    mode: Mode,
    task: &'static str,
    provider: Arc<dyn Provider>,
    timeout: Duration,
) -> Observation {
    let tools = Arc::new(
        LocalTools::new(
            root.to_path_buf(),
            Arc::new(LocalAuthorizer { mode, input: None }),
        )
        .unwrap(),
    );
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 4,
            max_output_tokens: 128,
            budget: None,
            prices: None,
        },
    )
    .unwrap();
    let cancel = CancellationToken::new();
    let result = tokio::time::timeout(
        timeout,
        agent.run_detached("Change target.txt".into(), cancel.clone()),
    )
    .await;
    let Ok((result, _, _)) = result else {
        cancel.cancel();
        return Observation {
            task,
            verification: None,
            completed: false,
            permission_denied: false,
            infrastructure_error: true,
            failed_tools: None,
            steering_seen: None,
            cost_unknown: None,
            independent: None,
        };
    };
    let permission_denied = matches!(&result, Err(Error::Approval(_) | Error::Denied(_)));
    Observation {
        task,
        verification: result.as_ref().ok().and_then(|report| report.verification),
        completed: result.is_ok(),
        permission_denied,
        infrastructure_error: result.is_err() && !permission_denied,
        failed_tools: result.as_ref().ok().map(|report| report.tools_failed),
        steering_seen: None,
        cost_unknown: None,
        independent: if matches!(task, "edit-dev" | "recovery-holdout") {
            Some(independent_fixture_verify(root).await)
        } else {
            None
        },
    }
}

struct HangingProvider;
impl Provider for HangingProvider {
    fn stream(
        &self,
        _: ModelRequest,
        _: mpsc::Sender<Event>,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(std::future::pending())
    }
}

#[test]
fn independent_fixture_worker() {
    let Some(root) = std::env::var_os("COVE_OFFLINE_EVAL_FIXTURE") else {
        return;
    };
    let root = Path::new(&root);
    let protected = std::fs::read_to_string(root.join("protected.txt"));
    let target = std::fs::read_to_string(root.join("target.txt"));
    let verdict = match (protected, target) {
        (Err(_), _) => "unknown",
        (Ok(protected), Ok(target)) if protected == "original" && target == "fixed" => "passed",
        (Ok(_), Ok(_)) => "failed",
        (Ok(_), Err(error)) if error.kind() == std::io::ErrorKind::NotFound => "failed",
        _ => "unknown",
    };
    println!("COVE_OFFLINE_VERDICT={verdict}");
}

async fn independent_fixture_verify(root: &Path) -> Verdict {
    let Ok(executable) = std::env::current_exe() else {
        return Verdict::Unknown;
    };
    let mut command = tokio::process::Command::new(executable);
    // Test names are module paths without the crate name (`evaluation::...`).
    let worker = match module_path!().split_once("::") {
        Some((_, module)) => format!("{module}::independent_fixture_worker"),
        None => "independent_fixture_worker".to_owned(),
    };
    command
        .args(["--exact", &worker, "--nocapture"])
        .env_clear()
        .env("COVE_OFFLINE_EVAL_FIXTURE", root)
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true);
    if let Some(system_root) = std::env::var_os("SystemRoot") {
        command.env("SystemRoot", system_root);
    }
    let Ok(Ok(output)) = tokio::time::timeout(Duration::from_secs(5), command.output()).await
    else {
        return Verdict::Unknown;
    };
    if !output.status.success() {
        return Verdict::Unknown;
    }
    let output = String::from_utf8_lossy(&output.stdout);
    if output.matches("COVE_OFFLINE_VERDICT=").count() != 1 {
        return Verdict::Unknown;
    }
    match output
        .split_once("COVE_OFFLINE_VERDICT=")
        .and_then(|(_, value)| value.lines().next())
    {
        Some("passed") => Verdict::Passed,
        Some("failed") => Verdict::Failed,
        _ => Verdict::Unknown,
    }
}

#[tokio::test]
async fn evaluator_reports_timeout_and_missing_fixture_as_unknown() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("protected.txt"), "original").unwrap();
    let observation = run_agent(
        root.path(),
        Mode::Auto,
        "edit-dev",
        Arc::new(HangingProvider),
        Duration::from_millis(50),
    )
    .await;
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    let missing_fixture = Observation {
        infrastructure_error: false,
        ..observation
    };
    std::fs::remove_file(root.path().join("protected.txt")).unwrap();
    assert_eq!(judge(root.path(), &missing_fixture), Verdict::Unknown);
}

#[tokio::test]
async fn offline_tasks_judge_files_permission_and_missing_verification() {
    assert_eq!(EVALUATOR_VERSION, "synthetic-v4");
    assert!(valid_suite(&CASES));
    for mode in [Mode::Auto, Mode::Readonly] {
        let root = tempfile::tempdir().unwrap();
        std::fs::write(root.path().join("protected.txt"), "original").unwrap();
        let observation = run_fake_task(root.path(), mode).await;
        let expected = if mode == Mode::Readonly {
            Verdict::Passed
        } else {
            Verdict::Unknown
        };
        assert_eq!(judge(root.path(), &observation), expected);
    }
}

#[tokio::test]
async fn offline_recovery_is_checked_independently_and_missing_verification_is_unknown() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("protected.txt"), "original").unwrap();
    let observation = run_fake_recovery_task(root.path()).await;
    assert_eq!(observation.failed_tools, Some(1));
    assert_eq!(
        std::fs::read_to_string(root.path().join("target.txt")).unwrap(),
        "fixed"
    );
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    std::fs::write(root.path().join("target.txt"), "wrong").unwrap();
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
}

#[test]
fn evaluator_rejects_bad_artifacts_missing_cases_and_unknown_results() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("protected.txt"), "original").unwrap();
    std::fs::write(root.path().join("wrong.txt"), "fixed").unwrap();
    let mut observation = Observation {
        task: "edit-dev",
        verification: Some(Outcome::Passed),
        completed: true,
        permission_denied: false,
        infrastructure_error: false,
        failed_tools: None,
        steering_seen: None,
        cost_unknown: None,
        independent: Some(Verdict::Passed),
    };
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    std::fs::write(root.path().join("target.txt"), "fixed").unwrap();
    assert_eq!(judge(root.path(), &observation), Verdict::Passed);
    observation.independent = None;
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    observation.independent = Some(Verdict::Failed);
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    observation.independent = Some(Verdict::Unknown);
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    observation.independent = Some(Verdict::Passed);
    observation.verification = None;
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    observation.verification = Some(Outcome::NotRun);
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    observation.verification = Some(Outcome::Failed);
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    observation.infrastructure_error = true;
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    observation.infrastructure_error = false;
    observation.task = "permission-holdout";
    std::fs::remove_file(root.path().join("target.txt")).unwrap();
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    observation.permission_denied = true;
    assert_eq!(judge(root.path(), &observation), Verdict::Passed);
    std::fs::write(root.path().join("protected.txt"), "changed").unwrap();
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    assert!(!all_pass(&[]));
    assert!(!valid_suite(&[]));
    assert!(!valid_suite(&[
        ("a", "same-family", "development"),
        ("b", "same-family", "holdout")
    ]));
    assert!(!valid_suite(&[
        ("a", "one", "development"),
        ("a", "two", "holdout")
    ]));
    assert!(!all_pass(&[("edit-dev", Verdict::Passed)]));
    assert!(!all_pass(&[
        ("edit-dev", Verdict::Passed),
        ("permission-holdout", Verdict::Unknown)
    ]));
    assert!(!all_pass(&[
        ("edit-dev", Verdict::Passed),
        ("edit-dev", Verdict::Passed)
    ]));
    assert!(all_pass(&[
        ("edit-dev", Verdict::Passed),
        ("permission-holdout", Verdict::Passed),
        ("recovery-holdout", Verdict::Passed),
        ("steering-holdout", Verdict::Passed)
    ]));
}

#[test]
fn evaluator_requires_steering_delivery_and_known_interrupted_cost() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("protected.txt"), "original").unwrap();
    std::fs::write(root.path().join("target.txt"), "fixed").unwrap();
    let mut observation = Observation {
        task: "steering-holdout",
        verification: None,
        completed: true,
        permission_denied: false,
        infrastructure_error: false,
        failed_tools: Some(0),
        steering_seen: Some(false),
        cost_unknown: Some(false),
        independent: None,
    };
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    observation.steering_seen = Some(true);
    observation.cost_unknown = Some(true);
    assert_eq!(judge(root.path(), &observation), Verdict::Failed);
    observation.cost_unknown = None;
    assert_eq!(judge(root.path(), &observation), Verdict::Unknown);
    observation.cost_unknown = Some(false);
    assert_eq!(judge(root.path(), &observation), Verdict::Passed);
}

#[tokio::test]
async fn offline_steering_reaches_the_resumed_request_and_keeps_cost_known() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("protected.txt"), "original").unwrap();
    let observation = run_fake_steering_task(root.path()).await;
    assert_eq!(observation.steering_seen, Some(true));
    assert_eq!(observation.cost_unknown, Some(false));
    assert_eq!(judge(root.path(), &observation), Verdict::Passed);
}

#[tokio::test]
async fn offline_uninterrupted_priced_request_has_known_cost() {
    let root = tempfile::tempdir().unwrap();
    let tools = Arc::new(
        LocalTools::new(
            root.path().to_path_buf(),
            Arc::new(LocalAuthorizer {
                mode: Mode::Auto,
                input: None,
            }),
        )
        .unwrap(),
    );
    let provider = Arc::new(FakeProvider(Mutex::new(VecDeque::from([response(vec![
        Block::Text("Completed.".into()),
    ])]))));
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 4,
            max_output_tokens: 128,
            budget: None,
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (result, accounting, _) = agent
        .run_detached("No interruption".into(), CancellationToken::new())
        .await;
    assert!(result.is_ok());
    assert_eq!(accounting.cost, Some(0.0));
}

#[tokio::test]
async fn registered_offline_suite_runs_each_case_without_promoting_unknowns() {
    let mut results = Vec::new();
    for (id, _, _) in CASES {
        let root = tempfile::tempdir().unwrap();
        std::fs::write(root.path().join("protected.txt"), "original").unwrap();
        let observation = match id {
            "edit-dev" => run_fake_task(root.path(), Mode::Auto).await,
            "permission-holdout" => run_fake_task(root.path(), Mode::Readonly).await,
            "recovery-holdout" => run_fake_recovery_task(root.path()).await,
            "steering-holdout" => run_fake_steering_task(root.path()).await,
            _ => panic!("registered task has no offline runner: {id}"),
        };
        assert_eq!(observation.task, id);
        results.push((id, judge(root.path(), &observation)));
    }
    assert_eq!(
        results,
        [
            ("edit-dev", Verdict::Unknown),
            ("permission-holdout", Verdict::Passed),
            ("recovery-holdout", Verdict::Unknown),
            ("steering-holdout", Verdict::Passed),
        ]
    );
    assert!(!all_pass(&results));
}

#[tokio::test]
async fn independent_fixture_verifier_checks_real_files_and_fails_closed() {
    let root = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("protected.txt"), "original").unwrap();
    std::fs::write(root.path().join("target.txt"), "fixed").unwrap();
    assert_eq!(
        independent_fixture_verify(root.path()).await,
        Verdict::Passed
    );
    std::fs::write(root.path().join("target.txt"), "wrong").unwrap();
    assert_eq!(
        independent_fixture_verify(root.path()).await,
        Verdict::Failed
    );
    std::fs::remove_file(root.path().join("protected.txt")).unwrap();
    assert_eq!(
        independent_fixture_verify(root.path()).await,
        Verdict::Unknown
    );
}
