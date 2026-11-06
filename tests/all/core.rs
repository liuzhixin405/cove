use std::{
    collections::VecDeque,
    sync::{Arc, Mutex},
    time::Duration,
};

use cove::{
    domain::{
        Block, Error, Event, Message, ModelRequest, ModelResponse, Provider, Result, Role, Usage,
    },
    engine::agent::{Agent, Limits},
    process,
    safety::permission::{LocalAuthorizer, Mode},
    session::{Ephemeral, Session, State, Store},
    tools::{LocalTools, ToolExecutor},
};
use futures_util::future::BoxFuture;
use serde_json::json;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

fn tools(root: &std::path::Path, mode: Mode) -> LocalTools {
    LocalTools::new(
        root.to_path_buf(),
        Arc::new(LocalAuthorizer { mode, input: None }),
    )
    .unwrap()
}

#[tokio::test]
async fn large_context_warns_once_with_compact_and_new_hints() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            response(vec![Block::Text("one".into())]),
            response(vec![Block::Text("two".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits())
        .unwrap()
        .with_context_tokens(850_000)
        .unwrap();
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), None);
    session.messages.push(Message::user("x".repeat(1_300_000)));
    session.messages.push(Message {
        role: Role::Assistant,
        blocks: vec![Block::Text("ok".into())],
    });
    let mut notices = Vec::new();
    for prompt in ["first", "second"] {
        let (tx, mut rx) = mpsc::channel(256);
        agent
            .run_in(
                &mut session,
                Some(prompt.into()),
                &Ephemeral,
                tx,
                CancellationToken::new(),
            )
            .await
            .unwrap();
        while let Ok(event) = rx.try_recv() {
            if let Event::Notice(text) = event
                && text.starts_with("[上下文]")
            {
                notices.push(text);
            }
        }
    }
    assert_eq!(notices.len(), 1, "{notices:?}");
    assert!(notices[0].contains("/compact") && notices[0].contains("/new"));
}

