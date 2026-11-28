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
        Err(Error::Unattended(_))
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

#[tokio::test]
async fn search_can_include_ignored_files_but_never_git_or_protected_directories() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    std::fs::write(
        root.join(".gitignore"),
        "ignored.txt\nnode_modules/\n*.log\n",
    )
    .unwrap();
    for (path, text) in [
        ("visible.txt", "needle\n"),
        ("ignored.txt", "needle\n"),
        ("node_modules/pkg/index.js", "needle\n"),
        ("node_modules/pkg/debug.log", "needle\n"),
        (".git/info/notes", "needle\n"),
        (".cove/config.json", "needle\n"),
    ] {
        let path = root.join(path);
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, text).unwrap();
    }
    cove::safety::protected::protect(&root.join(".cove"));
    let mut backends = vec![false];
    if which::which("rg").is_ok() {
        backends.push(true);
    }
    for ripgrep in backends {
        let tools = tools(root, Mode::Readonly).with_ripgrep(ripgrep);
        let run = |name: &str, input: serde_json::Value| {
            let call = tools.execute(name.into(), input, CancellationToken::new());
            async move {
                let out = call.await.unwrap();
                assert!(!out.failed, "{}", out.content);
                out.content
            }
        };
        let never = |out: &str| {
            assert!(!out.contains(".git/"), "ripgrep={ripgrep}: {out}");
            assert!(!out.contains(".cove/"), "ripgrep={ripgrep}: {out}");
        };
        for (name, input) in [
            ("glob", json!({"pattern":"*","include_ignored":true})),
            ("grep", json!({"pattern":"needle","include_ignored":true})),
        ] {
            let out = run(name, input).await;
            for path in [
                "visible.txt",
                "ignored.txt",
                "node_modules/pkg/index.js",
                "node_modules/pkg/debug.log",
            ] {
                assert!(out.contains(path), "ripgrep={ripgrep} {name}: {out}");
            }
            never(&out);
        }
        // By default ignored files stay out, and an empty result names the parameter.
        let out = run("grep", json!({"pattern":"needle"})).await;
        assert!(
            !out.contains("node_modules") && !out.contains("ignored.txt"),
            "{out}"
        );
        never(&out);
        let hint =
            "(files excluded by .gitignore were skipped; include_ignored=true searches them)";
        let out = run("glob", json!({"pattern":"*.js"})).await;
        assert_eq!(out, format!("No files found {hint}"));
        let out = run("grep", json!({"pattern":"needle","include":"*.js"})).await;
        assert_eq!(out, format!("No matches found {hint}"), "ripgrep={ripgrep}");
        // An explicit path below an ignored directory is searched; rules matching files
        // inside it (*.log) still apply unless include_ignored is set.
        let out = run(
            "grep",
            json!({"pattern":"needle","path":"node_modules/pkg"}),
        )
        .await;
        assert_eq!(
            out, "node_modules/pkg/index.js:1:needle",
            "ripgrep={ripgrep}"
        );
        let out = run("glob", json!({"pattern":"*","path":"node_modules/pkg"})).await;
        assert_eq!(out, "node_modules/pkg/index.js");
        let input = json!({"pattern":"needle","path":"node_modules/pkg","include_ignored":true});
        let out = run("grep", input).await;
        assert!(out.contains("node_modules/pkg/debug.log:1:needle"), "{out}");
    }
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
        Limits { prices, ..limits() },
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

/// Passes once, while `text` is typed: a user answering during the end-of-task checks.
struct TypingVerifier {
    steer: Arc<TestSteer>,
    text: Mutex<Option<String>>,
}

