use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
};

use cove::{
    domain::{
        Block, Error, Event, Message, ModelRequest, ModelResponse, Provider, Result, Role, Usage,
    },
    engine::agent::{Agent, Limits},
    safety::permission::{LocalAuthorizer, Mode},
    session::workspace::Workspace,
    session::{CallState, PendingCall, Session, SessionPersistence, State, Store},
    tools::{LocalTools, ToolExecutor, ToolOutput},
};
use futures_util::future::BoxFuture;
use serde_json::json;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

struct Fake {
    responses: Mutex<VecDeque<ModelResponse>>,
    requests: Mutex<Vec<ModelRequest>>,
}

impl Fake {
    fn new(responses: Vec<ModelResponse>) -> Arc<Self> {
        Arc::new(Self {
            responses: Mutex::new(responses.into()),
            requests: Mutex::new(Vec::new()),
        })
    }
}

impl Provider for Fake {
    fn stream(
        &self,
        request: ModelRequest,
        _: mpsc::Sender<Event>,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            self.requests.lock().unwrap().push(request);
            self.responses
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
        usage: Usage {
            input: 10,
            output: 5,
            ..Usage::default()
        },
        complete: true,
        usage_reported: true,
        reported_cost: None,
        model: None,
    }
}

fn setup(root: &std::path::Path, store: &Store) -> (Arc<Workspace>, Arc<LocalTools>) {
    let workspace = Arc::new(Workspace::new(root.to_path_buf(), store.clone()).unwrap());
    let tools = Arc::new(
        LocalTools::new(
            root.to_path_buf(),
            Arc::new(LocalAuthorizer {
                mode: Mode::Bypass,
                input: None,
            }),
        )
        .unwrap()
        .with_workspace(workspace.clone()),
    );
    (workspace, tools)
}

fn agent(provider: Arc<Fake>, tools: Arc<LocalTools>, max_turns: u32) -> Agent {
    Agent::new(
        provider,
        tools,
        Limits {
            max_turns,
            max_output_tokens: 256,
            budget: None,
            prices: None,
        },
    )
    .unwrap()
}

fn session(root: &std::path::Path) -> Session {
    Session::new(
        root.canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        None,
    )
}

fn events() -> (mpsc::Sender<Event>, tokio::task::JoinHandle<()>) {
    let (tx, mut rx) = mpsc::channel(64);
    (
        tx,
        tokio::spawn(async move { while rx.recv().await.is_some() {} }),
    )
}

#[test]
fn versioned_store_rejects_corrupt_data_and_overlapping_session_owners() {
    let data = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let session = session(root.path());
    store.save(&session).unwrap();
    let lease = store.acquire(&session.id).unwrap();
    assert!(store.acquire(&session.id).is_err());
    assert_eq!(store.load(&session.id).unwrap().cwd, session.cwd);
    assert_eq!(store.list().unwrap().len(), 1);
    drop(lease);
    assert!(store.acquire(&session.id).is_ok());
    assert!(store.load("../escape").is_err());
    std::fs::write(
        data.path()
            .join("sessions")
            .join(format!("{}.json", session.id)),
        b"{",
    )
    .unwrap();
    assert!(store.load(&session.id).is_err());
}

#[test]
fn workspace_has_a_single_cli_owner_per_data_directory() {
    let data = tempfile::tempdir().unwrap();
    let root = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let lease = store.acquire_workspace(root.path()).unwrap();
    assert!(store.acquire_workspace(root.path()).is_err());
    drop(lease);
    assert!(store.acquire_workspace(root.path()).is_ok());
}

