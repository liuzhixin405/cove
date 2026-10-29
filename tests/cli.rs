mod common;
use common::Launch;
use std::time::Duration;

use serde_json::{Value, json};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    process::Command,
};

async fn mock_server(responses: Vec<String>) -> (String, tokio::task::JoinHandle<Vec<Value>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let mut requests = Vec::new();
        for response in responses {
            let (mut socket, _) = tokio::time::timeout(Duration::from_secs(20), listener.accept())
                .await
                .unwrap()
                .unwrap();
            let mut request = Vec::new();
            let body_start;
            loop {
                let mut chunk = [0_u8; 4096];
                let size = socket.read(&mut chunk).await.unwrap();
                assert!(size > 0);
                request.extend_from_slice(&chunk[..size]);
                assert!(request.len() < 1024 * 1024);
                if let Some(end) = request.windows(4).position(|part| part == b"\r\n\r\n") {
                    body_start = end + 4;
                    break;
                }
            }
            let headers = std::str::from_utf8(&request[..body_start]).unwrap();
            let length: usize = headers
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    name.eq_ignore_ascii_case("content-length")
                        .then(|| value.trim().parse().unwrap())
                })
                .unwrap();
            while request.len() < body_start + length {
                let mut chunk = [0_u8; 4096];
                let size = socket.read(&mut chunk).await.unwrap();
                assert!(size > 0);
                request.extend_from_slice(&chunk[..size]);
            }
            requests
                .push(serde_json::from_slice(&request[body_start..body_start + length]).unwrap());
            let header = format!(
                "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                response.len()
            );
            socket.write_all(header.as_bytes()).await.unwrap();
            // Exercise network framing independently of JSON/SSE boundaries.
            for chunk in response.as_bytes().chunks(7) {
                socket.write_all(chunk).await.unwrap();
            }
            socket.shutdown().await.unwrap();
        }
        requests
    });
    (format!("http://{address}/v1"), task)
}

fn openai(delta: Value, reason: &str) -> String {
    format!(
        "data: {}\r\n\r\ndata: {}\n\ndata: [DONE]\n\n",
        json!({"choices":[{"delta":delta,"finish_reason":reason}]}),
        json!({"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10}})
    )
}

fn tool(id: &str, name: &str, arguments: Value) -> String {
    openai(
        json!({"tool_calls":[{"index":0,"id":id,"type":"function","function":{"name":name,"arguments":arguments.to_string()}}]}),
        "tool_calls",
    )
}

struct Invocation {
    command: Command,
    _data: tempfile::TempDir,
}

impl Invocation {
    async fn output(mut self) -> std::io::Result<std::process::Output> {
        self.command.launch().await
    }
}

fn cli(root: &std::path::Path, url: &str, protocol: &str) -> Invocation {
    let data = tempfile::tempdir().unwrap();
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        protocol,
        "--model",
        "mock-model",
        "--base-url",
        url,
        "--permission",
        "bypass",
        "--timeout",
        "20",
        "-p",
        "Fix the file and verify it",
    ]);
    command.arg("--cwd").arg(root);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.env("COVE_DATA_DIR", data.path());
    command.kill_on_drop(true);
    Invocation {
        command,
        _data: data,
    }
}

fn persistent_cli(root: &std::path::Path, data: &std::path::Path, url: &str) -> Command {
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        "openai",
        "--model",
        "mock-model",
        "--base-url",
        url,
        "--permission",
        "bypass",
        "--timeout",
        "20",
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.kill_on_drop(true);
    command
}

fn saved_id(data: &std::path::Path) -> String {
    let entry = std::fs::read_dir(data.join("sessions"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap();
    entry.path().file_stem().unwrap().to_str().unwrap().into()
}

#[tokio::test]
async fn piped_repl_keeps_history_and_can_create_and_list_sessions() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        openai(json!({"content":"first answer"}), "stop"),
        openai(json!({"content":"second answer"}), "stop"),
    ])
    .await;
    let mut command = persistent_cli(root.path(), data.path(), &url);
    command
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped());
    let mut child = command.spawn().unwrap();
    let mut stdin = child.stdin.take().unwrap();
    stdin
        .write_all(b"first task\nsecond task\n/status\n/new\n/status\n/resume\n/exit\n")
        .await
        .unwrap();
    drop(stdin);
    let output = tokio::time::timeout(Duration::from_secs(25), child.wait_with_output())
        .await
        .unwrap()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let text = String::from_utf8_lossy(&output.stdout);
    assert!(
        text.contains("first answer")
            && text.contains("second answer")
            && text.contains("模型调用 2 次")
    );
    assert_eq!(
        std::fs::read_dir(data.path().join("sessions"))
            .unwrap()
            .count(),
        2
    );
    let requests = server.await.unwrap();
    assert!(
        requests[1]["messages"].as_array().unwrap().len()
            > requests[0]["messages"].as_array().unwrap().len()
    );
}