#[tokio::test]
async fn editing_preserves_crlf_and_rejects_ambiguous_changes() {
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("中文.txt");
    std::fs::write(&path, "first\r\nold\r\nlast\r\n").unwrap();
    let tools = tools(dir.path(), Mode::Auto);
    let out = tools
        .execute(
            "edit".into(),
            json!({"path":"中文.txt","old":"old","new":"new"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(!out.failed);
    assert_eq!(std::fs::read(&path).unwrap(), b"first\r\nnew\r\nlast\r\n");
    let before = std::fs::read(&path).unwrap();
    let out = tools
        .execute(
            "edit".into(),
            json!({"path":"中文.txt","old":"\r\n","new":"\n"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(out.failed);
    assert_eq!(std::fs::read(&path).unwrap(), before);
}

#[tokio::test]
async fn writes_never_overwrite_and_traversal_is_rejected() {
    let dir = tempfile::tempdir().unwrap();
    let tools = tools(dir.path(), Mode::Auto);
    let token = CancellationToken::new();
    assert!(
        !tools
            .execute(
                "write".into(),
                json!({"path":"new.txt","content":"draft"}),
                token.clone()
            )
            .await
            .unwrap()
            .failed
    );
    // The file changed since the tool wrote it: replacing it needs a fresh read.
    std::fs::write(dir.path().join("new.txt"), "first").unwrap();
    assert!(
        tools
            .execute(
                "write".into(),
                json!({"path":"new.txt","content":"second"}),
                token.clone()
            )
            .await
            .unwrap()
            .failed
    );
    assert!(
        tools
            .execute(
                "write".into(),
                json!({"path":"../outside.txt","content":"bad"}),
                token.clone()
            )
            .await
            .unwrap()
            .failed
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("new.txt")).unwrap(),
        "first"
    );
    std::fs::create_dir(dir.path().join(".git")).unwrap();
    std::fs::write(dir.path().join(".git").join("config"), "private").unwrap();
    assert!(
        tools
            .execute("read".into(), json!({"path":".git\\config"}), token)
            .await
            .unwrap()
            .failed
    );
}

#[tokio::test]
async fn noninteractive_default_allows_workspace_writes_but_denies_shell() {
    let dir = tempfile::tempdir().unwrap();
    let tools = tools(dir.path(), Mode::Ask);
    // Copilot-style default: writes confined to the workspace need no approval,
    // even without a terminal to ask on.
    let out = tools
        .execute(
            "write".into(),
            json!({"path":"file","content":"ok"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(!out.failed);
    assert_eq!(
        std::fs::read_to_string(dir.path().join("file")).unwrap(),
        "ok"
    );
    assert!(matches!(
        tools
            .execute(
                "shell".into(),
                json!({"command":"echo nope > shell.txt"}),
                CancellationToken::new()
            )
            .await,
        Err(Error::Approval(_))
    ));
    assert!(!dir.path().join("shell.txt").exists());
}

#[tokio::test]
async fn search_respects_ignore_and_rejects_invalid_arguments() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join(".gitignore"), "ignored.txt\n").unwrap();
    std::fs::write(dir.path().join("visible.txt"), "needle\n").unwrap();
    std::fs::write(dir.path().join("ignored.txt"), "needle\n").unwrap();
    let tools = tools(dir.path(), Mode::Readonly);
    for (name, input) in [
        ("glob", json!({"pattern":"*.txt"})),
        ("grep", json!({"pattern":"needle"})),
    ] {
        let out = tools
            .execute(name.into(), input, CancellationToken::new())
            .await
            .unwrap();
        assert!(!out.failed);
        assert!(out.content.contains("visible.txt"));
        assert!(!out.content.contains("ignored.txt"));
    }
    assert!(
        tools
            .execute(
                "read".into(),
                json!({"path":"visible.txt","lines":0}),
                CancellationToken::new()
            )
            .await
            .unwrap()
            .failed
    );
    assert!(
        tools
            .execute(
                "write".into(),
                json!({"path":"bad","content":"no","unknown":true}),
                CancellationToken::new()
            )
            .await
            .unwrap()
            .failed
    );
}

#[cfg(windows)]
#[tokio::test]
async fn alternate_data_streams_are_rejected() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("visible.txt"), "value").unwrap();
    let out = tools(dir.path(), Mode::Auto)
        .execute(
            "write".into(),
            json!({"path":"visible.txt:hidden","content":"secret"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(out.failed);
}

#[cfg(unix)]
#[tokio::test]
async fn symlink_escape_is_rejected() {
    let dir = tempfile::tempdir().unwrap();
    let outside = tempfile::tempdir().unwrap();
    std::os::unix::fs::symlink(outside.path(), dir.path().join("link")).unwrap();
    let out = tools(dir.path(), Mode::Auto)
        .execute(
            "write".into(),
            json!({"path":"link/file","content":"bad"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(out.failed);
    assert!(!outside.path().join("file").exists());
}

#[tokio::test]
async fn shell_preserves_exit_code_stdout_and_stderr() {
    let dir = tempfile::tempdir().unwrap();
    let script = if cfg!(windows) {
        "[Console]::Out.WriteLine('ok'); [Console]::Error.WriteLine('problem'); exit 7"
    } else {
        "printf 'ok\\n'; printf 'problem\\n' >&2; exit 7"
    };
    let out = process::shell(
        dir.path(),
        script,
        Duration::from_secs(15),
        CancellationToken::new(),
    )
    .await
    .unwrap();
    assert_eq!(out.code, Some(7));
    assert!(out.stdout.contains("ok"));
    assert!(out.stderr.contains("problem"));
}

#[tokio::test]
async fn shell_drains_capped_output_without_deadlocking() {
    let dir = tempfile::tempdir().unwrap();
    let script = if cfg!(windows) {
        "[Console]::Out.Write(('x' * 100000)); [Console]::Error.Write(('y' * 100000))"
    } else {
        "head -c 100000 /dev/zero; head -c 100000 /dev/zero >&2"
    };
    let out = process::shell(
        dir.path(),
        script,
        Duration::from_secs(15),
        CancellationToken::new(),
    )
    .await
    .unwrap();
    assert_eq!(out.code, Some(0));
    assert!(out.truncated);
    // Head and tail are kept, the middle is replaced by a marker.
    assert!(
        out.stdout.len() <= process::STDOUT_LIMIT + 100,
        "{}",
        out.stdout.len()
    );
    assert!(
        out.stderr.len() <= process::STDERR_LIMIT + 100,
        "{}",
        out.stderr.len()
    );
    assert!(out.stdout.starts_with('x') && out.stdout.trim_end().ends_with('x'));
    assert!(out.stdout.contains("bytes omitted"));
    assert!(out.stderr.starts_with('y') && out.stderr.trim_end().ends_with('y'));
}

#[tokio::test]
async fn shell_deadline_is_visible() {
    let dir = tempfile::tempdir().unwrap();
    let script = if cfg!(windows) {
        "Start-Sleep -Seconds 20"
    } else {
        "sleep 20"
    };
    let out = process::shell(
        dir.path(),
        script,
        Duration::from_millis(250),
        CancellationToken::new(),
    )
    .await
    .unwrap();
    assert!(out.timed_out);
    assert_eq!(out.code, None);
}

struct FakeProvider {
    responses: Mutex<VecDeque<ModelResponse>>,
    requests: Mutex<Vec<ModelRequest>>,
}

type RecordedEvidence = (Option<String>, bool, Option<cove::safety::verify::Outcome>);

#[derive(Default)]
struct EndEvidence(Mutex<Vec<RecordedEvidence>>);

struct SequenceVerifier(Mutex<VecDeque<Option<cove::safety::verify::Verification>>>);

impl cove::safety::verify::Verifier for SequenceVerifier {
    fn verify(
        &self,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<Option<cove::safety::verify::Verification>>> {
        Box::pin(async move { Ok(self.0.lock().unwrap().pop_front().unwrap()) })
    }
}

impl cove::engine::turn::TurnHook for EndEvidence {
    fn task_end_with_verification<'a>(
        &'a self,
        session: &'a Session,
        completed: bool,
        verification: Option<cove::safety::verify::Outcome>,
        _budget: Option<f64>,
    ) -> BoxFuture<'a, ()> {
        self.0
            .lock()
            .unwrap()
            .push((session.task_id.clone(), completed, verification));
        Box::pin(async {})
    }
}

#[tokio::test]
async fn task_end_hooks_receive_distinct_persisted_task_ids_and_verification() {
    let dir = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            response(vec![Block::Text("first done".into())]),
            response(vec![Block::Text("second done".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let evidence = Arc::new(EndEvidence::default());
    let verifier = Arc::new(SequenceVerifier(Mutex::new(VecDeque::from([
        Some(cove::safety::verify::Verification {
            outcome: cove::safety::verify::Outcome::Passed,
            summary: "checks passed".into(),
            detail: String::new(),
        }),
        None,
    ]))));
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits())
        .unwrap()
        .with_turn_hook(evidence.clone())
        .with_verifier(Some(verifier));
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), None);
    for prompt in ["first", "second"] {
        let (tx, mut rx) = mpsc::channel(64);
        let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
        agent
            .run_in(
                &mut session,
                Some(prompt.into()),
                &store,
                tx,
                CancellationToken::new(),
            )
            .await
            .unwrap();
        drain.await.unwrap();
        assert_eq!(store.load(&session.id).unwrap().task_id, session.task_id);
    }
    let entries = evidence.0.lock().unwrap();
    assert_eq!(entries.len(), 2);
    assert!(entries[0].0.is_some());
    assert!(entries[1].0.is_some());
    assert_ne!(entries[0].0, entries[1].0);
    assert!(entries.iter().all(|(_, completed, _)| *completed));
    assert_eq!(entries[0].2, Some(cove::safety::verify::Outcome::Passed));
    assert_eq!(entries[1].2, None);
}

/// First call hangs forever (a slow model mid-generation); later calls use `inner`.
struct HangOnce {
    first: std::sync::atomic::AtomicBool,
    inner: FakeProvider,
}

impl Provider for HangOnce {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            if self.first.swap(false, std::sync::atomic::Ordering::SeqCst) {
                // Never completes; the agent must abort this call when steering arrives.
                std::future::pending::<()>().await;
                return Err(Error::Provider("unreachable".into()));
            }
            self.inner.stream(request, events, cancel).await
        })
    }
}

struct HangTwice {
    calls: std::sync::atomic::AtomicUsize,
    started: tokio::sync::Notify,
    inner: FakeProvider,
}

struct SettleOnce {
    first: std::sync::atomic::AtomicBool,
    started: tokio::sync::Notify,
    release: tokio::sync::Notify,
    usage_reported: bool,
    inner: FakeProvider,
}

impl Provider for SettleOnce {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            if self.first.swap(false, std::sync::atomic::Ordering::SeqCst) {
                self.started.notify_one();
                self.release.notified().await;
                let mut result = response(vec![Block::ToolUse {
                    id: "obsolete".into(),
                    name: "write".into(),
                    input: json!({"path":"obsolete.txt","content":"wrong"}),
                    extra: None,
                }]);
                result.usage_reported = self.usage_reported;
                return Ok(result);
            }
            self.inner.stream(request, events, cancel).await
        })
    }
}

impl Provider for HangTwice {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            let call = self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            if call < 2 {
                self.started.notify_one();
                std::future::pending::<()>().await;
                return Err(Error::Provider("unreachable".into()));
            }
            self.inner.stream(request, events, cancel).await
        })
    }
}