#[tokio::test]
async fn model_dispatch_preserves_cost_before_contacting_provider() {
    struct ObservingProvider {
        store: Store,
        id: String,
        renewed: bool,
    }
    impl Provider for ObservingProvider {
        fn stream(
            &self,
            _: ModelRequest,
            _: mpsc::Sender<Event>,
            _: CancellationToken,
        ) -> BoxFuture<'_, Result<ModelResponse>> {
            Box::pin(async move {
                let durable = self.store.load(&self.id)?;
                if self.renewed {
                    assert_eq!(durable.accounting.cost, None);
                    assert!(durable.accounting.renewed_budget.is_some());
                    assert_eq!(durable.accounting.budget_cost(), Some(0.0));
                } else {
                    assert_eq!(durable.accounting.cost, Some(0.0));
                }
                assert_eq!(durable.accounting.model_calls, 1);
                Ok(response(vec![Block::Text("done".into())]))
            })
        }
    }
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    let provider = Arc::new(ObservingProvider {
        store: store.clone(),
        id: session.id.clone(),
        renewed: false,
    });
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 2,
            max_output_tokens: 256,
            budget: Some(1.0),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    agent
        .run_in(
            &mut session,
            Some("task".into()),
            &store,
            tx,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(
        store.load(&session.id).unwrap().accounting.cost,
        Some(0.000015)
    );

    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    session.accounting.cost = None;
    session.accounting.reset_budget_spend();
    let provider = Arc::new(ObservingProvider {
        store: store.clone(),
        id: session.id.clone(),
        renewed: true,
    });
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 2,
            max_output_tokens: 256,
            budget: Some(1.0),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    agent
        .run_in(
            &mut session,
            Some("task".into()),
            &store,
            tx,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    drain.await.unwrap();
    let saved = store.load(&session.id).unwrap();
    assert_eq!(saved.accounting.cost, None);
    assert_eq!(saved.accounting.budget_cost(), Some(0.000015));
}

#[tokio::test]
async fn rollback_restores_original_crlf_and_removes_only_created_files() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("old.txt"), "before\r\n").unwrap();
    let store = Store::open(data.path()).unwrap();
    let (workspace, tools) = setup(root.path(), &store);
    let id = workspace.begin().unwrap();
    for (name, input) in [
        (
            "edit",
            json!({"path":"old.txt","old":"before","new":"after"}),
        ),
        (
            "edit",
            json!({"path":"old.txt","old":"after","new":"final"}),
        ),
        ("write", json!({"path":"created.txt","content":"created"})),
    ] {
        assert!(
            !tools
                .execute(name.into(), input, CancellationToken::new())
                .await
                .unwrap()
                .failed
        );
    }
    assert!(workspace.diff().unwrap().contains("+final"));
    workspace.activate(Some(&id)).unwrap();
    assert!(workspace.undo_preview().unwrap().contains("created.txt"));
    workspace.undo().unwrap();
    assert_eq!(
        std::fs::read(root.path().join("old.txt")).unwrap(),
        b"before\r\n"
    );
    assert!(!root.path().join("created.txt").exists());
    assert!(workspace.undo().is_err());
}

#[tokio::test]
async fn rollback_conflict_aborts_before_restoring_any_file() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (workspace, tools) = setup(root.path(), &store);
    workspace.begin().unwrap();
    for file in ["one", "two"] {
        std::fs::write(root.path().join(file), "old").unwrap();
        assert!(
            !tools
                .execute(
                    "edit".into(),
                    json!({"path":file,"old":"old","new":"agent"}),
                    CancellationToken::new()
                )
                .await
                .unwrap()
                .failed
        );
    }
    std::fs::write(root.path().join("one"), "user edit").unwrap();
    assert!(workspace.undo_preview().is_err());
    assert!(workspace.undo().is_err());
    assert_eq!(
        std::fs::read_to_string(root.path().join("one")).unwrap(),
        "user edit"
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("two")).unwrap(),
        "agent"
    );
}

#[tokio::test]
async fn shell_effects_are_explicitly_excluded_from_file_rollback() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (workspace, tools) = setup(root.path(), &store);
    workspace.begin().unwrap();
    let script = if cove::process::Shell::current().kind == cove::process::ShellKind::PowerShell {
        "Set-Content -LiteralPath shell.txt -Value shell"
    } else {
        "printf shell > shell.txt"
    };
    assert!(
        !tools
            .execute(
                "shell".into(),
                json!({"command":script}),
                CancellationToken::new()
            )
            .await
            .unwrap()
            .failed
    );
    assert!(
        !tools
            .execute(
                "write".into(),
                json!({"path":"tracked","content":"tracked"}),
                CancellationToken::new()
            )
            .await
            .unwrap()
            .failed
    );
    assert!(
        workspace
            .undo_preview()
            .unwrap()
            .contains("Shell 命令的效果")
    );
    workspace.undo().unwrap();
    assert!(root.path().join("shell.txt").exists());
    assert!(!root.path().join("tracked").exists());
}