impl cove::safety::verify::Verifier for TypingVerifier {
    fn verify(
        &self,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<Option<cove::safety::verify::Verification>>> {
        Box::pin(async move {
            Ok(self.text.lock().unwrap().take().map(|text| {
                self.steer.push(&text);
                cove::safety::verify::Verification {
                    outcome: cove::safety::verify::Outcome::Passed,
                    summary: String::new(),
                    detail: String::new(),
                }
            }))
        })
    }
}

#[tokio::test]
async fn text_typed_during_the_final_checks_stays_in_the_task() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            response(vec![Block::Text("which one?".into())]),
            response(vec![Block::Text("went with the second".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    let verifier = Arc::new(TypingVerifier {
        steer: steer.clone(),
        text: Mutex::new(Some("the second one".into())),
    });
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits(),
    )
    .unwrap()
    .with_steer(steer.clone())
    .with_verifier(Some(verifier));
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let report = agent
        .run("pick one".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.model_calls, 2);
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 2, "the answer is handled in this task");
    assert!(requests[1].messages.iter().any(|message| {
        message.role == Role::User
            && message
                .blocks
                .iter()
                .any(|block| matches!(block, Block::Text(text) if text.contains("the second one")))
    }));
    assert!(cove::domain::SteerSource::take(&*steer).is_none());
}

/// Answers after a short wait, so a wake can arrive while the reply is in flight.
struct Delayed(FakeProvider);

impl Provider for Delayed {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            tokio::time::sleep(Duration::from_millis(200)).await;
            self.0.stream(request, events, cancel).await
        })
    }
}

#[tokio::test]
async fn a_wake_whose_text_was_already_delivered_keeps_the_reply() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(Delayed(FakeProvider {
        responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
        requests: Mutex::new(Vec::new()),
    }));
    let steer = Arc::new(TestSteer {
        text: Mutex::new(Vec::new()),
        wake: tokio::sync::Notify::new(),
    });
    // `!` text typed during a tool run and taken with its result leaves the wake behind.
    steer.wake.notify_one();
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits(),
    )
    .unwrap()
    .with_steer(steer.clone());
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let report = agent
        .run("write a function".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.model_calls, 1, "the reply was not restarted");
    assert_eq!(provider.0.requests.lock().unwrap().len(), 1);
}

