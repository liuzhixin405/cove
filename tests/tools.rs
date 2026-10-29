//! Area A: file tools, shell, search, question/todo, concurrent dispatch and output caps.

mod common;
use common::Launch;
use std::{
    collections::VecDeque,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use cove::{
    domain::{
        Block, Error, Event, Message, ModelRequest, ModelResponse, Provider, Result, Role,
        ToolDefinition, Usage, UserInput,
    },
    engine::agent::{Agent, Limits},
    process::{Shell, ShellKind},
    safety::permission::{LocalAuthorizer, Mode},
    session::Store,
    session::workspace::Workspace,
    tools::{LocalTools, ToolExecutor, ToolOutput},
};
use futures_util::future::BoxFuture;
use serde_json::{Value, json};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    process::Command,
    sync::mpsc,
};
use tokio_util::sync::CancellationToken;

fn tools(root: &std::path::Path) -> LocalTools {
    LocalTools::new(
        root.to_path_buf(),
        Arc::new(LocalAuthorizer {
            mode: Mode::Bypass,
            input: None,
        }),
    )
    .unwrap()
}

async fn run(tools: &LocalTools, name: &str, input: Value) -> ToolOutput {
    tools
        .execute(name.into(), input, CancellationToken::new())
        .await
        .unwrap()
}

fn gbk(text: &str) -> Vec<u8> {
    encoding_rs::GBK.encode(text).0.into_owned()
}

#[tokio::test]
async fn read_windows_lines_lists_directories_and_clips_long_lines() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir(dir.path().join("sub")).unwrap();
    let text: String = (1..=2500).map(|n| format!("line {n}\n")).collect();
    std::fs::write(dir.path().join("big.txt"), text).unwrap();
    std::fs::write(dir.path().join("wide.txt"), "x".repeat(2500)).unwrap();
    let tools = tools(dir.path());

    let out = run(&tools, "read", json!({"path":"big.txt"})).await;
    assert!(
        out.content
            .starts_with("File: big.txt (2500 lines total)\n\n1: line 1\n")
    );
    assert!(out.content.contains("\n2000: line 2000\n"));
    assert!(!out.content.contains("2001: line 2001"));
    assert!(
        out.content.ends_with("[next: offset=2001]"),
        "{}",
        out.content
    );

    let out = run(
        &tools,
        "read",
        json!({"file_path":"big.txt","offset":2499,"limit":5}),
    )
    .await;
    assert!(
        out.content.ends_with("2499: line 2499\n2500: line 2500"),
        "{}",
        out.content
    );
    let out = run(
        &tools,
        "read",
        json!({"path":"big.txt","start_line":10,"lines":1}),
    )
    .await;
    assert!(
        out.content
            .contains("10: line 10\n... [showing lines 10-10 of 2500]\n[next: offset=11]")
    );

    let out = run(&tools, "read", json!({"path":"wide.txt"})).await;
    assert!(
        out.content
            .ends_with(&format!("1: {}…[+500 chars]", "x".repeat(2000)))
    );

    let out = run(&tools, "read", json!({"path":"."})).await;
    assert!(
        out.content.contains("big.txt\nsub/\nwide.txt"),
        "{}",
        out.content
    );
}

#[tokio::test]
async fn read_decodes_gbk_and_refuses_binary_and_utf16() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("gbk.txt"), gbk("你好，世界\r\n第二行\r\n")).unwrap();
    std::fs::write(dir.path().join("bin.dat"), b"\x00\x01\x02").unwrap();
    std::fs::write(dir.path().join("u16.txt"), b"\xff\xfeh\x00i\x00").unwrap();
    let tools = tools(dir.path());
    let out = run(&tools, "read", json!({"path":"gbk.txt"})).await;
    assert!(!out.failed);
    assert!(
        out.content.contains("file is GBK-encoded"),
        "{}",
        out.content
    );
    assert!(
        out.content.contains("1: 你好，世界\n2: 第二行"),
        "{}",
        out.content
    );
    let out = run(&tools, "read", json!({"path":"bin.dat"})).await;
    assert!(out.failed && out.content.contains("binary"));
    let out = run(&tools, "read", json!({"path":"u16.txt"})).await;
    assert!(
        out.failed && out.content.contains("UTF-16"),
        "{}",
        out.content
    );
}