#[tokio::test]
async fn continue_uses_saved_results_instead_of_repeating_finished_tools() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (workspace, tools) = setup(root.path(), &store);
    workspace.begin().unwrap();
    let mut session = session(root.path());
    let first = Fake::new(vec![response(vec![Block::ToolUse {
        id: "write".into(),
        name: "write".into(),
        input: json!({"path":"once","content":"value"}),
        extra: None,
    }])]);
    let (tx, drain) = events();
    assert!(matches!(
        agent(first, tools.clone(), 1)
            .run_in(
                &mut session,
                Some("create once".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await,
        Err(Error::Limit(_))
    ));
    drain.await.unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    assert!(matches!(loaded.state, State::Interrupted(_)));
    let next = Fake::new(vec![response(vec![Block::Text(
        "Already created; no tests run.".into(),
    )])]);
    let (tx, drain) = events();
    agent(next.clone(), tools, 2)
        .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("once")).unwrap(),
        "value"
    );
    assert!(next.requests.lock().unwrap()[0].messages.iter().flat_map(|message| &message.blocks)
        .any(|block| matches!(block, Block::ToolResult { id, is_error:false, .. } if id == "write")));
}

#[tokio::test]
async fn crash_intents_are_uncertain_and_never_replayed_automatically() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    session.state = State::Running;
    session.messages = vec![
        Message::user("create a file"),
        Message {
            role: Role::Assistant,
            blocks: vec![Block::ToolUse {
                id: "uncertain".into(),
                name: "write".into(),
                input: json!({"path":"bad","content":"bad"}),
                extra: None,
            }],
        },
    ]
    .into();
    session.pending.push(PendingCall {
        id: "uncertain".into(),
        name: "write".into(),
        input: json!({"path":"bad","content":"bad"}),
        state: CallState::Executing,
        result: None,
    });
    store.save(&session).unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    assert_eq!(loaded.pending[0].state, CallState::Uncertain);
    let provider = Fake::new(Vec::new());
    let (tx, drain) = events();
    let result = agent(provider.clone(), tools, 2)
        .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
        .await;
    drain.await.unwrap();
    assert!(matches!(result, Err(Error::Session(_))));
    assert!(provider.requests.lock().unwrap().is_empty());
    assert!(!root.path().join("bad").exists());
}

#[test]
fn orphan_tool_results_are_rejected_on_load() {
    let root = tempfile::tempdir().unwrap();
    let mut session = session(root.path());
    session.messages.push(Message {
        role: Role::User,
        blocks: vec![Block::ToolResult {
            id: "missing".into(),
            content: "fake success".into(),
            is_error: false,
        }],
    });
    assert!(session.validate().is_err());
}

#[test]
fn rejected_oversized_write_does_not_leave_an_uncertain_checkpoint() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Workspace::new(root.path().into(), store).unwrap();
    workspace.begin().unwrap();
    assert!(
        workspace
            .replace(
                &root.path().canonicalize().unwrap().join("huge"),
                None,
                &"a".repeat(1024 * 1024 + 1)
            )
            .is_err()
    );
    assert!(!root.path().join("huge").exists());
    assert!(workspace.undo_preview().is_ok());
}