#[tokio::test]
async fn steering_with_known_prices_does_not_wait_for_usage() {
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
        Limits { prices, ..limits() },
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
async fn steering_an_empty_wakeup_keeps_the_call_and_typed_text_still_interrupts() {
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
                // A wake whose text was already delivered: the hung call goes on.
                tokio::time::sleep(Duration::from_millis(300)).await;
                steer.wake.notify_one();
                tokio::time::sleep(Duration::from_millis(300)).await;
                steer.push("use iteration");
            }
        )
    })
    .await
    .expect("typed text must still interrupt after an empty wake");
    drain.await.unwrap();
    assert!(result.is_ok());
    // The interrupted call and the answer; the empty wake cost nothing.
    assert_eq!(session.accounting.model_calls, 2);
    let requests = provider.inner.requests.lock().unwrap();
    assert_eq!(requests.len(), 1);
    assert!(requests[0].messages.iter().any(|message| {
        message
            .blocks
            .iter()
            .any(|block| matches!(block, Block::Text(text) if text.contains("use iteration")))
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
            origin: cove::domain::Origin::Unknown,
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
        prices: None,
    }
}

#[tokio::test]
async fn cancelled_shell_kills_its_spawned_child_before_late_writes() {
    let dir = tempfile::tempdir().unwrap();
    let script = if cfg!(windows) {
        std::fs::write(dir.path().join("child.ps1"),
                "Set-Content -LiteralPath child-ready -Value $PID; Start-Sleep -Seconds 3; Set-Content -LiteralPath leaked -Value bad").unwrap();
        "Start-Process -FilePath powershell.exe -ArgumentList @('-NoProfile','-NonInteractive','-File','child.ps1') -WorkingDirectory (Get-Location).Path; Start-Sleep -Seconds 120"
    } else {
        "(echo ready > child-ready; sleep 3; echo bad > leaked) & sleep 120"
    };
    // Two PowerShell starts can take many seconds under a full parallel test run: the
    // limits before cancelling leave room, the checks after it stay tight.
    let cancel = CancellationToken::new();
    let work_root = dir.path().to_path_buf();
    let work_cancel = cancel.clone();
    let task = tokio::spawn(async move {
        process::shell(&work_root, script, Duration::from_secs(120), work_cancel).await
    });
    let ready = tokio::time::timeout(Duration::from_secs(60), async {
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
        responses: Mutex::new(VecDeque::from([
            cut,
            response(vec![Block::Text("done".into())]),
        ])),
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
        Err(Error::Stopped(stopped)) if stopped.stop == cove::engine::agent::Stop::Limit(1)
    ));
}

#[tokio::test]
async fn reported_cost_is_used_without_static_prices() {
    let dir = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    result.reported_cost = Some(0.5);
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
    limits.prices = None;
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        limits,
    )
    .unwrap();
    let (tx, _rx) = mpsc::channel(64);
    let report = agent
        .run("task".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(report.estimated_cost, Some(0.5));
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
    limits.prices = Some((1.0, 1.0));
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits).unwrap();
    let (tx, _rx) = mpsc::channel(64);
    let report = agent
        .run("task".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    assert_eq!(report.estimated_cost, Some(0.5));
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
        pricing: Arc::new(|_: &str| Some(cove::model::models::Prices::from_pair((1.0, 1.0)))),
        source: "session",
    };
    let request = ModelRequest {
        system: String::new(),
        messages: vec![Message::user("hello")],
        tools: Vec::new(),
        max_output_tokens: 100,
        estimated_tokens: None,
        model: None,
        cache: cove::domain::Cache::Short,
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
async fn the_ledger_records_the_model_that_answered_at_its_cache_aware_prices() {
    let data = tempfile::tempdir().unwrap();
    let mut result = response(vec![Block::Text("done".into())]);
    // A background call: the router sent it to the fast model.
    result.model = Some("fast-model".into());
    result.usage_reported = true;
    result.usage = cove::domain::Usage {
        input: 100_065,
        output: 955,
        cache_read: 98_816,
        cache_write: 1_247,
        ..Default::default()
    };
    let inner = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([result])),
        requests: Mutex::new(Vec::new()),
    });
    let ledger = Arc::new(cove::session::usage::UsageLedger::new(data.path()));
    let metered = cove::session::usage::Metered {
        inner,
        ledger: ledger.clone(),
        provider: "bedrock".into(),
        model: "main-model".into(),
        // Only the fast model is priced: recording the configured model would leave no cost.
        pricing: Arc::new(|model: &str| {
            (model == "fast-model").then(|| cove::model::models::Prices::from_pair((4.0, 20.0)))
        }),
        source: "background",
    };
    let request = ModelRequest {
        system: String::new(),
        messages: vec![Message::user("hello")],
        tools: Vec::new(),
        max_output_tokens: 100,
        estimated_tokens: None,
        model: None,
        cache: cove::domain::Cache::Short,
    };
    let (tx, _rx) = mpsc::channel(64);
    metered
        .stream(request, tx, CancellationToken::new())
        .await
        .unwrap();
    let entries = ledger.load(0, u64::MAX).unwrap();
    assert_eq!(entries[0].model, "fast-model");
    // Cache reads at their own rate: about $0.065, not the $0.419 of plain input.
    let cost = entries[0].cost.unwrap();
    assert!((0.06..0.07).contains(&cost), "{cost}");
}

#[tokio::test]
async fn missing_reported_cost_counts_as_zero_without_static_prices() {
    let dir = tempfile::tempdir().unwrap();
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([response(vec![Block::Text("done".into())])])),
        requests: Mutex::new(Vec::new()),
    });
    let mut limits = limits();
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

/// Answers the stall questions in order and records them.
struct StallAnswers {
    answers: Mutex<VecDeque<&'static str>>,
    questions: Mutex<Vec<String>>,
}