/// A test SteerSource: `push` is the user typing mid-task.
struct TestSteer {
    text: Mutex<Vec<String>>,
    wake: tokio::sync::Notify,
}

impl TestSteer {
    fn push(&self, text: &str) {
        self.text.lock().unwrap().push(text.to_owned());
        self.wake.notify_one();
    }
}

impl cove::domain::SteerSource for TestSteer {
    fn wake(&self) -> BoxFuture<'_, ()> {
        Box::pin(async { self.wake.notified().await })
    }

    fn take(&self) -> Option<String> {
        let mut text = self.text.lock().unwrap();
        (!text.is_empty()).then(|| text.drain(..).collect::<Vec<_>>().join("\n"))
    }
}

#[tokio::test]
async fn steering_text_aborts_the_in_flight_model_call() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(HangOnce {
        first: true.into(),
        inner: FakeProvider {
            responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
            requests: Mutex::new(Vec::new()),
        },
    });
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits(),
    )
    .unwrap()
    .with_steer(steer.clone());
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let (result, _) = tokio::join!(
        agent.run("write a function".into(), tx, CancellationToken::new()),
        async {
            tokio::time::sleep(std::time::Duration::from_millis(300)).await;
            steer.push("use iteration instead of recursion");
        }
    );
    drain.await.unwrap();
    let report = result.unwrap();
    // The hung call was aborted and the question re-asked with the steer text.
    assert_eq!(report.model_calls, 2);
    let requests = provider.inner.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert!(
        requests[0].messages.iter().any(|message| {
            message.role == Role::User
                && message.blocks.iter().any(|block| {
                    matches!(block, Block::Text(text) if text.contains("use iteration instead of recursion"))
                })
        }),
        "steer text must reach the retried request"
    );
}