#[tokio::test]
async fn partially_finished_batch_skips_saved_steps_and_executes_only_planned_calls() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (workspace, tools) = setup(root.path(), &store);
    workspace.begin().unwrap();
    std::fs::write(root.path().join("first"), "already done").unwrap();
    let mut session = session(root.path());
    session.messages.push(Message::user("create two files"));
    let mut blocks = Vec::new();
    for (id, state) in [
        ("first", CallState::Finished),
        ("second", CallState::Planned),
    ] {
        let input = json!({"path":id,"content":"new"});
        blocks.push(Block::ToolUse {
            id: id.into(),
            name: "write".into(),
            input: input.clone(),
            extra: None,
        });
        session.pending.push(PendingCall {
            id: id.into(),
            name: "write".into(),
            input,
            state: state.clone(),
            result: (state == CallState::Finished).then(|| ToolOutput {
                content: "saved result".into(),
                failed: false,
                images: Vec::new(),
                refused: false,
            }),
        });
    }
    session.messages.push(Message {
        role: Role::Assistant,
        blocks,
    });
    session.state = State::Running;
    store.save(&session).unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    let provider = Fake::new(vec![response(vec![Block::Text("done".into())])]);
    let (tx, drain) = events();
    let report = agent(provider.clone(), tools, 2)
        .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.tools_run, 1);
    assert_eq!(report.tools_failed, 0);
    assert_eq!(
        std::fs::read_to_string(root.path().join("first")).unwrap(),
        "already done"
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("second")).unwrap(),
        "new"
    );
    let requests = provider.requests.lock().unwrap();
    assert_eq!(
        requests[0]
            .messages
            .iter()
            .flat_map(|message| &message.blocks)
            .filter(|block| matches!(block, Block::ToolResult { .. }))
            .count(),
        2
    );
}

#[tokio::test]
async fn uncertain_file_intent_requires_matching_evidence_before_resolution() {
    for executed in [true, false] {
        let root = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let store = Store::open(data.path()).unwrap();
        let (workspace, tools) = setup(root.path(), &store);
        let id = workspace.begin().unwrap();
        std::fs::write(root.path().join("file"), "before").unwrap();
        assert!(
            !tools
                .execute(
                    "edit".into(),
                    json!({"path":"file","old":"before","new":"after"}),
                    CancellationToken::new()
                )
                .await
                .unwrap()
                .failed
        );
        let path = store.checkpoint_path(&id).unwrap();
        let mut checkpoint: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        checkpoint["changes"][0]["after"] = checkpoint["changes"][0]["before"].clone();
        checkpoint["changes"][0]["state"] = json!("Intent");
        checkpoint["changes"][0]["intended"] = json!("after");
        cove::session::atomic_json(&path, &checkpoint).unwrap();
        if !executed {
            std::fs::write(root.path().join("file"), "before").unwrap();
        }
        workspace.activate(Some(&id)).unwrap();
        assert!(workspace.undo_preview().is_err());
        assert!(
            workspace
                .resolve_edit(std::path::Path::new("file"), !executed)
                .is_err()
        );
        workspace
            .resolve_edit(std::path::Path::new("file"), executed)
            .unwrap();
        workspace.undo().unwrap();
        assert_eq!(
            std::fs::read_to_string(root.path().join("file")).unwrap(),
            "before"
        );
    }
}

fn long_history(session: &mut Session) {
    session.current_request = "Keep the exact current requirement: preserve CRLF.".into();
    session.messages = vec![
        Message::user("original task"),
        Message {
            role: Role::Assistant,
            blocks: vec![Block::Text("old detail ".repeat(3000))],
        },
        Message::user(session.current_request.clone()),
        Message {
            role: Role::Assistant,
            blocks: vec![
                Block::Thinking(
                    json!({"type":"thinking","thinking":"opaque","signature":"keep-me"}),
                ),
                Block::ToolUse {
                    id: "read".into(),
                    name: "read".into(),
                    input: json!({"path":"file"}),
                    extra: None,
                },
            ],
        },
        Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: "read".into(),
                content: "actual output".into(),
                is_error: false,
            }],
        },
        Message {
            role: Role::Assistant,
            blocks: vec![Block::Text("recent response".into())],
        },
    ]
    .into();
    session.state = State::Completed;
}

#[tokio::test]
async fn compaction_archives_original_and_preserves_request_tool_pairs_and_signature() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    long_history(&mut session);
    let original_size = cove::engine::context::bytes(&session.messages).unwrap();
    let provider = Fake::new(vec![response(vec![Block::Text(
        "Earlier files were inspected; no verified checks.".into(),
    )])]);
    let agent = agent(provider.clone(), tools, 3)
        .with_context_limit(16384)
        .unwrap();
    let (tx, drain) = events();
    agent
        .compact(&mut session, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert!(cove::engine::context::bytes(&session.messages).unwrap() < original_size);
    assert_eq!(session.compactions, 1);
    assert!(
        serde_json::to_string(&session.messages)
            .unwrap()
            .contains("Keep the exact current requirement")
    );
    assert!(
        serde_json::to_string(&session.messages)
            .unwrap()
            .contains("keep-me")
    );
    session.validate().unwrap();
    assert_eq!(
        std::fs::read_dir(data.path().join("archives"))
            .unwrap()
            .count(),
        1
    );
    assert_eq!(session.accounting.model_calls, 1);
    assert!(provider.requests.lock().unwrap()[0].tools.is_empty());
}