#[tokio::test]
async fn a_new_process_continues_without_repeating_a_finished_write() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "create-once",
            "write",
            json!({"path":"once","content":"saved"}),
        ),
        // Tool-free wrap-up summary when the one-call window is used up.
        openai(
            json!({"content":"Created once; nothing verified yet."}),
            "stop",
        ),
        openai(
            json!({"content":"File already created; no checks run."}),
            "stop",
        ),
    ])
    .await;
    let first = persistent_cli(root.path(), data.path(), &url)
        .args(["--max-turns", "1", "-p", "create once"])
        .launch()
        .await
        .unwrap();
    assert!(!first.status.success());
    let id = saved_id(data.path());
    let resumed = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &id, "--continue"])
        .launch()
        .await
        .unwrap();
    assert!(
        resumed.status.success(),
        "{}",
        String::from_utf8_lossy(&resumed.stderr)
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("once")).unwrap(),
        "saved"
    );
    let requests = server.await.unwrap();
    assert!(
        requests[1]["messages"]
            .as_array()
            .unwrap()
            .iter()
            .any(|message| message["tool_call_id"] == "create-once")
    );
}

#[tokio::test]
async fn resumed_diff_and_undo_preview_are_read_only_until_confirmation() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("file"), "before\r\n").unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "edit",
            "edit",
            json!({"path":"file","old":"before","new":"after"}),
        ),
        openai(json!({"content":"Edited; checks not run."}), "stop"),
    ])
    .await;
    assert!(
        persistent_cli(root.path(), data.path(), &url)
            .args(["-p", "edit file"])
            .launch()
            .await
            .unwrap()
            .status
            .success()
    );
    server.await.unwrap();
    let id = saved_id(data.path());
    for operation in ["/diff", "/undo"] {
        let output = persistent_cli(root.path(), data.path(), &url)
            .args(["--resume", &id, "-p", operation])
            .launch()
            .await
            .unwrap();
        assert!(
            output.status.success(),
            "{}",
            String::from_utf8_lossy(&output.stderr)
        );
        assert_eq!(
            std::fs::read_to_string(root.path().join("file")).unwrap(),
            "after\r\n"
        );
    }
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &id, "-p", "/undo confirm"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("file")).unwrap(),
        "before\r\n"
    );
}

#[tokio::test]
async fn unknown_command_and_resume_identity_mismatch_fail_without_model_requests() {
    let root = tempfile::tempdir().unwrap();
    let other = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let output = persistent_cli(root.path(), data.path(), "http://127.0.0.1:9/v1")
        .args(["-p", "/does-not-exist"])
        .launch()
        .await
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("未知命令"));
    let id = saved_id(data.path());
    let output = persistent_cli(other.path(), data.path(), "http://127.0.0.1:9/v1")
        .args(["--resume", &id, "-p", "/status"])
        .launch()
        .await
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("属于另一个工作区"));
}

#[tokio::test]
async fn explicit_uncertain_resolution_never_fabricates_success_or_replays_the_call() {
    use cove::{
        domain::{Block, Message, Role},
        session::workspace::Workspace,
        session::{CallState, PendingCall, Session, SessionPersistence, State, Store},
    };
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Workspace::new(root.path().into(), store.clone()).unwrap();
    let checkpoint = workspace.begin().unwrap();
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock-model".into(),
        None,
    );
    session.current_checkpoint = Some(checkpoint.clone());
    session.checkpoints.push(checkpoint);
    session.current_request = "create once".into();
    session.state = State::Running;
    let input = json!({"path":"once","content":"value"});
    session.messages = vec![
        Message::user("create once"),
        Message {
            role: Role::Assistant,
            blocks: vec![Block::ToolUse {
                id: "uncertain".into(),
                name: "write".into(),
                input: input.clone(),
                extra: None,
            }],
        },
    ]
    .into();
    session.pending.push(PendingCall {
        id: "uncertain".into(),
        name: "write".into(),
        input,
        state: CallState::Executing,
        result: None,
    });
    store.save(&session).unwrap();
    let (url, server) = mock_server(vec![openai(
        json!({"content":"No original evidence was recovered."}),
        "stop",
    )])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &session.id, "--continue"])
        .launch()
        .await
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("其效果未知"));
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &session.id, "-p", "/resolve uncertain executed"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &session.id, "--continue"])
        .launch()
        .await
        .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("有 1 个工具调用失败"));
    assert!(!root.path().join("once").exists());
    let requests = server.await.unwrap();
    assert!(
        requests[0]["messages"]
            .as_array()
            .unwrap()
            .iter()
            .any(|message| message["tool_call_id"] == "uncertain"
                && message["content"]
                    .as_str()
                    .unwrap()
                    .contains("NOT verification evidence"))
    );
    assert!(store.load(&session.id).unwrap().pending.is_empty());
}