#[tokio::test]
async fn steering_counts_interrupted_request_as_zero() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(HangOnce {
        first: true.into(),
        inner: FakeProvider {
            responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
            requests: Mutex::new(Vec::new()),
        },
    });
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    let prices = Some((1.0, 1.0));
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        Limits {
            prices,
            budget: Some(1.0),
            ..limits()
        },
    )
    .unwrap()
    .with_steer(steer.clone());
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), prices);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let (result, _) = tokio::join!(
        agent.run_in(
            &mut session,
            Some("write a function".into()),
            &Ephemeral,
            tx,
            CancellationToken::new()
        ),
        async {
            tokio::time::sleep(Duration::from_millis(300)).await;
            steer.push("use iteration instead of recursion");
        }
    );
    drain.await.unwrap();
    assert!(result.is_ok());
    assert_eq!(session.accounting.model_calls, 2);
    assert_eq!(session.accounting.cost, Some(0.000015));
    assert_eq!(provider.inner.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn steering_repeated_interruptions_preserve_both_messages_and_known_cost() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(HangTwice {
        calls: 0.into(),
        started: tokio::sync::Notify::new(),
        inner: FakeProvider {
            responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
            requests: Mutex::new(Vec::new()),
        },
    });
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    let prices = Some((1.0, 1.0));
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        Limits { prices, ..limits() },
    )
    .unwrap()
    .with_steer(steer.clone());
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), prices);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let (result, _) = tokio::time::timeout(Duration::from_secs(5), async {
        tokio::join!(
            agent.run_in(
                &mut session,
                Some("write a function".into()),
                &Ephemeral,
                tx,
                CancellationToken::new()
            ),
            async {
                provider.started.notified().await;
                steer.push("first adjustment");
                provider.started.notified().await;
                steer.push("second adjustment");
            }
        )
    })
    .await
    .expect("repeated steering timed out");
    drain.await.unwrap();
    assert_eq!(result.unwrap().model_calls, 3);
    assert_eq!(session.accounting.cost, Some(0.000015));
    let requests = provider.inner.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    for text in ["first adjustment", "second adjustment"] {
        assert!(requests[0].messages.iter().any(|message| {
            message.role == Role::User
                && message
                    .blocks
                    .iter()
                    .any(|block| matches!(block, Block::Text(content) if content.contains(text)))
        }));
    }
}