#[tokio::test]
async fn compaction_uses_larger_output_budget_without_exceeding_configured_limit() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    long_history(&mut session);
    let provider = Fake::new(vec![response(vec![Block::Text("Complete summary".into())])]);
    let mut agent = agent(provider.clone(), tools, 3)
        .with_context_limit(16384)
        .unwrap();
    agent.limits_mut().max_output_tokens = 10_000;
    let (tx, drain) = events();
    agent
        .compact(&mut session, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].max_output_tokens, 8192);
}

#[tokio::test]
async fn truncated_compaction_reports_limit_and_preserves_original_conversation() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    long_history(&mut session);
    let original = serde_json::to_vec(&session.messages).unwrap();
    let mut truncated = response(vec![Block::Text("partial summary".into())]);
    truncated.complete = false;
    let provider = Fake::new(vec![truncated]);
    let mut agent = agent(provider.clone(), tools, 3)
        .with_context_limit(16384)
        .unwrap();
    agent.limits_mut().max_output_tokens = 4096;
    let (tx, drain) = events();
    let error = agent
        .compact(&mut session, &store, tx, CancellationToken::new())
        .await
        .unwrap_err();
    drain.await.unwrap();
    assert!(error.to_string().contains("输出上限 4096"), "{error}");
    assert!(error.to_string().contains("/compact"), "{error}");
    assert!(serde_json::to_vec(&session.messages).unwrap() == original);
    assert_eq!(session.compactions, 0);
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].max_output_tokens, 4096);
}

fn million_window_history(session: &mut Session, count: usize) {
    long_history(session);
    let suffix = session.messages.to_vec();
    let old_text = "context ".repeat(1000);
    let mut messages: Vec<Message> = (0..count)
        .map(|index| Message {
            role: if index % 2 == 0 {
                Role::User
            } else {
                Role::Assistant
            },
            blocks: vec![Block::Text(old_text.clone())],
        })
        .collect();
    messages.extend(suffix);
    session.messages = messages.into();
}

#[tokio::test]
async fn million_window_compaction_preserves_history_on_truncation_and_persists_success() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    million_window_history(&mut session, 240);
    let original = serde_json::to_vec(&session.messages).unwrap();
    let before = cove::engine::context::tokens(&session.messages);
    let budget = cove::model::models::context_budget(1_000_000, 64_000);
    assert_eq!(budget, 850_000);
    assert!(before >= budget * 3 / 4 && before < budget, "{before}");
    store.save(&session).unwrap();
    let mut truncated = response(vec![Block::Text("partial summary".into())]);
    truncated.complete = false;
    truncated.usage.output = 8192;
    let summary = "summary ".repeat(1500);
    assert!(cove::engine::context::text_tokens(&summary) > 2048);
    let mut complete = response(vec![Block::Text(summary.clone())]);
    complete.usage.output = 4000;
    let provider = Fake::new(vec![truncated, complete]);
    let mut agent = agent(provider.clone(), tools, 3)
        .with_context_tokens(budget)
        .unwrap();
    agent.limits_mut().max_output_tokens = 64_000;
    let (tx, drain) = events();
    assert!(
        agent
            .compact(&mut session, &store, tx, CancellationToken::new())
            .await
            .is_err()
    );
    drain.await.unwrap();
    assert!(serde_json::to_vec(&session.messages).unwrap() == original);
    assert!(serde_json::to_vec(&store.load(&session.id).unwrap().messages).unwrap() == original);
    assert_eq!(provider.requests.lock().unwrap().len(), 1);
    let (tx, drain) = events();
    agent
        .compact(&mut session, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 2);
    assert!(
        requests
            .iter()
            .all(|request| request.max_output_tokens == 8192 && request.tools.is_empty())
    );
    assert!(requests[0].estimated_tokens.unwrap() > 600_000);
    assert_eq!(session.compactions, 1);
    assert!(cove::engine::context::tokens(&session.messages) < before);
    let saved = store.load(&session.id).unwrap();
    assert!(
        serde_json::to_vec(&saved.messages).unwrap()
            == serde_json::to_vec(&session.messages).unwrap()
    );
    assert!(saved.messages.iter().any(|message| message.blocks.iter().any(|block| matches!(block, Block::Text(text) if text.contains(&summary) && text.contains("preserve CRLF")))));
    assert!(saved.messages.iter().any(|message| {
        message
            .blocks
            .iter()
            .any(|block| matches!(block, Block::ToolResult { id, .. } if id == "read"))
    }));
    assert_eq!(
        std::fs::read_dir(data.path().join("archives"))
            .unwrap()
            .count(),
        2
    );
}