#[tokio::test]
async fn openai_cli_reads_edits_runs_real_check_and_returns_evidence() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("code.txt"), "old\r\n").unwrap();
    // The CLI detects the same shell as this test process.
    let powershell =
        cove::process::Shell::current().kind == cove::process::ShellKind::PowerShell;
    let check = if powershell {
        "if ((Get-Content -Raw code.txt).Trim() -ne 'new') { exit 1 }; Write-Output 'check passed'"
    } else {
        "test \"$(tr -d '\\r\\n' < code.txt)\" = new && echo 'check passed'"
    };
    let (url, server) = mock_server(vec![
        tool("read-1", "read", json!({"path":"code.txt"})),
        tool(
            "edit-1",
            "edit",
            json!({"path":"code.txt","old":"old","new":"new"}),
        ),
        tool("check-1", "shell", json!({"command":check})),
        openai(
            json!({"content":"Changed the file; the actual shell check passed."}),
            "stop",
        ),
    ])
    .await;
    let output = tokio::time::timeout(
        Duration::from_secs(25),
        cli(dir.path(), &url, "openai").output(),
    )
    .await
    .unwrap()
    .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        std::fs::read(dir.path().join("code.txt")).unwrap(),
        b"new\r\n"
    );
    assert!(String::from_utf8_lossy(&output.stderr).contains("check passed"));
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 4);
    let messages = requests[3]["messages"].as_array().unwrap();
    assert!(messages.iter().any(|message| {
        message["tool_call_id"] == "check-1"
            && message["content"]
                .as_str()
                .unwrap()
                .contains("exit_code=Some(0)")
    }));
}

#[tokio::test]
async fn skills_and_memory_use_the_same_model_tool_result_path() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let skills = root.path().join("skills");
    std::fs::create_dir(&skills).unwrap();
    std::fs::write(
        skills.join("SKILL.md"),
        "---\nname: coding\ndescription: inspect first\n---\nDo not overwrite user edits.\n",
    )
    .unwrap();
    let config = root.path().join("config.json");
    std::fs::write(
        &config,
        json!({"extensions":{"skills":["skills"]}}).to_string(),
    )
    .unwrap();
    let (url,server) = mock_server(vec![
        tool("list","skill",json!({"action":"list"})),
        tool("load","skill",json!({"action":"load","name":"coding"})),
        tool("add","memory",json!({"action":"add","scope":"project","title":"verify","content":"cargo test","source":"user instruction"})),
        tool("search","memory",json!({"action":"search","scope":"project","query":"cargo"})),
        openai(json!({"content":"Instructions loaded and sourced note stored; no checks claimed."}),"stop"),
    ]).await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .arg("--config")
        .arg(config)
        .args(["-p", "use coding guidance"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.await.unwrap();
    assert!(
        !requests[1]
            .to_string()
            .contains("Do not overwrite user edits.")
    );
    assert!(
        requests[2]
            .to_string()
            .contains("Do not overwrite user edits.")
    );
    assert!(requests[4].to_string().contains("user instruction"));
}

#[tokio::test]
async fn all_hook_events_run_with_scrubbed_environment_and_session_end_once() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let events = [
        "session_start",
        "task_start",
        "before_tool",
        "after_tool",
        "task_end",
        "session_end",
    ];
    let hooks = events.iter().map(|event| {
        let command = if cfg!(windows) {
            format!("if ($env:COVE_API_KEY) {{ exit 8 }}; $data=ConvertFrom-Json $env:COVE_HOOK_INPUT; if ($data.event -ne '{event}') {{ exit 9 }}; Add-Content -LiteralPath hook-events.txt -Value '{event}'; [Console]::Out.Write('{{\"decision\":\"allow\"}}')")
        } else {
            format!("test -z \"$COVE_API_KEY\" || exit 8; printf '%s\\n' '{event}' >> hook-events.txt; printf '%s' '{{\"decision\":\"allow\"}}'")
        };
        json!({"name":event,"event":event,"command":command,"timeout_seconds":5})
    }).collect::<Vec<_>>();
    let config = root.path().join("hooks.json");
    std::fs::write(&config, json!({"extensions":{"hooks":hooks}}).to_string()).unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "write",
            "write",
            json!({"path":"file","content":"actual change"}),
        ),
        openai(
            json!({"content":"File written; verification not run."}),
            "stop",
        ),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .arg("--config")
        .arg(config)
        .args(["--trust-extensions", "-p", "write file"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    server.await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("hook-events.txt"))
            .unwrap()
            .lines()
            .collect::<Vec<_>>(),
        events
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("file")).unwrap(),
        "actual change"
    );
    let id = saved_id(data.path());
    let session = cove::session::Store::open(data.path())
        .unwrap()
        .load(&id)
        .unwrap();
    assert_eq!(session.hook_runs.len(), 6);
    assert!(
        session
            .hook_runs
            .iter()
            .all(|run| run.state == cove::extensions::hooks::HookState::Completed)
    );
}