#[tokio::test]
async fn steering_with_budget_does_not_wait_for_usage() {
    let dir = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let provider = Arc::new(SettleOnce {
        first: true.into(),
        started: tokio::sync::Notify::new(),
        release: tokio::sync::Notify::new(),
        usage_reported: true,
        inner: FakeProvider {
            responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
            requests: Mutex::new(Vec::new()),
        },
    });
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    let prices = Some((1.0, 1.0));
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        Limits {
            prices,
            budget: Some(1.0),
            ..limits()
        },
    )
    .unwrap()
    .with_steer(steer.clone());
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), prices);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let (result, _) = tokio::join!(
        agent.run_in(
            &mut session,
            Some("write a function".into()),
            &store,
            tx,
            CancellationToken::new()
        ),
        async {
            provider.started.notified().await;
            steer.push("use iteration instead of recursion");
        }
    );
    drain.await.unwrap();
    assert_eq!(result.unwrap().model_calls, 2);
    let saved = store.load(&session.id).unwrap();
    assert_eq!(saved.accounting.cost, Some(0.000015));
    assert_eq!(saved.accounting.model_calls, 2);
    assert!(matches!(saved.state, State::Completed));
    assert!(saved.messages.iter().any(|message| {
        message.role == Role::User
            && message.blocks.iter().any(|block| {
                matches!(block, Block::Text(text) if text.contains("use iteration instead of recursion"))
            })
    }));
    assert!(!dir.path().join("obsolete.txt").exists());
    let requests = provider.inner.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].messages.iter().any(|message| {
        message.role == Role::User
            && message.blocks.iter().any(|block| {
                matches!(block, Block::Text(text) if text.contains("use iteration instead of recursion"))
            })
    }));
}

#[tokio::test]
async fn steering_with_budget_continues_when_spent() {
    for (usage_reported, budget) in [(false, 1.0), (true, 0.00001)] {
        let dir = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let store = Store::open(data.path()).unwrap();
        let provider = Arc::new(SettleOnce {
            first: true.into(),
            started: tokio::sync::Notify::new(),
            release: tokio::sync::Notify::new(),
            usage_reported,
            inner: FakeProvider {
                responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
                requests: Mutex::new(Vec::new()),
            },
        });
        let steer = Arc::new(TestSteer {
            text: Mutex::new(Vec::new()),
            wake: tokio::sync::Notify::new(),
        });
        let prices = Some((1.0, 1.0));
        let agent = Agent::new(
            provider.clone(),
            Arc::new(tools(dir.path(), Mode::Auto)),
            Limits {
                prices,
                budget: Some(budget),
                ..limits()
            },
        )
        .unwrap()
        .with_steer(steer.clone());
        let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), prices);
        let (tx, mut rx) = mpsc::channel(64);
        let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
        let (result, _) = tokio::join!(
            agent.run_in(
                &mut session,
                Some("task".into()),
                &store,
                tx,
                CancellationToken::new()
            ),
            async {
                provider.started.notified().await;
                steer.push("change the task");
            }
        );
        drain.await.unwrap();
        assert!(result.is_ok());
        let saved = store.load(&session.id).unwrap();
        assert_eq!(saved.accounting.cost, Some(0.000015));
        assert_eq!(saved.accounting.model_calls, 2);
        assert!(matches!(saved.state, State::Completed));
        assert!(saved.messages.iter().any(|message| {
            message
                .blocks
                .iter()
                .any(|block| matches!(block, Block::Text(text) if text.contains("change the task")))
        }));
        assert_eq!(provider.inner.requests.lock().unwrap().len(), 1);
        assert!(!dir.path().join("obsolete.txt").exists());
    }
}

#[tokio::test]
async fn steering_empty_wakeup_counts_interrupted_request() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(HangOnce {
        first: true.into(),
        inner: FakeProvider {
            responses: Mutex::new(VecDeque::new()),
            requests: Mutex::new(Vec::new()),
        },
    });
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    let prices = Some((1.0, 1.0));
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        Limits {
            prices,
            max_turns: 1,
            ..limits()
        },
    )
    .unwrap()
    .with_steer(steer.clone());
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), prices);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let (result, _) = tokio::join!(
        agent.run_in(
            &mut session,
            Some("write a function".into()),
            &Ephemeral,
            tx,
            CancellationToken::new()
        ),
        async {
            tokio::time::sleep(Duration::from_millis(300)).await;
            steer.wake.notify_one();
        }
    );
    drain.await.unwrap();
    assert!(matches!(result, Err(Error::Limit(_))));
    assert_eq!(session.accounting.model_calls, 2);
    assert_eq!(session.accounting.cost, Some(0.0));
    let requests = provider.inner.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].tools.is_empty());
    assert!(requests[0].messages.iter().any(|message| {
        message.blocks.iter().any(
            |block| matches!(block, Block::Text(text) if text.contains("model call limit reached")),
        )
    }));
}