impl cove::domain::UserInput for StallAnswers {
    fn answer(&self, question: String, _: CancellationToken) -> BoxFuture<'_, Result<String>> {
        self.questions.lock().unwrap().push(question);
        let answer = self.answers.lock().unwrap().pop_front().unwrap_or("");
        Box::pin(async move { Ok(answer.into()) })
    }
}

#[tokio::test]
async fn a_stalled_task_asks_and_each_choice_does_what_it_says() {
    let dir = tempfile::tempdir().unwrap();
    let failing = || {
        response(vec![Block::ToolUse {
            id: cove::session::new_id(),
            name: "read".into(),
            input: json!({"path":"missing.txt"}),
            extra: None,
        }])
    };
    // stall_rounds 2: round 1 is new information, rounds 2-3 stall (asked: 3 = go on,
    // now 4 rounds), rounds 4-7 stall (2 = another approach), 8-11 (typed text),
    // 12-15 (Enter = stop and summarize).
    let mut responses: VecDeque<ModelResponse> = (0..15).map(|_| failing()).collect();
    responses.push_back(response(vec![Block::Text(
        "missing.txt does not exist; nothing to read.".into(),
    )]));
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(responses),
        requests: Mutex::new(Vec::new()),
    });
    let input = Arc::new(StallAnswers {
        answers: Mutex::new(VecDeque::from(["3", "2", "look elsewhere", ""])),
        questions: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Auto)),
        Limits {
            max_turns: 50,
            ..limits()
        },
    )
    .unwrap()
    .with_stall_rounds(2)
    .with_user_input(Some(input.clone()));
    let mut session = Session::new(dir.path().into(), "Openai".into(), "mock".into(), None);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move {
        let mut notices = Vec::new();
        while let Some(event) = rx.recv().await {
            if let Event::Notice(text) = event {
                notices.push(text);
            }
        }
        notices
    });
    let result = agent
        .run_in(
            &mut session,
            Some("read missing.txt".into()),
            &Ephemeral,
            tx,
            CancellationToken::new(),
        )
        .await;
    let notices = drain.await.unwrap();
    let Err(Error::Stopped(stopped)) = result else {
        panic!("{result:?}");
    };
    // Each choice says what it did: 3, 2 and Enter.
    for said in ["照原样继续", "换个思路再试", "停下并总结"] {
        assert!(
            notices.iter().any(|notice| notice.contains(said)),
            "{said}: {notices:?}"
        );
    }
    assert_eq!(stopped.stop, cove::engine::agent::Stop::Stalled(4));
    assert_eq!(stopped.report.idle_max, 4);
    assert!(matches!(session.state, State::Interrupted(_)));
    let questions = input.questions.lock().unwrap();
    assert_eq!(questions.len(), 4, "{questions:?}");
    assert!(
        questions[0].contains("[没有进展] 最近 2 轮"),
        "{}",
        questions[0]
    );
    assert!(
        questions[0].contains("反复出现：read `missing.txt`"),
        "{}",
        questions[0]
    );
    assert!(
        questions[1].contains("最近 4 轮"),
        "doubled: {}",
        questions[1]
    );
    // Counted in those 4 rounds, not the 7 of the whole run.
    assert!(questions[1].contains("×4"), "{}", questions[1]);
    let requests = provider.requests.lock().unwrap();
    assert_eq!(requests.len(), 16);
    let last = requests.last().unwrap();
    assert!(last.tools.is_empty());
    let text = |request: &ModelRequest| format!("{:?}", request.messages);
    assert!(
        text(last).contains("cove stopped the task"),
        "{}",
        text(last)
    );
    assert!(text(last).contains("Repeated errors"));
    // 2: the model hears what was tried; typed text reaches it as steering.
    assert!(text(&requests[7]).contains("asked for a different approach"));
    assert!(!text(&requests[6]).contains("asked for a different approach"));
    assert!(
        session
            .messages
            .iter()
            .any(|message| message.origin == cove::domain::Origin::Steer
                && format!("{:?}", message.blocks).contains("look elsewhere"))
    );
}