#[tokio::test]
async fn plugin_registration_enable_disable_uninstall_preserve_source_files() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let plugin = root.path().join("plugin");
    std::fs::create_dir(&plugin).unwrap();
    std::fs::write(
        plugin.join("SKILL.md"),
        "---\nname: coding\ndescription: plugin skill\n---\nPlugin guidance.\n",
    )
    .unwrap();
    let manifest = plugin.join("plugin.json");
    std::fs::write(
        &manifest,
        json!({"version":1,"name":"local","skills":["."],
        "hooks":[{"name":"never","event":"task_start","command":"must-not-run"}]})
        .to_string(),
    )
    .unwrap();
    for command in [
        format!("/config plugins install {}", manifest.display()),
        "/config plugins disable local".into(),
        "/config skills".into(),
        "/config plugins enable local".into(),
        "/config skills".into(),
        "/config plugins uninstall local".into(),
        "/config skills".into(),
    ] {
        let output = persistent_cli(root.path(), data.path(), "http://127.0.0.1:9/v1")
            .args(["-p", &command])
            .launch()
            .await
            .unwrap();
        assert!(
            output.status.success(),
            "command={command}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        if command == "/config plugins enable local" {
            assert!(String::from_utf8_lossy(&output.stdout).contains("enable"));
        }
    }
    assert!(manifest.exists());
    assert!(plugin.join("SKILL.md").exists());
    assert!(
        cove::extensions::Registry::open(data.path())
            .unwrap()
            .snapshot()
            .unwrap()
            .installed
            .is_empty()
    );
}

#[tokio::test]
async fn hook_acknowledgement_is_explicit_and_continue_does_not_replay_task_start() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let command = if cfg!(windows) {
        "[Console]::Out.Write('{\"decision\":\"deny\",\"message\":\"manual check required\"}')"
    } else {
        "printf '%s' '{\"decision\":\"deny\",\"message\":\"manual check required\"}'"
    };
    let config = root.path().join("hooks.json");
    std::fs::write(&config,json!({"extensions":{"hooks":[{"name":"gate","event":"task_start","command":command,"timeout_seconds":5}]}}).to_string()).unwrap();
    let (url, server) = mock_server(vec![openai(
        json!({"content":"Task continued only after operator acknowledgement."}),
        "stop",
    )])
    .await;
    let first = persistent_cli(root.path(), data.path(), &url)
        .arg("--config")
        .arg(&config)
        .args(["--trust-extensions", "-p", "task"])
        .launch()
        .await
        .unwrap();
    assert!(!first.status.success());
    let id = saved_id(data.path());
    let store = cove::session::Store::open(data.path()).unwrap();
    let session = store.load(&id).unwrap();
    assert_eq!(session.accounting.model_calls, 0);
    let run = &session.hook_runs[0].id;
    let ack = persistent_cli(root.path(), data.path(), &url)
        .arg("--config")
        .arg(&config)
        .args([
            "--resume",
            &id,
            "-p",
            &format!("/config hooks resolve {run}"),
        ])
        .launch()
        .await
        .unwrap();
    assert!(
        ack.status.success(),
        "{}",
        String::from_utf8_lossy(&ack.stderr)
    );
    let resumed = persistent_cli(root.path(), data.path(), &url)
        .arg("--config")
        .arg(&config)
        .args(["--trust-extensions", "--resume", &id, "--continue"])
        .launch()
        .await
        .unwrap();
    assert!(
        resumed.status.success(),
        "{}",
        String::from_utf8_lossy(&resumed.stderr)
    );
    assert_eq!(store.load(&id).unwrap().hook_runs.len(), 1);
    assert_eq!(server.await.unwrap().len(), 1);
}