#[tokio::test]
async fn million_window_does_not_compact_before_new_threshold() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    million_window_history(&mut session, 240);
    let budget = cove::model::models::context_budget(1_000_000, 64_000);
    let before = cove::engine::context::tokens(&session.messages);
    assert!(before > budget * 3 / 4 && before < budget * 9 / 10);
    let provider = Fake::new(vec![
        response(vec![Block::Text("Task finished".into())]),
        response(vec![Block::Text("Unexpected extra request".into())]),
    ]);
    let agent = agent(provider.clone(), tools, 3)
        .with_context_tokens(budget)
        .unwrap();
    let (tx, drain) = events();
    agent
        .run_in(
            &mut session,
            Some("next task".into()),
            &store,
            tx,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(session.compactions, 0);
    assert_eq!(provider.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn million_window_automatically_compacts_before_continuing_task() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    million_window_history(&mut session, 300);
    let provider = Fake::new(vec![
        response(vec![Block::Text("Old verified context".into())]),
        response(vec![Block::Text("Task finished".into())]),
    ]);
    let budget = cove::model::models::context_budget(1_000_000, 64_000);
    let mut agent = agent(provider.clone(), tools, 3)
        .with_context_tokens(budget)
        .unwrap();
    agent.limits_mut().max_output_tokens = 64_000;
    let (tx, drain) = events();
    let report = agent
        .run_in(
            &mut session,
            Some("next task".into()),
            &store,
            tx,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(session.compactions, 1);
    assert_eq!(report.model_calls, 2);
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 2);
    assert_eq!(requests[0].max_output_tokens, 8192);
    assert!(requests[0].estimated_tokens.unwrap() > 600_000);
    assert!(
        requests[1]
            .messages
            .iter()
            .flat_map(|message| &message.blocks)
            .any(|block| matches!(block, Block::Text(text) if text.contains("next task")))
    );
}

#[tokio::test]
async fn multi_million_window_automatically_compacts_before_continuing_task() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    million_window_history(&mut session, 870);
    let budget = cove::model::models::context_budget(3_000_000, 64_000);
    let before = cove::engine::context::tokens(&session.messages);
    assert!(before > budget * 9 / 10 && before < budget, "{before}");
    let provider = Fake::new(vec![
        response(vec![Block::Text("Old verified context".into())]),
        response(vec![Block::Text("Task finished".into())]),
    ]);
    let mut agent = agent(provider.clone(), tools, 3)
        .with_context_tokens(budget)
        .unwrap();
    agent.limits_mut().max_output_tokens = 64_000;
    let (tx, drain) = events();
    agent
        .run_in(
            &mut session,
            Some("next task".into()),
            &store,
            tx,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(session.compactions, 1);
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 2);
    assert!(requests[0].estimated_tokens.unwrap() > 2_000_000);
    assert_eq!(requests[0].max_output_tokens, 8192);
    assert_eq!(store.load(&session.id).unwrap().compactions, 1);
}