/// A shell command for the platform's routed shell: PowerShell syntax on Windows.
fn platform(windows: &str, unix: &str) -> String {
    if cfg!(windows) { windows } else { unix }.to_owned()
}

fn shell_call(id: &str, input: serde_json::Value) -> ModelResponse {
    response(vec![Block::ToolUse {
        id: id.into(),
        name: "shell".into(),
        input,
        extra: None,
    }])
}

fn request_text(request: &ModelRequest) -> String {
    request
        .messages
        .iter()
        .flat_map(|message| &message.blocks)
        .filter_map(|block| match block {
            Block::Text(text) => Some(text.clone()),
            Block::ToolResult { content, .. } => Some(content.clone()),
            _ => None,
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// Background ends the turn hooks heard: command and whether it failed.
#[derive(Default)]
struct Heard(Mutex<Vec<(String, bool)>>);

impl cove::engine::turn::TurnHook for Heard {
    fn background_finished(&self, command: &str, output: &cove::tools::ToolOutput) {
        self.0
            .lock()
            .unwrap()
            .push((command.to_owned(), output.failed));
    }
}

/// Records whether the command's last write had happened when the checks ran.
struct SeesFile(std::path::PathBuf, Mutex<Vec<bool>>);

impl cove::safety::verify::Verifier for SeesFile {
    fn verify(
        &self,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<Option<cove::safety::verify::Verification>>> {
        self.1.lock().unwrap().push(self.0.exists());
        Box::pin(async { Ok(None) })
    }
}

#[tokio::test]
async fn a_command_moved_to_the_background_is_awaited_and_its_result_goes_to_the_model() {
    let dir = tempfile::tempdir().unwrap();
    let command = platform(
        "Write-Output first; Start-Sleep -Seconds 3; Write-Output second; Set-Content -LiteralPath done.txt -Value x",
        "echo first; sleep 3; echo second; echo x > done.txt",
    );
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            shell_call("call-1", json!({"command": command, "timeout_seconds": 1})),
            // Ends the reply while the command still runs: the task waits for it.
            response(vec![Block::Text("waiting for it".into())]),
            response(vec![Block::Text("it printed second".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let progress = Arc::new(cove::domain::Progress::default());
    let agent = Agent::new(
        provider.clone(),
        Arc::new(tools(dir.path(), Mode::Bypass).with_progress(progress.clone())),
        limits(),
    )
    .unwrap();
    let checks = Arc::new(SeesFile(
        dir.path().join("done.txt"),
        Mutex::new(Vec::new()),
    ));
    let heard = Arc::new(Heard::default());
    let agent = agent
        .with_verifier(Some(checks.clone()))
        .with_turn_hook(heard.clone());
    let (tx, mut rx) = mpsc::channel(64);
    progress.attach(Some(tx.clone()));
    let drain = tokio::spawn(async move {
        let mut seen = Vec::new();
        while let Some(event) = rx.recv().await {
            match event {
                Event::Notice(text) | Event::Activity(text) => seen.push(text),
                _ => {}
            }
        }
        seen
    });
    let report = agent
        .run("count it".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    progress.attach(None);
    let seen = drain.await.unwrap();
    assert_eq!(report.model_calls, 3);
    // The checks ran once, at the end, after the command had finished.
    assert_eq!(*checks.1.lock().unwrap(), vec![true]);
    // The checks' evidence hears the end as it hears a foreground result.
    assert_eq!(*heard.0.lock().unwrap(), vec![(command.clone(), false)]);
    let requests = provider.requests.lock().unwrap();
    let moved = request_text(&requests[1]);
    assert!(
        moved.contains("[background #1] Still running after 1s"),
        "{moved}"
    );
    let delivered = request_text(&requests[2]);
    assert!(
        delivered.contains("[background command #1 finished"),
        "{delivered}"
    );
    assert!(delivered.contains("result: exit code 0"), "{delivered}");
    assert!(delivered.contains("second"), "{delivered}");
    assert!(
        seen.iter()
            .any(|text| text.starts_with("命令运行超过 1s，已转到后台继续（#1）")),
        "{seen:?}"
    );
    assert!(
        seen.iter().any(|text| text.starts_with("等待后台命令 · ")),
        "{seen:?}"
    );
    assert!(
        seen.iter()
            .any(|text| text.starts_with("[后台] #1 已结束：退出码 0")),
        "{seen:?}"
    );
}

#[tokio::test]
async fn cancelling_the_wait_for_a_background_command_kills_it() {
    let dir = tempfile::tempdir().unwrap();
    let command = platform(
        "Start-Sleep -Seconds 5; Set-Content -LiteralPath late.txt -Value x",
        "sleep 5; echo x > late.txt",
    );
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            shell_call("call-1", json!({"command": command, "timeout_seconds": 1})),
            response(vec![Block::Text("waiting".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(
        provider,
        Arc::new(tools(dir.path(), Mode::Bypass)),
        limits(),
    )
    .unwrap();
    let cancel = CancellationToken::new();
    let (tx, mut rx) = mpsc::channel(64);
    let stop = cancel.clone();
    let drain = tokio::spawn(async move {
        while let Some(event) = rx.recv().await {
            if matches!(&event, Event::Activity(text) if text.starts_with("等待后台命令")) {
                stop.cancel();
            }
        }
    });
    let result = tokio::time::timeout(
        Duration::from_secs(30),
        agent.run("wait".into(), tx, cancel),
    )
    .await
    .expect("the wait was not cancelled");
    let _ = drain.await;
    assert!(matches!(result, Err(Error::Cancelled)), "{result:?}");
    tokio::time::sleep(Duration::from_secs(6)).await;
    assert!(
        !dir.path().join("late.txt").exists(),
        "the background command survived the cancel"
    );
}

#[tokio::test]
async fn a_background_start_is_not_awaited_and_shell_task_stops_it() {
    let dir = tempfile::tempdir().unwrap();
    let command = platform(
        "Write-Output listening; Start-Sleep -Seconds 30",
        "echo listening; sleep 30",
    );
    let jobs = Arc::new(cove::tools::jobs::Jobs::new(Some(dir.path().join("logs"))));
    let tools = tools(dir.path(), Mode::Bypass).with_jobs(jobs.clone());
    let provider = Arc::new(FakeProvider {
        responses: Mutex::new(VecDeque::from([
            shell_call(
                "call-1",
                json!({"command": command, "run_in_background": true}),
            ),
            response(vec![Block::Text("server started".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let agent = Agent::new(provider.clone(), Arc::new(tools.clone()), limits()).unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let started = std::time::Instant::now();
    let report = agent
        .run("start the server".into(), tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.model_calls, 2);
    assert!(started.elapsed() < Duration::from_secs(15));
    let result = request_text(&provider.requests.lock().unwrap()[1]);
    assert!(
        result.contains("[background #1] Started in the background"),
        "{result}"
    );
    // Still running after the task; `shell_task` sees and stops it.
    assert_eq!(jobs.running(), 1);
    let list = tools
        .execute(
            "shell_task".into(),
            json!({"action":"list"}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(
        list.content.contains("#1 [run_in_background] running for"),
        "{}",
        list.content
    );
    let stopped = tools
        .execute(
            "shell_task".into(),
            json!({"action":"stop","id":1}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(!stopped.failed && stopped.content.contains("stopped with shell_task stop"));
    assert_eq!(jobs.running(), 0);
    let missing = tools
        .execute(
            "shell_task".into(),
            json!({"action":"output","id":7}),
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(missing.failed);
}