#[tokio::test]
async fn anthropic_cli_roundtrips_thinking_and_tool_results() {
    let dir = tempfile::tempdir().unwrap();
    let frames = [
        json!({"type":"message_start","message":{"usage":{"input_tokens":10}}}),
        json!({"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}),
        json!({"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}),
        json!({"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}),
        json!({"type":"content_block_stop","index":0}),
        json!({"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"write-1","name":"write","input":{}}}),
        json!({"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"new.txt\",\"content\":\"中文\"}"}}),
        json!({"type":"content_block_stop","index":1}),
        json!({"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":8}}),
        json!({"type":"message_stop"}),
    ];
    let end = [
        json!({"type":"message_start","message":{"usage":{"input_tokens":20}}}),
        json!({"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}),
        json!({"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Created; tests not run."}}),
        json!({"type":"content_block_stop","index":0}),
        json!({"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}),
        json!({"type":"message_stop"}),
    ];
    let sse = |values: &[Value]| {
        values
            .iter()
            .map(|v| format!("data: {v}\n\n"))
            .collect::<String>()
    };
    let (url, server) = mock_server(vec![sse(&frames), sse(&end)]).await;
    let output = cli(dir.path(), &url, "anthropic").output().await.unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("new.txt")).unwrap(),
        "中文"
    );
    let requests = server.await.unwrap();
    assert_eq!(
        requests[1]["messages"][1]["content"][0]["signature"],
        "opaque-signature"
    );
    assert_eq!(
        requests[1]["messages"][2]["content"][0]["tool_use_id"],
        "write-1"
    );
}

#[tokio::test]
async fn headless_default_permission_denies_a_real_model_write() {
    let dir = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![tool(
        "write-1",
        "write",
        json!({"path":"bad","content":"bad"}),
    )])
    .await;
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        "openai",
        "--model",
        "mock",
        "--base-url",
        &url,
        "-p",
        "write a file",
    ]);
    command
        .arg("--cwd")
        .arg(dir.path())
        .env("COVE_API_KEY", "test-key");
    command.env("COVE_DATA_DIR", data.path());
    let output = command.launch().await.unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("需要批准"));
    assert!(!dir.path().join("bad").exists());
    assert_eq!(server.await.unwrap().len(), 1);
}

#[tokio::test]
async fn stream_without_terminal_event_cannot_dispatch_tools() {
    let dir = tempfile::tempdir().unwrap();
    let frame = format!(
        "data: {}\n\n",
        json!({"choices":[{"delta":{"tool_calls":[
        {"index":0,"id":"write-1","function":{"name":"write","arguments":"{\"path\":\"bad\",\"content\":\"bad\"}"}}
    ]},"finish_reason":"tool_calls"}]})
    );
    let (url, server) = mock_server(vec![frame]).await;
    let output = cli(dir.path(), &url, "openai").output().await.unwrap();
    assert!(!output.status.success());
    assert!(!dir.path().join("bad").exists());
    assert!(String::from_utf8_lossy(&output.stderr).contains("terminal event"));
    server.await.unwrap();
}

#[tokio::test]
async fn cli_help_is_available_without_credentials() {
    let output = Command::new(common::exe())
        .arg("--help")
        .env_remove("COVE_API_KEY")
        .launch()
        .await
        .unwrap();
    assert!(output.status.success());
    assert!(String::from_utf8_lossy(&output.stdout).contains("--permission"));
}

#[tokio::test]
async fn task_deadline_cancels_a_pending_http_stream_without_hanging() {
    let dir = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/v1", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut bytes = [0_u8; 8192];
        assert!(socket.read(&mut bytes).await.unwrap() > 0);
        socket.write_all(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n").await.unwrap();
        tokio::time::sleep(Duration::from_secs(10)).await;
    });
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        "openai",
        "--model",
        "mock",
        "--base-url",
        &url,
        "--timeout",
        "1",
        "-p",
        "task",
    ]);
    command
        .arg("--cwd")
        .arg(dir.path())
        .env("COVE_API_KEY", "test-key")
        .kill_on_drop(true);
    command.env("COVE_DATA_DIR", data.path());
    let output = tokio::time::timeout(Duration::from_secs(5), command.launch()).await;
    server.abort();
    let output = output.unwrap().unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("任务超过时限"));
}