#[tokio::test]
async fn million_window_compacts_near_context_budget() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    million_window_history(&mut session, 314);
    let before = cove::engine::context::tokens(&session.messages);
    let budget = cove::model::models::context_budget(1_000_000, 64_000);
    assert!(before > 830_000 && before < budget, "{before}");
    let provider = Fake::new(vec![response(vec![Block::Text("Complete summary".into())])]);
    let mut agent = agent(provider.clone(), tools, 3)
        .with_context_tokens(budget)
        .unwrap();
    agent.limits_mut().max_output_tokens = 64_000;
    let (tx, drain) = events();
    agent
        .compact(&mut session, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].estimated_tokens.unwrap() > 830_000);
    assert_eq!(requests[0].max_output_tokens, 8192);
    assert!(cove::engine::context::tokens(&session.messages) < before);
    assert_eq!(store.load(&session.id).unwrap().compactions, 1);
}

#[tokio::test]
async fn failed_compaction_retains_original_conversation() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    long_history(&mut session);
    let original = serde_json::to_string(&session.messages).unwrap();
    let provider = Fake::new(vec![response(vec![Block::Text("too large ".repeat(5000))])]);
    let agent = agent(provider, tools, 3).with_context_limit(16384).unwrap();
    let (tx, drain) = events();
    assert!(
        agent
            .compact(&mut session, &store, tx, CancellationToken::new())
            .await
            .is_err()
    );
    drain.await.unwrap();
    assert_eq!(serde_json::to_string(&session.messages).unwrap(), original);
    assert_eq!(session.compactions, 0);
}

#[tokio::test]
async fn automatic_compaction_uses_the_same_meter_and_then_continues() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    long_history(&mut session);
    let provider = Fake::new(vec![
        response(vec![Block::Text("Older inspection summary.".into())]),
        response(vec![Block::Text(
            "New task completed; checks not run.".into(),
        )]),
    ]);
    let agent = agent(provider.clone(), tools, 3)
        .with_context_limit(16384)
        .unwrap();
    let (tx, drain) = events();
    let report = agent
        .run_in(
            &mut session,
            Some("new requirement".into()),
            &store,
            tx,
            CancellationToken::new(),
        )
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.model_calls, 2);
    assert_eq!(report.usage.input, 20);
    assert_eq!(session.compactions, 1);
    assert!(
        provider.requests.lock().unwrap()[1]
            .messages
            .iter()
            .flat_map(|message| &message.blocks)
            .any(|block| matches!(block, Block::Text(text) if text.contains("new requirement")))
    );
}

#[test]
fn checkpoint_cannot_be_loaded_for_another_workspace() {
    let root = tempfile::tempdir().unwrap();
    let other = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Workspace::new(root.path().into(), store.clone()).unwrap();
    let id = workspace.begin().unwrap();
    assert!(
        Workspace::new(other.path().into(), store)
            .unwrap()
            .activate(Some(&id))
            .is_err()
    );
}

#[tokio::test]
async fn unknown_usage_counts_as_zero_and_preserves_known_spend() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    session.accounting.cost = Some(0.25);
    let mut missing_usage = response(vec![Block::Text("first".into())]);
    missing_usage.usage_reported = false;
    let provider = Fake::new(vec![
        missing_usage,
        response(vec![Block::Text("second".into())]),
    ]);
    let agent = Agent::new(
        provider.clone(),
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: Some(1.0),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut session,
                Some("first".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_ok()
    );
    drain.await.unwrap();
    assert_eq!(session.accounting.cost, Some(0.25));
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut session,
                Some("second".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_ok()
    );
    drain.await.unwrap();
    assert_eq!(session.accounting.cost, Some(0.250015));
    assert_eq!(provider.requests.lock().unwrap().len(), 2);
}

#[tokio::test]
async fn provider_failure_counts_as_zero_and_allows_continuation() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    session.accounting.cost = Some(0.25);
    let provider = Fake::new(Vec::new());
    let agent = Agent::new(
        provider.clone(),
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: Some(1.0),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut session,
                Some("task".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    drain.await.unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    assert_eq!(loaded.accounting.cost, Some(0.25));
    assert_eq!(loaded.accounting.model_calls, 1);
    provider
        .responses
        .lock()
        .unwrap()
        .push_back(response(vec![Block::Text("done".into())]));
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
            .await
            .is_ok()
    );
    drain.await.unwrap();
    assert_eq!(loaded.accounting.cost, Some(0.250015));
    assert_eq!(provider.requests.lock().unwrap().len(), 2);
}

