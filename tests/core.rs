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
async fn noninteractive_default_denies_mutation_and_shell() {
    let dir = tempfile::tempdir().unwrap();
    let tools = tools(dir.path(), Mode::Ask);
    assert!(matches!(
        tools
            .execute(
                "write".into(),
                json!({"path":"file","content":"bad"}),
                CancellationToken::new()
            )
            .await,
        Err(Error::Approval(_))
    ));
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
    assert!(!dir.path().join("file").exists());
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
async fn exact_budget_threshold_stops_before_a_write() {
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
    let mut limits = limits();
    limits.prices = Some((1.0, 2.0));
    limits.budget = Some(20.0 / 1_000_000.0);
    let agent = Agent::new(provider, Arc::new(tools(dir.path(), Mode::Auto)), limits).unwrap();
    let (tx, _rx) = mpsc::channel(64);
    assert!(matches!(
        agent.run("task".into(), tx, CancellationToken::new()).await,
        Err(Error::Limit(_))
    ));
    assert!(!dir.path().join("bad").exists());
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
            response(vec![Block::ToolUse {
                id: "call-1".into(),
                name: "write".into(),
                input: json!({"path":"refused.txt","content":"x"}),
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
async fn missing_usage_cannot_be_treated_as_free_when_budgeted() {
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
    assert!(matches!(
        agent.run("task".into(), tx, CancellationToken::new()).await,
        Err(Error::Limit(_))
    ));
}