#[tokio::test]
async fn user_config_selects_profile_model_and_limits() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![openai(json!({"content":"done"}), "stop")]).await;
    std::fs::write(
        data.path().join("config.json"),
        json!({
            "active_profile": "ds",
            "model": "root-model",
            "debug": true,
            "permission_mode": "plan",
            "profiles": {
                "ds": {
                    "model": "profile-model",
                    "theme": "ignored",
                    "provider": {"name": "deepseek", "api_key": "file-only-test-key", "base_url": format!("{url}/chat/completions")},
                    "max_output_tokens": 777,
                    "system_prompt": "Prefer small diffs."
                },
                "other": {"model": "other-model"}
            }
        })
        .to_string(),
    )
    .unwrap();
    let output = Command::new(common::exe())
        .args(["-p", "say done", "--timeout", "20"])
        .arg("--cwd")
        .arg(root.path())
        .arg("--data-dir")
        .arg(data.path())
        .env_remove("COVE_API_KEY")
        .env_remove("DEEPSEEK_API_KEY")
        .env_remove("COVE_MODEL")
        .env_remove("COVE_PROVIDER")
        .env_remove("COVE_BASE_URL")
        .env_remove("COVE_PROFILE")
        .env_remove("COVE_CONFIG")
        .kill_on_drop(true)
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(
        stderr.contains("debug") && stderr.contains("profiles.ds.theme"),
        "{stderr}"
    );
    let requests = server.await.unwrap();
    assert_eq!(requests[0]["model"], "profile-model");
    assert_eq!(requests[0]["max_tokens"], 777);
    assert!(requests[0].to_string().contains("Prefer small diffs."));

    let missing = Command::new(common::exe())
        .args(["-p", "x", "--profile", "nope"])
        .arg("--cwd")
        .arg(root.path())
        .arg("--data-dir")
        .arg(data.path())
        .env_remove("COVE_PROFILE")
        .env_remove("COVE_CONFIG")
        .launch()
        .await
        .unwrap();
    assert!(!missing.status.success());
    let stderr = String::from_utf8_lossy(&missing.stderr);
    assert!(
        stderr.contains("没有 profile \"nope\"") && stderr.contains("ds, other"),
        "{stderr}"
    );
}

fn python() -> &'static str {
    if cfg!(windows) { "python" } else { "python3" }
}

#[tokio::test]
async fn verification_gate_feeds_failures_back_until_checks_pass() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "old").unwrap();
    std::fs::write(
        data.path().join("config.json"),
        json!({"done_verify_commands":[format!("{} -c \"import sys; sys.exit(0 if open('notes.txt').read() == 'fixed' else 3)\"", python())]}).to_string(),
    )
    .unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "plan",
            "todo",
            json!({"todos":[{"content":"rewrite notes","status":"completed"}]}),
        ),
        tool("w1", "write", json!({"path":"notes.txt","content":"draft"})),
        tool("r1", "read", json!({"path":"notes.txt"})),
        tool(
            "w2",
            "write",
            json!({"path":"notes.txt","content":"draft","overwrite":true}),
        ),
        openai(json!({"content":"Done."}), "stop"),
        tool(
            "w3",
            "write",
            json!({"path":"notes.txt","content":"fixed","overwrite":true}),
        ),
        openai(json!({"content":"Fixed and verified."}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["-p", "rewrite notes"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("notes.txt")).unwrap(),
        "fixed"
    );
    assert!(
        requests[1]
            .to_string()
            .contains("[✓] todo-1. rewrite notes")
    );
    assert!(requests[2].to_string().contains("read it first"));
    assert!(
        requests[5]
            .to_string()
            .contains("automatic verification failed")
    );
    assert!(requests[5].to_string().contains("exit_code=Some(3)"));
    assert!(
        stderr.contains("passed") && stderr.contains("验证通过"),
        "{stderr}"
    );
    // The refused first write still counts as a failed tool call.
    assert!(!output.status.success());
}