#[tokio::test]
async fn legacy_unknown_cost_resumes_with_a_new_budget_counter() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    session.accounting.cost = None;
    session.state = cove::session::State::Interrupted("old unknown cost".into());
    let provider = Fake::new(vec![response(vec![Block::Text("done".into())])]);
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: Some(1.0),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(&mut session, None, &store, tx, CancellationToken::new())
            .await
            .is_ok()
    );
    drain.await.unwrap();
    let loaded = store.load(&session.id).unwrap();
    assert_eq!(loaded.accounting.cost, None);
    assert_eq!(loaded.accounting.budget_cost(), Some(0.000015));
}

#[tokio::test]
async fn renewed_budget_counts_future_spend_without_claiming_historical_cost() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    let provider = Fake::new(Vec::new());
    let agent = Agent::new(
        provider.clone(),
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: Some(0.000025),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut session,
                Some("task".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    drain.await.unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    loaded.accounting.cost = None;
    loaded.accounting.reset_budget_spend();
    store.save(&loaded).unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    provider.responses.lock().unwrap().extend([
        response(vec![Block::Text("done".into())]),
        response(vec![Block::Text("done again".into())]),
    ]);
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
            .await
            .is_ok()
    );
    drain.await.unwrap();
    assert_eq!(loaded.accounting.cost, None);
    assert_eq!(loaded.accounting.budget_cost(), Some(0.000015));
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut loaded,
                Some("next task".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_ok()
    );
    drain.await.unwrap();
    assert_eq!(loaded.accounting.cost, None);
    assert_eq!(loaded.accounting.budget_cost(), Some(0.00003));
    assert_eq!(provider.requests.lock().unwrap().len(), 3);
}

#[tokio::test]
async fn renewed_budget_counts_missing_usage_as_zero() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock".into(),
        Some((1.0, 1.0)),
    );
    session.accounting.cost = None;
    session.accounting.reset_budget_spend();
    let mut missing_usage = response(vec![Block::Text("done".into())]);
    missing_usage.usage_reported = false;
    let provider = Fake::new(vec![missing_usage]);
    let agent = Agent::new(
        provider.clone(),
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: Some(1.0),
            prices: Some((1.0, 1.0)),
        },
    )
    .unwrap();
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut session,
                Some("task".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_ok()
    );
    drain.await.unwrap();
    let loaded = store.load(&session.id).unwrap();
    assert_eq!(loaded.accounting.cost, None);
    assert_eq!(loaded.accounting.budget_cost(), Some(0.0));
    assert_eq!(provider.requests.lock().unwrap().len(), 1);
}

#[test]
fn renewed_budget_counts_unknown_subagent_spend_as_zero() {
    let mut accounting = cove::session::Accounting::default();
    accounting.reset_budget_spend();
    cove::engine::agent::add_spend(
        &mut accounting,
        &cove::session::Accounting {
            cost: Some(0.25),
            ..Default::default()
        },
    );
    assert_eq!(accounting.cost, None);
    assert_eq!(accounting.budget_cost(), Some(0.25));
    cove::engine::agent::add_spend(&mut accounting, &cove::session::Accounting::default());
    assert_eq!(accounting.budget_cost(), Some(0.25));
}

#[tokio::test]
async fn continuation_does_not_hide_a_failure_from_an_earlier_window() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let (_, tools) = setup(root.path(), &store);
    let mut session = session(root.path());
    let provider = Fake::new(vec![
        response(vec![Block::ToolUse {
            id: "read".into(),
            name: "read".into(),
            input: json!({"path":"missing"}),
            extra: None,
        }]),
        // Tool-free wrap-up when the one-call window is used up.
        response(vec![Block::Text("Read failed; nothing else done.".into())]),
        response(vec![Block::Text("Task ended with a failed read.".into())]),
    ]);
    let agent = agent(provider, tools, 1);
    let (tx, drain) = events();
    assert!(
        agent
            .run_in(
                &mut session,
                Some("read missing".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    drain.await.unwrap();
    let mut loaded = store.load(&session.id).unwrap();
    let (tx, drain) = events();
    let report = agent
        .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.tools_failed, 1);
}