impl Provider for FakeProvider {
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

fn limits() -> Limits {
    Limits {
        max_turns: 3,
        max_output_tokens: 100,
        budget: None,
        prices: None,
    }
}

#[tokio::test]
async fn cancelled_shell_kills_its_spawned_child_before_late_writes() {
    let dir = tempfile::tempdir().unwrap();
    let script = if cfg!(windows) {
        std::fs::write(dir.path().join("child.ps1"),
                "Set-Content -LiteralPath child-ready -Value $PID; Start-Sleep -Seconds 3; Set-Content -LiteralPath leaked -Value bad").unwrap();
        "Start-Process -FilePath powershell.exe -ArgumentList @('-NoProfile','-NonInteractive','-File','child.ps1') -WorkingDirectory (Get-Location).Path; Start-Sleep -Seconds 20"
    } else {
        "(echo ready > child-ready; sleep 3; echo bad > leaked) & sleep 20"
    };
    let cancel = CancellationToken::new();
    let work_root = dir.path().to_path_buf();
    let work_cancel = cancel.clone();
    let task = tokio::spawn(async move {
        process::shell(&work_root, script, Duration::from_secs(15), work_cancel).await
    });
    let ready = tokio::time::timeout(Duration::from_secs(10), async {
        while !dir.path().join("child-ready").exists() {
            tokio::time::sleep(Duration::from_millis(25)).await;
        }
    })
    .await;
    cancel.cancel();
    let result = tokio::time::timeout(Duration::from_secs(6), task)
        .await
        .unwrap()
        .unwrap();
    assert!(
        ready.is_ok(),
        "child did not start before the test deadline"
    );
    assert!(matches!(result, Err(Error::Cancelled)));
    tokio::time::sleep(Duration::from_secs(4)).await;
    assert!(
        !dir.path().join("leaked").exists(),
        "child survived cancellation"
    );
}

#[tokio::test]
async fn shell_handles_unicode_script_and_output() {
    let dir = tempfile::tempdir().unwrap();
    let script = if cfg!(windows) {
        "[Console]::Out.Write('中文测试')"
    } else {
        "printf '中文测试'"
    };
    let output = process::shell(
        dir.path(),
        script,
        Duration::from_secs(10),
        CancellationToken::new(),
    )
    .await
    .unwrap();
    assert_eq!(output.code, Some(0));
    assert_eq!(output.stdout, "中文测试");
    assert!(!output.encoding_warning);
}

struct FakeInput;

impl cove::domain::UserInput for FakeInput {
    fn answer(&self, _: String, cancel: CancellationToken) -> BoxFuture<'_, Result<String>> {
        Box::pin(async move {
            if cancel.is_cancelled() {
                Err(Error::Cancelled)
            } else {
                Ok("y".into())
            }
        })
    }
}