#[tokio::test]
async fn sub_agent_reads_with_its_own_tools_and_reports_to_parent() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("lib.rs"), "fn entry() {}\n").unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "delegate",
            "agent",
            json!({"description":"find entry","prompt":"Where is entry defined?","type":"review"}),
        ),
        tool("look", "read", json!({"path":"lib.rs"})),
        openai(json!({"content":"entry is at lib.rs:1"}), "stop"),
        openai(json!({"content":"The sub-agent found lib.rs:1."}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args([
            "-p",
            "find entry",
            "--input-price",
            "1",
            "--output-price",
            "1",
            "--budget",
            "5",
        ])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    let requests = server.await.unwrap();
    let sub = &requests[1];
    assert!(
        sub.to_string()
            .contains("delegated read-only review sub-agent")
    );
    let tools: Vec<_> = sub["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|tool| tool["function"]["name"].as_str().unwrap().to_owned())
        .collect();
    assert_eq!(tools, ["read", "glob", "grep", "repo_map"]);
    assert!(requests[2].to_string().contains("fn entry()"));
    assert!(requests[3].to_string().contains("entry is at lib.rs:1"));
    // Parent and sub-agent requests are both accounted.
    assert!(stderr.contains("模型调用 4 次"), "{stderr}");
}

#[tokio::test]
async fn shell_output_streams_live_without_repeating_the_result() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "say",
            "shell",
            json!({"command":"echo live-one; echo live-two"}),
        ),
        openai(json!({"content":"Ran it."}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["-p", "run echo"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(stderr.contains("  | live-one\n  | live-two\n"), "{stderr}");
    assert!(stderr.contains("[ok] shell: exit_code=Some(0)"), "{stderr}");
    assert!(!stderr.contains("stdout:"), "{stderr}");
    // The model still receives the complete captured result.
    let requests = server.await.unwrap();
    assert!(requests[1].to_string().contains("live-two"));
}

#[tokio::test]
async fn repo_map_ranks_definitions_for_a_query() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::create_dir(root.path().join("src")).unwrap();
    std::fs::write(
        root.path().join("src/store.rs"),
        "pub struct Store;\nimpl Store {\n    pub fn load_session(&self) {}\n}\n",
    )
    .unwrap();
    std::fs::write(
        root.path().join("src/app.rs"),
        "fn start(store: &Store) { store.load_session(); }\n",
    )
    .unwrap();
    let (url, server) = mock_server(vec![
        tool("map", "repo_map", json!({"query":"load_session"})),
        tool("all", "repo_map", json!({"path":"src"})),
        openai(json!({"content":"Mapped."}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["-p", "map the repo"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.await.unwrap();
    let result = |request: &Value| {
        request["messages"].as_array().unwrap().last().unwrap()["content"]
            .as_str()
            .unwrap()
            .to_owned()
    };
    assert_eq!(
        result(&requests[1]),
        "src/store.rs\n      3: pub fn load_session(&self)\nsrc/app.rs\n      1: fn start(store: &Store)\n"
    );
    let all = result(&requests[2]);
    assert!(all.starts_with("src/store.rs\n"), "{all}");
    assert!(all.contains("      1: pub struct Store;"), "{all}");
}

#[tokio::test]
async fn write_sub_agent_changes_files_under_the_parent_checkpoint() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool(
            "delegate",
            "agent",
            json!({"description":"create readme","prompt":"Create README.md saying hi","mode":"write"}),
        ),
        tool("create", "write", json!({"path":"README.md","content":"hi\n"})),
        openai(json!({"content":"Created README.md; no checks needed."}), "stop"),
        openai(json!({"content":"The sub-agent created README.md."}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["-p", "add a readme"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("README.md")).unwrap(),
        "hi\n"
    );
    let tools: Vec<_> = requests[1]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|tool| tool["function"]["name"].as_str().unwrap().to_owned())
        .collect();
    assert!(tools.contains(&"write".to_owned()) && tools.contains(&"shell".to_owned()));
    assert!(
        !tools
            .iter()
            .any(|name| ["agent", "ask_user", "question", "memory", "mcp"].contains(&name.as_str()))
    );
    assert!(
        requests[3]
            .to_string()
            .contains("(general): create readme] [exit: completed, steps: 2, truncated: no]")
    );
    // The sub-agent's write belongs to the parent task's checkpoint.
    let id = saved_id(data.path());
    let diff = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &id, "-p", "/diff"])
        .launch()
        .await
        .unwrap();
    assert!(
        String::from_utf8_lossy(&diff.stdout).contains("+hi"),
        "{}",
        String::from_utf8_lossy(&diff.stdout)
    );
}

#[tokio::test]
async fn print_mode_does_not_wait_forever_on_an_idle_stdin_pipe() {
    let dir = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let mut command = Command::new(common::exe());
    command
        .args([
            "--provider",
            "openai",
            "--model",
            "mock",
            "--base-url",
            "http://127.0.0.1:9/v1",
        ])
        .arg("--cwd")
        .arg(dir.path())
        .arg("--data-dir")
        .arg(data.path())
        .args(["-p", "/status"])
        .env("COVE_API_KEY", "test-key")
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped())
        .kill_on_drop(true);
    // The pipe stays open and silent, like an IDE's unused stdin; it was taken
    // for a redirected file and read to its end, which never came.
    let mut child = command.spawn().unwrap();
    let stdin = child.stdin.take();
    let output = tokio::time::timeout(Duration::from_secs(30), child.wait_with_output())
        .await
        .expect("cove waited on a silent stdin pipe")
        .unwrap();
    drop(stdin);
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(stderr.contains("标准输入没有数据"), "{stderr}");
}

#[tokio::test]
async fn usage_reports_spend_time_code_changes_and_models() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "one\ntwo\nthree\n").unwrap();
    let (url, _server) = mock_server(vec![
        tool("r1", "read", json!({"path":"notes.txt"})),
        tool(
            "w1",
            "write",
            json!({"path":"notes.txt","content":"one\n2\nthree\nfour\n"}),
        ),
        openai(json!({"content":"Updated notes.txt."}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["-p", "update the notes"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let id = saved_id(data.path());
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &id, "-p", "/usage"])
        .launch()
        .await
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(
        stdout.contains("认证方式        OpenAI API（密钥来自 COVE_API_KEY）"),
        "{stdout}"
    );
    assert!(stdout.contains("新增 2 行，删除 1 行"), "{stdout}");
    assert!(stdout.contains("模型调用        3"), "{stdout}");
    assert!(stdout.contains("API 耗时"), "{stdout}");
    assert!(
        stdout.contains("mock-model\n    输入 60，输出 30"),
        "{stdout}"
    );
}

#[tokio::test]
async fn a_data_directory_inside_the_workspace_is_out_of_every_tools_reach() {
    let root = tempfile::tempdir().unwrap();
    let data = root.path().join(".cove");
    std::fs::create_dir_all(&data).unwrap();
    std::fs::write(data.join("config.json"), r#"{"marker": "SECRET-MARKER"}"#).unwrap();
    std::fs::write(
        root.path().join("notes.txt"),
        "SECRET-MARKER is only a word here\n",
    )
    .unwrap();
    let (url, server) = mock_server(vec![
        tool("r1", "read", json!({"path":".cove/config.json"})),
        tool("g1", "grep", json!({"pattern":"SECRET-MARKER"})),
        openai(json!({"content":"done"}), "stop"),
    ])
    .await;
    let output = persistent_cli(root.path(), &data, &url)
        .args(["-p", "look around"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(!stderr.contains("must be outside"), "{stderr}");
    let requests = server.await.unwrap();
    let results = requests[2]["messages"].to_string();
    assert!(results.contains("own data directory"), "{results}");
    assert!(
        results.contains("notes.txt") && !results.contains("config.json:"),
        "{results}"
    );
}

#[tokio::test]
async fn a_new_message_moves_on_from_a_task_stopped_during_a_question() {
    use cove::{
        domain::{Block, Message, Role},
        session::workspace::Workspace,
        session::{CallState, PendingCall, Session, SessionPersistence, State, Store},
    };
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Workspace::new(root.path().into(), store.clone()).unwrap();
    let checkpoint = workspace.begin().unwrap();
    let mut session = Session::new(
        root.path().canonicalize().unwrap(),
        "Openai".into(),
        "mock-model".into(),
        None,
    );
    session.current_checkpoint = Some(checkpoint.clone());
    session.checkpoints.push(checkpoint);
    session.current_request = "plan the journal query".into();
    session.state = State::Running;
    let input = json!({"questions":[{"question":"Which permission?","options":[{"label":"none"},{"label":"read"}]}]});
    session.messages = vec![
        Message::user("plan the journal query"),
        Message {
            role: Role::Assistant,
            blocks: vec![Block::ToolUse {
                id: "q1".into(),
                name: "question".into(),
                input: input.clone(),
                extra: None,
            }],
        },
    ]
    .into();
    session.pending.push(PendingCall {
        id: "q1".into(),
        name: "question".into(),
        input,
        state: CallState::Executing,
        result: None,
    });
    store.save(&session).unwrap();
    let (url, server) = mock_server(vec![openai(
        json!({"content":"Yes, CCP-3359 is reachable."}),
        "stop",
    )])
    .await;
    let output = persistent_cli(root.path(), data.path(), &url)
        .args(["--resume", &session.id, "-p", "能访问CCP-3359吗"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(!stderr.contains("unfinished task exists"), "{stderr}");
    let request = server.await.unwrap()[0]["messages"].to_string();
    assert!(
        request.contains("Not run: the user sent a new message instead."),
        "{request}"
    );
    assert!(request.contains("能访问CCP-3359吗"), "{request}");
}