#[tokio::test]
async fn write_creates_parents_and_replaces_only_files_read_in_their_current_version() {
    let dir = tempfile::tempdir().unwrap();
    let tools = tools(dir.path());
    let out = run(
        &tools,
        "write",
        json!({"filePath":"a/b/new.txt","text":"one\n"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert_eq!(
        std::fs::read(dir.path().join("a/b/new.txt")).unwrap(),
        b"one\n"
    );

    std::fs::write(dir.path().join("win.txt"), "\u{feff}old\r\nlines\r\n").unwrap();
    let out = run(&tools, "write", json!({"path":"win.txt","content":"new\n"})).await;
    assert!(
        out.failed && out.content.contains("read it first"),
        "{}",
        out.content
    );
    // A read-only sub-agent's view shares the tracker.
    let readonly = tools.restricted(Arc::new(LocalAuthorizer {
        mode: Mode::Readonly,
        input: None,
    }));
    assert!(
        !run(&readonly, "read", json!({"path":"win.txt","limit":1}))
            .await
            .failed
    );
    let out = run(
        &tools,
        "write",
        json!({"path":"win.txt","content":"new\ntext\n"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert_eq!(
        std::fs::read(dir.path().join("win.txt")).unwrap(),
        "\u{feff}new\r\ntext\r\n".as_bytes()
    );
    std::fs::write(dir.path().join("win.txt"), "user change").unwrap();
    let out = run(&tools, "write", json!({"path":"win.txt","content":"mine"})).await;
    assert!(
        out.failed && out.content.contains("changed since"),
        "{}",
        out.content
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("win.txt")).unwrap(),
        "user change"
    );
}

#[tokio::test]
async fn gbk_files_stay_gbk_through_journaled_write_edit_and_undo() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Arc::new(Workspace::new(root.path().into(), store).unwrap());
    workspace.begin().unwrap();
    let original = gbk("// 中文注释\r\nlet a = 1;\r\n");
    std::fs::write(root.path().join("main.rs"), &original).unwrap();
    let tools = tools(root.path()).with_workspace(workspace.clone());

    let out = run(
        &tools,
        "edit",
        json!({"path":"main.rs","old":"中文注释\nlet a = 1;","new":"注释已改\nlet a = 2;"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert!(out.content.contains("1: // 注释已改"), "{}", out.content);
    assert_eq!(
        std::fs::read(root.path().join("main.rs")).unwrap(),
        gbk("// 注释已改\r\nlet a = 2;\r\n")
    );
    let out = run(
        &tools,
        "edit",
        json!({"path":"main.rs","old":"a = 2","new":"😀"}),
    )
    .await;
    assert!(
        out.failed && out.content.contains("GBK cannot represent"),
        "{}",
        out.content
    );

    assert!(!run(&tools, "read", json!({"path":"main.rs"})).await.failed);
    let out = run(
        &tools,
        "write",
        json!({"path":"main.rs","content":"// 全部重写\n"}),
    )
    .await;
    assert!(
        !out.failed && out.content.contains("kept GBK"),
        "{}",
        out.content
    );
    assert_eq!(
        std::fs::read(root.path().join("main.rs")).unwrap(),
        gbk("// 全部重写\r\n")
    );
    assert!(workspace.diff().unwrap().contains("+// 全部重写"));
    workspace.undo().unwrap();
    assert_eq!(
        std::fs::read(root.path().join("main.rs")).unwrap(),
        original
    );
}

#[tokio::test]
async fn edit_replaces_all_lists_ambiguous_lines_and_matches_whitespace_variants() {
    let dir = tempfile::tempdir().unwrap();
    std::fs::write(dir.path().join("a.txt"), "x = 1\ny = 2\nx = 1\n").unwrap();
    std::fs::write(
        dir.path().join("b.py"),
        "def f():\n    if ok:\n        run()\n    return 1\n",
    )
    .unwrap();
    let tools = tools(dir.path());
    let out = run(
        &tools,
        "edit",
        json!({"path":"a.txt","old":"x = 1","new":"x = 3"}),
    )
    .await;
    assert!(
        out.failed && out.content.contains("2 times (lines 1, 3)"),
        "{}",
        out.content
    );
    let out = run(
        &tools,
        "edit",
        json!({"path":"a.txt","old_string":"x = 1","new_string":"x = 3","replaceAll":true}),
    )
    .await;
    assert!(
        !out.failed && out.content.contains("2 replacement(s)"),
        "{}",
        out.content
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("a.txt")).unwrap(),
        "x = 3\ny = 2\nx = 3\n"
    );

    let out = run(
        &tools,
        "edit",
        json!({"path":"b.py","old":"if ok:\n  run()","new":"if ok:\n  run()\n  log()"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert!(
        out.content
            .contains("whitespace-insensitive match at lines 2-3")
    );
    assert_eq!(
        std::fs::read_to_string(dir.path().join("b.py")).unwrap(),
        "def f():\n    if ok:\n        run()\n        log()\n    return 1\n"
    );
    let out = run(
        &tools,
        "edit",
        json!({"path":"b.py","old":"def f(): pass","new":"def g(): pass"}),
    )
    .await;
    assert!(
        out.failed && out.content.contains("Closest candidate"),
        "{}",
        out.content
    );
}

async fn grep_and_glob_semantics(ripgrep: bool) {
    let dir = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(dir.path().join("src/deep")).unwrap();
    std::fs::write(
        dir.path().join("src/lib.rs"),
        "fn Alpha() {}\nfn beta() {}\n",
    )
    .unwrap();
    std::fs::write(dir.path().join("src/deep/mod.rs"), "// alpha here\n").unwrap();
    std::fs::write(dir.path().join("notes.md"), "alpha notes\n").unwrap();
    let many: String = (0..150).map(|n| format!("hit {n}\n")).collect();
    std::fs::write(dir.path().join("many.txt"), many).unwrap();
    let tools = tools(dir.path()).with_ripgrep(ripgrep);

    let out = run(
        &tools,
        "grep",
        json!({"pattern":"alpha","include":"*.rs","ignore_case":true}),
    )
    .await;
    let mut lines: Vec<&str> = out.content.lines().collect();
    lines.sort();
    assert_eq!(
        lines,
        [
            "src/deep/mod.rs:1:// alpha here",
            "src/lib.rs:1:fn Alpha() {}"
        ],
        "{}",
        out.content
    );
    let out = run(
        &tools,
        "grep",
        json!({"pattern":"beta","path":"src","context":1}),
    )
    .await;
    assert_eq!(
        out.content,
        "src/lib.rs-1-fn Alpha() {}\nsrc/lib.rs:2:fn beta() {}"
    );
    let out = run(&tools, "grep", json!({"pattern":"alpha","files_only":true})).await;
    let mut files: Vec<&str> = out.content.lines().collect();
    files.sort();
    assert_eq!(files, ["notes.md", "src/deep/mod.rs"]);
    let out = run(&tools, "grep", json!({"pattern":"^hit"})).await;
    assert_eq!(out.content.lines().count(), 101, "{}", out.content);
    assert!(
        out.content.contains("more matches not shown"),
        "{}",
        out.content
    );
    let out = run(&tools, "grep", json!({"pattern":"nothing-matches-this"})).await;
    assert_eq!(out.content, "No matches found");

    let out = run(&tools, "glob", json!({"pattern":"*.rs"})).await;
    let mut lines: Vec<&str> = out.content.lines().collect();
    lines.sort();
    assert_eq!(lines, ["src/deep/mod.rs", "src/lib.rs"]);
    let out = run(
        &tools,
        "glob",
        json!({"pattern":"**/*.rs","path":"src/deep"}),
    )
    .await;
    assert_eq!(out.content, "src/deep/mod.rs");
    let out = run(&tools, "glob", json!({"pattern":"src/*.rs"})).await;
    assert_eq!(out.content, "src/lib.rs");
}

#[tokio::test]
async fn builtin_grep_and_glob_follow_the_go_semantics() {
    grep_and_glob_semantics(false).await;
}

#[tokio::test]
async fn ripgrep_backed_grep_gives_the_same_results() {
    if which::which("rg").is_err() {
        return;
    }
    grep_and_glob_semantics(true).await;
}

#[tokio::test]
async fn glob_caps_results_at_two_hundred() {
    let dir = tempfile::tempdir().unwrap();
    for n in 0..230 {
        std::fs::write(dir.path().join(format!("f{n}.txt")), "").unwrap();
    }
    let out = run(&tools(dir.path()), "glob", json!({"pattern":"*.txt"})).await;
    assert_eq!(out.content.lines().count(), 201);
    assert!(out.content.ends_with("... and 30 more"), "{}", out.content);
}

fn posix() -> bool {
    Shell::current().kind == ShellKind::Posix
}

#[tokio::test]
async fn shell_runs_noninteractive_with_long_timeouts() {
    let dir = tempfile::tempdir().unwrap();
    let tools = tools(dir.path());
    let script = if posix() {
        "echo \"color=$NO_COLOR prompt=$GIT_TERMINAL_PROMPT pager=$GIT_PAGER\""
    } else {
        "Write-Output \"color=$env:NO_COLOR prompt=$env:GIT_TERMINAL_PROMPT pager=$env:GIT_PAGER\""
    };
    let out = run(
        &tools,
        "shell",
        json!({"command":script,"timeout_seconds":600,"description":"show env"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert!(
        out.content.contains("color=1 prompt=0 pager=cat"),
        "{}",
        out.content
    );
    let out = run(
        &tools,
        "shell",
        json!({"command":"echo x","timeout_seconds":601}),
    )
    .await;
    assert!(out.failed && out.content.contains("1..600"));
    let description = tools
        .definitions()
        .into_iter()
        .find(|tool| tool.name == "shell")
        .unwrap()
        .description;
    assert!(
        description.contains(Shell::current().dialect()),
        "{description}"
    );
}

#[tokio::test]
async fn shell_output_keeps_head_and_tail() {
    let dir = tempfile::tempdir().unwrap();
    let script = if posix() {
        "for i in $(seq 1 5000); do echo \"row $i\"; done"
    } else {
        "1..5000 | ForEach-Object { \"row $_\" }"
    };
    let out = run(&tools(dir.path()), "shell", json!({"command":script})).await;
    assert!(!out.failed, "{}", out.content);
    assert!(out.content.contains("truncated=true"));
    assert!(
        out.content.contains("stdout:\nrow 1\nrow 2\n"),
        "{}",
        &out.content[..200]
    );
    assert!(out.content.contains("bytes omitted from the middle"));
    assert!(out.content.contains("row 5000\n"));
}

#[cfg(windows)]
#[tokio::test]
async fn cancelled_bash_kills_background_children() {
    if !posix() {
        return;
    }
    let dir = tempfile::tempdir().unwrap();
    let tools = tools(dir.path());
    let cancel = CancellationToken::new();
    let stop = cancel.clone();
    let ready = dir.path().join("ready");
    tokio::spawn(async move {
        // Cancel once the background child exists, however slow bash starts under load.
        for _ in 0..200 {
            if ready.exists() {
                break;
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
        stop.cancel();
    });
    let result = tools
        .execute(
            "shell".into(),
            json!({"command":"(sleep 3; echo bad > leaked) & echo started > ready; sleep 20"}),
            cancel,
        )
        .await;
    assert!(matches!(result, Err(Error::Cancelled)));
    tokio::time::sleep(Duration::from_secs(3)).await;
    assert!(dir.path().join("ready").exists());
    assert!(!dir.path().join("leaked").exists());
}

struct Answers(Mutex<VecDeque<&'static str>>);

impl UserInput for Answers {
    fn answer(&self, _: String, _: CancellationToken) -> BoxFuture<'_, Result<String>> {
        Box::pin(async move { Ok(self.0.lock().unwrap().pop_front().unwrap_or("").into()) })
    }
}

#[tokio::test]
async fn question_accepts_numbers_or_free_text_and_todo_shows_the_plan() {
    let dir = tempfile::tempdir().unwrap();
    let input: Arc<dyn UserInput> =
        Arc::new(Answers(Mutex::new(VecDeque::from(["2", "1 3", "my own"]))));
    let tools = tools(dir.path()).with_user_input(Some(input));
    let out = run(
        &tools,
        "question",
        json!({"questions":[
            {"question":"Database?","options":[{"label":"Postgres"},{"label":"SQLite"}]},
            {"question":"Extras?","multiple":true,"options":[{"label":"a"},{"label":"b"},{"label":"c"}]},
            {"question":"Name?"}
        ]}),
    )
    .await;
    assert_eq!(
        out.content,
        "[Q1] Database?\nAnswer: SQLite\n\n[Q2] Extras?\nAnswer: a, c\n\n[Q3] Name?\nAnswer: my own"
    );
    let out = run(
        &tools,
        "todo",
        json!({"todos":[
            {"content":"plan","status":"completed","priority":"high"},
            {"content":"depends:todo-1 build","status":"in_progress","priority":"medium"},
            {"content":"skip","status":"cancelled"}
        ]}),
    )
    .await;
    assert_eq!(
        out.content,
        "[✓] todo-1. plan [high]\n[>] todo-2. build [medium] (after todo-1)\n[x] todo-3. skip\n1 open of 3\n"
    );
}

/// Read-only tool that records how many calls run at once.
struct SlowReads {
    active: AtomicUsize,
    peak: AtomicUsize,
}

impl ToolExecutor for SlowReads {
    fn definitions(&self) -> Vec<ToolDefinition> {
        vec![ToolDefinition {
            name: "read".into(),
            description: "read".into(),
            schema: json!({"type":"object"}),
        }]
    }

    fn metered(&self, _: &str) -> bool {
        false
    }

    fn concurrent(&self, name: &str) -> bool {
        name == "read"
    }

    fn execute(
        &self,
        _: String,
        input: Value,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<ToolOutput>> {
        Box::pin(async move {
            let now = self.active.fetch_add(1, Ordering::SeqCst) + 1;
            self.peak.fetch_max(now, Ordering::SeqCst);
            // Later calls finish first, so ordering is not an accident of timing.
            let delay = 400 - 100 * input["n"].as_u64().unwrap();
            tokio::time::sleep(Duration::from_millis(delay)).await;
            self.active.fetch_sub(1, Ordering::SeqCst);
            Ok(ToolOutput {
                content: format!("result {}", input["n"]),
                failed: false,
                ..Default::default()
            })
        })
    }
}

struct Scripted {
    responses: Mutex<VecDeque<ModelResponse>>,
    requests: Mutex<Vec<ModelRequest>>,
}

impl Provider for Scripted {
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
                .ok_or_else(|| Error::Provider("exhausted".into()))
        })
    }
}

fn reply(blocks: Vec<Block>) -> ModelResponse {
    ModelResponse {
        message: Message {
            role: Role::Assistant,
            blocks,
        },
        usage: Usage {
            input: 1,
            output: 1,
            ..Usage::default()
        },
        complete: true,
        usage_reported: true,
        model: None,
    }
}

#[tokio::test]
async fn consecutive_read_only_calls_run_concurrently_with_ordered_results() {
    let calls = (0..3)
        .map(|n| Block::ToolUse {
            id: format!("c{n}"),
            name: "read".into(),
            input: json!({"n":n}),
            extra: None,
        })
        .collect();
    let provider = Arc::new(Scripted {
        responses: Mutex::new(VecDeque::from([
            reply(calls),
            reply(vec![Block::Text("done".into())]),
        ])),
        requests: Mutex::new(Vec::new()),
    });
    let reads = Arc::new(SlowReads {
        active: AtomicUsize::new(0),
        peak: AtomicUsize::new(0),
    });
    let agent = Agent::new(
        provider.clone(),
        reads.clone(),
        Limits {
            max_turns: 3,
            max_output_tokens: 100,
            budget: None,
            prices: None,
        },
    )
    .unwrap();
    let (events, mut receiver) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while receiver.recv().await.is_some() {} });
    let started = std::time::Instant::now();
    let report = agent
        .run("read three files".into(), events, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.tools_run, 3);
    assert_eq!(reads.peak.load(Ordering::SeqCst), 3);
    assert!(
        started.elapsed() < Duration::from_millis(800),
        "{:?}",
        started.elapsed()
    );
    let requests = provider.requests.lock().unwrap();
    let results: Vec<(String, String)> = requests[1]
        .messages
        .last()
        .unwrap()
        .blocks
        .iter()
        .filter_map(|block| match block {
            Block::ToolResult { id, content, .. } => Some((id.clone(), content.clone())),
            _ => None,
        })
        .collect();
    assert_eq!(
        results,
        [
            ("c0".to_owned(), "result 0".to_owned()),
            ("c1".to_owned(), "result 1".to_owned()),
            ("c2".to_owned(), "result 2".to_owned())
        ]
    );
}

// ---- CLI level, against a mock OpenAI-compatible SSE server ----

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
            socket.write_all(response.as_bytes()).await.unwrap();
            socket.shutdown().await.unwrap();
        }
        requests
    });
    (format!("http://{address}/v1"), task)
}

fn openai(delta: Value, reason: &str) -> String {
    format!(
        "data: {}\n\ndata: {}\n\ndata: [DONE]\n\n",
        json!({"choices":[{"delta":delta,"finish_reason":reason}]}),
        json!({"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10}})
    )
}

fn tool_calls(calls: &[(&str, &str, Value)]) -> String {
    let calls: Vec<Value> = calls
        .iter()
        .enumerate()
        .map(|(index, (id, name, arguments))| {
            json!({"index":index,"id":id,"type":"function","function":{"name":name,"arguments":arguments.to_string()}})
        })
        .collect();
    openai(json!({ "tool_calls": calls }), "tool_calls")
}

fn cli(root: &std::path::Path, data: &std::path::Path, url: &str) -> Command {
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
        "30",
        "-p",
        "update the module",
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.kill_on_drop(true);
    command
}

fn tool_result<'a>(request: &'a Value, id: &str) -> &'a str {
    request["messages"]
        .as_array()
        .unwrap()
        .iter()
        .find(|message| message["tool_call_id"] == id)
        .and_then(|message| message["content"].as_str())
        .unwrap_or_else(|| panic!("no result for {id}"))
}

#[tokio::test]
async fn cli_reads_in_parallel_keeps_gbk_and_adds_subdirectory_instructions() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(root.path().join("pkg")).unwrap();
    std::fs::write(root.path().join("pkg/AGENTS.md"), "Use tabs in pkg.").unwrap();
    std::fs::write(
        root.path().join("pkg/mod.c"),
        gbk("/* 模块 */\r\nint x = 1;\r\n"),
    )
    .unwrap();
    let (url, server) = mock_server(vec![
        tool_calls(&[
            ("r1", "read", json!({"path":"pkg/mod.c"})),
            ("g1", "grep", json!({"pattern":"int x","include":"*.c"})),
            ("l1", "glob", json!({"pattern":"**/*.c"})),
        ]),
        tool_calls(&[(
            "e1",
            "edit",
            json!({"path":"pkg/mod.c","old":"int x = 1;","new":"int x = 2; /* 已改 */"}),
        )]),
        openai(json!({"content":"Updated pkg/mod.c."}), "stop"),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url).launch().await.unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    let requests = server.await.unwrap();
    let read = tool_result(&requests[1], "r1");
    assert!(read.contains("File: pkg/mod.c (2 lines total)"), "{read}");
    assert!(
        read.contains("GBK-encoded") && read.contains("1: /* 模块 */"),
        "{read}"
    );
    assert!(
        read.contains("[Context from pkg/AGENTS.md]\nUse tabs in pkg."),
        "{read}"
    );
    assert!(tool_result(&requests[1], "g1").contains("pkg/mod.c:2:int x = 1;"));
    assert_eq!(tool_result(&requests[1], "l1"), "pkg/mod.c");
    // The hint is shown once, not again for the grep result or the edit.
    assert!(!tool_result(&requests[1], "g1").contains("[Context from"));
    let edit = tool_result(&requests[2], "e1");
    assert!(edit.contains("2: int x = 2; /* 已改 */"), "{edit}");
    assert_eq!(
        std::fs::read(root.path().join("pkg/mod.c")).unwrap(),
        gbk("/* 模块 */\r\nint x = 2; /* 已改 */\r\n")
    );
    // The system prompt names the shell the tool really runs.
    let system = requests[0]["messages"][0]["content"].as_str().unwrap();
    assert!(
        system.contains(&format!("Shell dialect: {}", Shell::current().dialect())),
        "{system}"
    );
}

#[tokio::test]
async fn cli_writes_need_a_read_and_create_missing_directories() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("README.md"), "old\r\n").unwrap();
    let (url, server) = mock_server(vec![
        tool_calls(&[("w1", "write", json!({"path":"README.md","content":"new\n"}))]),
        tool_calls(&[
            ("r1", "read", json!({"file_path":"README.md"})),
            (
                "w2",
                "write",
                json!({"file_path":"README.md","content":"new\n"}),
            ),
            (
                "w3",
                "write",
                json!({"path":"docs/guide/intro.md","content":"intro\n"}),
            ),
        ]),
        openai(json!({"content":"Done."}), "stop"),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url).launch().await.unwrap();
    let requests = server.await.unwrap();
    assert!(tool_result(&requests[1], "w1").contains("read it first"));
    assert!(tool_result(&requests[2], "w2").starts_with("Overwrote README.md"));
    assert!(tool_result(&requests[2], "w3").starts_with("Created docs/guide/intro.md"));
    assert_eq!(
        std::fs::read(root.path().join("README.md")).unwrap(),
        b"new\r\n"
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("docs/guide/intro.md")).unwrap(),
        "intro\n"
    );
    // The refused first write counts as a failed tool call.
    assert!(!output.status.success());
}