#[tokio::test]
async fn explicit_approval_and_user_questions_use_the_input_port() {
    let dir = tempfile::tempdir().unwrap();
    let input: Arc<dyn cove::domain::UserInput> = Arc::new(FakeInput);
    let tools = LocalTools::new(
        dir.path().to_path_buf(),
        Arc::new(LocalAuthorizer {
            mode: Mode::Ask,
            input: Some(input.clone()),
        }),
    )
    .unwrap()
    .with_user_input(Some(input));
    assert!(
        tools
            .definitions()
            .iter()
            .any(|tool| tool.name == "question")
    );
    let out = tools
        .execute(
            "ask_user".into(),
            json!({"question":"Continue?"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert_eq!(out.content, "y");
    let out = tools
        .execute(
            "write".into(),
            json!({"path":"approved","content":"yes"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(!out.failed);
    assert_eq!(
        std::fs::read_to_string(dir.path().join("approved")).unwrap(),
        "yes"
    );
}

#[test]
fn headless_tool_catalogue_does_not_offer_questions() {
    let dir = tempfile::tempdir().unwrap();
    assert!(
        !tools(dir.path(), Mode::Auto)
            .definitions()
            .iter()
            .any(|tool| matches!(tool.name.as_str(), "question" | "ask_user"))
    );
}

#[tokio::test]
async fn budget_threshold_warns_and_allows_write() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            response(vec![Block::ToolUse {
                id: "call".into(),
                name: "write".into(),
                input: json!({"path":"bad","content":"bad"}),
                extra: None,
            }]),
            response(vec![Block::Text("done".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.prices = Some((1.0, 2.0));
    limits.budget = Some(20.0 / 1_000_000.0);
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits).unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    assert!(
        agent
            .run("task".into(), tx, CancellationToken::new())
            .await
            .is_ok()
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("bad")).unwrap(),
        "bad"
    );
    let mut warned = false;
    while let Ok(event) = rx.try_recv() {
        if matches!(event, Event::Notice(text) if text.contains("[预算超限]")) {
            warned = true;
        }
    }
    assert!(warned);
}

#[tokio::test]
async fn closed_event_consumer_prevents_tool_side_effects() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([response(vec![Block::ToolUse {
            id: "call".into(),
            name: "write".into(),
            input: json!({"path":"bad","content":"bad"}),
            extra: None,
        }])])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits()).unwrap();
    let (tx, rx) = mpsc::channel(64);
    drop(rx);
    assert!(matches!(
        agent.run("task".into(), tx, CancellationToken::new()).await,
        Err(Error::ConsumerClosed)
    ));
    assert!(!dir.path().join("bad").exists());
}

#[tokio::test]
async fn agent_executes_real_edit_then_returns_matching_result_to_model() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("code.txt"), "old").unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            response(vec![Block::ToolUse {
                id: "call-1".into(),
                name: "edit".into(),
                input: json!({"path":"code.txt","old":"old","new":"new"}),
                extra: None,
            }]),
            response(vec![Block::Text("changed; checks not run".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits(),
    )
    .unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let report = agent
        .run("change old to new".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.tools_run, 1);
    assert_eq!(report.model_calls, 2);
    assert_eq!(
        std::fs::read_to_string(dir.path().join("code.txt")).unwrap(),
        "new"
    );
    let requests = provider.requests.lock().unwrap();
    assert!(
        matches!(&requests[1].messages[2].blocks[0], Block::ToolResult { id, is_error:false, .. } if id == "call-1")
    );
}

struct Refuse;

impl cove::domain::UserInput for Refuse {
    fn answer(&self, _: String, _: CancellationToken) -> BoxFuture<'_, Result<String>> {
        Box::pin(async { Ok("n 不要写这个文件".into()) })
    }
}

#[tokio::test]
async fn a_refused_call_goes_back_to_the_model_and_the_task_goes_on() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            // Default mode no longer asks for workspace writes; a mutating shell
            // command is what still reaches the prompt and can be refused.
            response(vec![Block::ToolUse {
                id: "call-1".into(),
                name: "shell".into(),
                input: json!({"command":"echo x > refused.txt"}),
                extra: None,
            }]),
            response(vec![Block::Text("好的，不写了".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let input: Arc<dyn cove::domain::UserInput> = Arc::new(Refuse);
    let tools = LocalTools::new(
        dir.path().to_path_buf(),
        Arc::new(LocalAuthorizer {
            mode: Mode::Ask,
            input: Some(input),
        }),
    )
    .unwrap();
    let agent = Agent::new(provider.clone(), Arc::new(tools), limits()).unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let report = agent
        .run("write a file".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.model_calls, 2);
    // A refusal is not a failure of the task.
    assert_eq!(report.tools_failed, 0);
    assert!(!dir.path().join("refused.txt").exists());
    let requests = provider.requests.lock().unwrap();
    assert!(matches!(
        &requests[1].messages[2].blocks[0],
        Block::ToolResult { id, is_error: true, content }
            if id == "call-1" && content.contains("denied") && content.contains("不要写这个文件")
    ));
}

#[tokio::test]
async fn truncated_response_cannot_execute_a_tool() {
    let dir = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::ToolUse {
        id: "call".into(),
        name: "write".into(),
        input: json!({"path":"bad","content":"bad"}),
        extra: None,
    }]);
    result.complete = false;
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits()).unwrap();
    let (tx, _rx) = mpsc::channel(64);
    assert!(
        agent
            .run("task".into(), tx, CancellationToken::new())
            .await
            .is_err()
    );
    assert!(!dir.path().join("bad").exists());
}

#[tokio::test]
async fn a_truncated_reply_is_retried_once_with_a_note_kept_out_of_history() {
    let dir = tempfile::tempdir().unwrap();
    let mut cut = response(vec![Block::Text("partial".into())]);
    cut.complete = false;
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([cut, response(vec![Block::Text("done".into())])])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits(),
    )
    .unwrap();
    let (tx, _rx) = mpsc::channel(64);
    agent
        .run("task".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 2);
    // The cut-off reply is dropped; only the note is added, and only to this request.
    assert_eq!(requests[1].messages.len(), requests[0].messages.len() + 1);
    let note = format!("{:?}", requests[1].messages.last().unwrap());
    assert!(note.contains("output token limit"), "{note}");
}

#[tokio::test]
async fn model_call_limit_stops_without_claiming_completion() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([response(vec![Block::ToolUse {
            id: "call".into(),
            name: "glob".into(),
            input: json!({"pattern":"*"}),
            extra: None,
        }])])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.max_turns = 1;
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits).unwrap();
    let (tx, _rx) = mpsc::channel(64);
    assert!(matches!(
        agent.run("task".into(), tx, CancellationToken::new()).await,
        Err(Error::Limit(_))
    ));
}

#[tokio::test]
async fn missing_usage_counts_as_zero_when_budgeted() {
    let dir = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    result.usage_reported = false;
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.budget = Some(1.0);
    limits.prices = Some((1.0, 2.0));
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits).unwrap();
    let (tx, _rx) = mpsc::channel(64);
    assert!(
        agent
            .run("task".into(), tx, CancellationToken::new())
            .await
            .is_ok()
    );
}

#[tokio::test]
async fn reported_cost_warns_without_static_prices() {
    let dir = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    result.reported_cost = Some(0.5);
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.budget = Some(0.1);
    limits.prices = None;
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits,
    )
    .unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    let report = agent
        .run("task".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(report.estimated_cost, Some(0.5));
    assert!(
        std::iter::from_fn(|| rx.try_recv().ok())
            .any(|event| matches!(event, Event::Notice(text) if text.contains("[预算超限]")))
    );
    assert_eq!(provider.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn reported_cost_takes_precedence_over_estimated_cost() {
    let dir = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    result.reported_cost = Some(0.5);
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.budget = Some(0.1);
    limits.prices = Some((1.0, 1.0));
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits).unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    let report = agent
        .run("task".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(report.estimated_cost, Some(0.5));
    assert!(
        std::iter::from_fn(|| rx.try_recv().ok())
            .any(|event| matches!(event, Event::Notice(text) if text.contains("[预算超限]")))
    );
}

#[tokio::test]
async fn reported_cost_is_recorded_in_usage_ledger() {
    let data = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    result.reported_cost = Some(0.5);
    let inner = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let ledger = Arc::new(cove::session::usage::UsageLedger::new(data.path()));
    let metered = cove::session::usage::Metered {
        inner,
        ledger: ledger.clone(),
        provider: "openrouter".into(),
        model: "model".into(),
        prices: Some((1.0, 1.0)),
        source: "session",
    };
    let request = ModelRequest {
        system: String::new(),
        messages: vec![Message::user("hello")],
        tools: Vec::new(),
        max_output_tokens: 100,
        estimated_tokens: None,
        model: None,
    };
    let (tx, _rx) = mpsc::channel(64);
    metered
        .stream(request, tx, CancellationToken::new())
        .await
        .unwrap();
    let entries = ledger.load(0, u64::MAX).unwrap();
    assert_eq!(entries.len(), 1);
    assert_eq!(entries[0].cost, Some(0.5));
}

#[tokio::test]
async fn missing_reported_cost_counts_as_zero_without_static_prices() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.budget = Some(1.0);
    limits.prices = None;
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits,
    )
    .unwrap();
    let (tx, _rx) = mpsc::channel(64);
    assert!(
        agent
            .run("task".into(), tx, CancellationToken::new())
            .await
            .is_ok()
    );
    assert_eq!(provider.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn reported_cost_accumulates_without_static_prices() {
    let dir = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    result.reported_cost = Some(0.05);
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.budget = Some(0.1);
    limits.prices = None;
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits,
    )
    .unwrap();
    let (result, accounting, _) = agent
        .run_detached("task".into(), CancellationToken::new())
        .await;
    assert!(result.is_ok());
    assert_eq!(accounting.cost, Some(0.05));
    assert_eq!(provider.requests.lock().unwrap().len(), 1);
}
