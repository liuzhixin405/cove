//! Area D: history, cost ledger, Git snapshots with /undo, memory commands and the
//! bare `继续` shortcut, through the real binary against a mock SSE server.

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
            let (mut socket, _) = tokio::time::timeout(Duration::from_secs(30), listener.accept())
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
        json!({"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10,
            "prompt_tokens_details":{"cached_tokens":5}}})
    )
}

fn tool(id: &str, name: &str, arguments: Value) -> String {
    openai(
        json!({"tool_calls":[{"index":0,"id":id,"type":"function","function":{"name":name,"arguments":arguments.to_string()}}]}),
        "tool_calls",
    )
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
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.kill_on_drop(true);
    command
}

async fn repl(mut command: Command, input: &str) -> (String, String) {
    command
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped());
    let mut child = command.spawn().unwrap();
    let mut stdin = child.stdin.take().unwrap();
    stdin.write_all(input.as_bytes()).await.unwrap();
    drop(stdin);
    let output = tokio::time::timeout(Duration::from_secs(90), child.wait_with_output())
        .await
        .unwrap()
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    assert!(output.status.success(), "{stdout}\n{stderr}");
    (stdout, stderr)
}

async fn one_shot(mut command: Command, prompt: &str) -> String {
    let output = command.args(["-p", prompt]).launch().await.unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    assert!(
        output.status.success(),
        "{stdout}\n{}",
        String::from_utf8_lossy(&output.stderr)
    );
    stdout
}

fn session_ids(data: &std::path::Path) -> Vec<String> {
    std::fs::read_dir(data.join("sessions"))
        .unwrap()
        .map(|entry| {
            entry
                .unwrap()
                .path()
                .file_stem()
                .unwrap()
                .to_string_lossy()
                .into_owned()
        })
        .collect()
}

#[tokio::test]
async fn history_lists_titles_resumes_by_number_and_exports_markdown() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        openai(json!({"content":"login fixed"}), "stop"),
        openai(json!({"content":"retries added"}), "stop"),
    ])
    .await;
    let (stdout, stderr) = repl(
        cli(root.path(), data.path(), &url),
        "Fix the login bug\n/new\nAdd retry logic\n/resume\n/resume search login\n1\n/resume detail 1\n/resume export\n/exit\n",
    )
    .await;
    server.await.unwrap();
    let listed: Vec<&str> = stdout
        .lines()
        .filter(|line| line.trim_start().starts_with("1.") || line.trim_start().starts_with("2."))
        .collect();
    assert!(
        listed
            .iter()
            .any(|line| line.contains("Add retry logic") && line.contains("(current)"))
            && listed.iter().any(|line| line.contains("Fix the login bug")),
        "{stdout}"
    );
    // The search lists only the matching session, so "1" resumes it.
    assert!(
        stdout
            .lines()
            .any(|line| line.starts_with("[session] ") && line.ends_with("Fix the login bug")),
        "{stdout}"
    );
    assert!(stderr.contains("[session]"), "{stderr}");
    assert!(stdout.contains("最近的请求："), "{stdout}");
    let exported = std::fs::read_dir(data.path().join("exports"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let markdown = std::fs::read_to_string(exported).unwrap();
    assert!(markdown.starts_with("# Fix the login bug"), "{markdown}");
    assert!(markdown.contains("## 助手\n\nlogin fixed"));
    let output = Command::new(common::exe())
        .arg("--list-sessions")
        .arg("--cwd")
        .arg(root.path())
        .arg("--data-dir")
        .arg(data.path())
        .env_remove("COVE_API_KEY")
        .launch()
        .await
        .unwrap();
    let listed = String::from_utf8_lossy(&output.stdout);
    assert!(output.status.success());
    assert!(listed.contains("Add retry logic") && listed.contains("Fix the login bug"));
    let other = tempfile::tempdir().unwrap();
    let output = Command::new(common::exe())
        .args(["--list-sessions", "all"])
        .arg("--cwd")
        .arg(other.path())
        .arg("--data-dir")
        .arg(data.path())
        .launch()
        .await
        .unwrap();
    assert!(String::from_utf8_lossy(&output.stdout).contains("Fix the login bug"));
}

#[tokio::test]
async fn delete_and_clear_never_remove_the_current_session() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![openai(json!({"content":"ok"}), "stop")]).await;
    let (stdout, _) = repl(
        cli(root.path(), data.path(), &url),
        "Write the changelog\n/new\n/new\n/resume clear\n/resume clear confirm\n/exit\n",
    )
    .await;
    server.await.unwrap();
    assert!(stdout.contains("将删除 2 个会话"), "{stdout}");
    assert!(stdout.contains("已删除 2 个会话"), "{stdout}");
    let ids = session_ids(data.path());
    assert_eq!(ids.len(), 1);
    let mut command = cli(root.path(), data.path(), &url);
    let delete = format!("/resume delete {}", &ids[0][..8]);
    command.args(["--resume", &ids[0], "-p", &delete]);
    let output = command.launch().await.unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("不能删除当前会话"));
    assert_eq!(session_ids(data.path()).len(), 1);
}

#[tokio::test]
async fn cost_ledger_records_each_call_and_exports_csv() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "hello").unwrap();
    let (url, server) = mock_server(vec![
        tool("r", "read", json!({"path":"notes.txt"})),
        openai(json!({"content":"done"}), "stop"),
    ])
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--input-price", "1", "--output-price", "2"]);
    one_shot(command, "inspect").await;
    server.await.unwrap();
    let month = std::fs::read_dir(data.path().join("usage"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let ledger = std::fs::read_to_string(month).unwrap();
    assert_eq!(ledger.lines().count(), 2);
    let entry: Value = serde_json::from_str(ledger.lines().next().unwrap()).unwrap();
    assert_eq!(entry["in"], 20);
    assert_eq!(entry["cache_read"], 5);
    assert_eq!(entry["model"], "mock-model");
    assert!(entry["session"].as_str().unwrap().len() == 32);
    let (url, _server) = mock_server(Vec::new()).await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--input-price", "1", "--output-price", "2"]);
    let text = one_shot(command, "/status cost today export").await;
    assert!(text.contains("openai/mock-model"), "{text}");
    assert!(text.contains("$0.0001"), "{text}");
    assert!(text.contains("已导出 2 次调用"), "{text}");
    let csv =
        std::fs::read_to_string(data.path().join("usage").join("export-todayUTC.csv")).unwrap();
    assert!(csv.starts_with("time_utc,provider,model"));
    assert_eq!(csv.lines().count(), 3);
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--input-price", "1", "--output-price", "2"]);
    let output = command
        .args(["-p", "/status cost yesterday"])
        .launch()
        .await
        .unwrap();
    assert!(!output.status.success());
}

fn git_available() -> bool {
    std::process::Command::new("git")
        .arg("--version")
        .output()
        .is_ok_and(|output| output.status.success())
}

fn python() -> &'static str {
    if cfg!(windows) { "python" } else { "python3" }
}

#[tokio::test]
async fn git_snapshots_restore_shell_effects_and_the_restore_is_undoable() {
    if !git_available() {
        return;
    }
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("a.txt"), "original").unwrap();
    let command = format!(
        "{} -c \"open('new.txt','w').write('x'); open('a.txt','w').write('changed')\"",
        python()
    );
    let (url, server) = mock_server(vec![
        tool("s", "shell", json!({"command": command})),
        openai(json!({"content":"ran the script"}), "stop"),
    ])
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["-p", "run the script"]);
    let output = command.launch().await.unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    server.await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("a.txt")).unwrap(),
        "changed"
    );
    let id = session_ids(data.path()).pop().unwrap();
    let (url, _server) = mock_server(Vec::new()).await;
    let resume = |prompt: &str| {
        let mut command = cli(root.path(), data.path(), &url);
        command.args(["--resume", &id]);
        let prompt = prompt.to_owned();
        async move { one_shot(command, &prompt).await }
    };
    let list = resume("/undo list").await;
    assert!(
        list.contains("#1") && list.contains("before shell"),
        "{list}"
    );
    let overview = resume("/undo").await;
    assert!(
        overview.contains("不含 Shell 的影响") && overview.contains("包含 Shell 的影响"),
        "{overview}"
    );
    let preview = resume("/undo #1").await;
    assert!(
        preview.contains("remove") && preview.contains("new.txt"),
        "{preview}"
    );
    assert!(
        preview.contains("restore") && preview.contains("a.txt"),
        "{preview}"
    );
    let token = regex::Regex::new(r"/undo confirm ([0-9a-f]{8})")
        .unwrap()
        .captures(&preview)
        .unwrap()[1]
        .to_owned();
    // Still read-only until confirmed.
    assert!(root.path().join("new.txt").exists());
    let applied = resume(&format!("/undo confirm {token}")).await;
    assert!(applied.contains("恢复整个工作区"), "{applied}");
    assert_eq!(
        std::fs::read_to_string(root.path().join("a.txt")).unwrap(),
        "original"
    );
    assert!(!root.path().join("new.txt").exists());
    // The token is single-use.
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--resume", &id, "-p", &format!("/undo confirm {token}")]);
    assert!(!command.launch().await.unwrap().status.success());
    // Undo the undo, restoring only one file.
    let list = resume("/undo list").await;
    assert!(list.contains("before restore"), "{list}");
    let preview = resume("/undo files #1 new.txt").await;
    let token = regex::Regex::new(r"/undo confirm ([0-9a-f]{8})")
        .unwrap()
        .captures(&preview)
        .unwrap()[1]
        .to_owned();
    resume(&format!("/undo confirm {token}")).await;
    assert!(root.path().join("new.txt").exists());
    assert_eq!(
        std::fs::read_to_string(root.path().join("a.txt")).unwrap(),
        "original"
    );
}

#[tokio::test]
async fn bare_continue_resumes_an_interrupted_task() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "hello").unwrap();
    let (url, server) = mock_server(vec![
        tool("r", "read", json!({"path":"notes.txt"})),
        openai(json!({"content":"summary: read notes"}), "stop"),
        openai(json!({"content":"finished after continue"}), "stop"),
    ])
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--max-turns", "1"]);
    let (stdout, stderr) = repl(command, "read the notes\n继续\n/exit\n").await;
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 3);
    assert!(
        stdout.contains("finished after continue"),
        "{stdout}\n{stderr}"
    );
    assert!(!stderr.contains("unfinished task exists"), "{stderr}");
}

#[tokio::test]
async fn memory_commands_inject_search_and_show_provenance() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![openai(json!({"content":"ok"}), "stop")]).await;
    let (stdout, stderr) = repl(
        cli(root.path(), data.path(), &url),
        "/config memory add 用户偏好中文回复\n/config memory add global Prefer ripgrep over grep\n/config memory search 中文回复\n/config memory stats\n/config memory dream status\nsay hello\n/exit\n",
    )
    .await;
    let requests = server.await.unwrap();
    assert!(
        requests[0].to_string().contains("用户偏好中文回复")
            && requests[0].to_string().contains("Prefer ripgrep over grep"),
        "{}",
        requests[0]
    );
    let id = stdout
        .lines()
        .find_map(|line| line.strip_prefix("已保存记忆 "))
        .and_then(|rest| rest.split_whitespace().next())
        .unwrap()
        .to_owned();
    assert!(stdout.contains("[project] 用户偏好中文回复"), "{stdout}");
    assert!(
        stdout.contains("项目：1 条") && stdout.contains("全局：1 条"),
        "{stdout}"
    );
    assert!(
        stdout.contains("自动提取：关闭（local provider）"),
        "{stdout}"
    );
    assert!(stdout.contains("dream：已启用"), "{stdout}\n{stderr}");
    let (url, _server) = mock_server(Vec::new()).await;
    let source = one_shot(
        cli(root.path(), data.path(), &url),
        &format!("/config memory source {}", &id[..8]),
    )
    .await;
    assert!(
        source.contains("operator 于") && source.contains("与当前内容一致"),
        "{source}"
    );
    let removed = one_shot(
        cli(root.path(), data.path(), &url),
        &format!("/config memory remove {}", &id[..8]),
    )
    .await;
    assert!(removed.contains("已删除记忆"), "{removed}");
    let list = one_shot(cli(root.path(), data.path(), &url), "/config memory list").await;
    assert!(!list.contains("中文") && list.contains("ripgrep"), "{list}");
}

#[tokio::test]
async fn continue_in_a_new_session_goes_back_to_the_last_task_and_shows_it() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, _first) = mock_server(vec![openai(
        json!({"content":"Rust ownership haiku."}),
        "stop",
    )])
    .await;
    repl(
        cli(root.path(), data.path(), &url),
        "write a haiku about rust\n/exit\n",
    )
    .await;
    let (url, second) = mock_server(vec![openai(
        json!({"content":"Here is a second verse."}),
        "stop",
    )])
    .await;
    let (stdout, stderr) = repl(cli(root.path(), data.path(), &url), "继续\n/exit\n").await;
    assert!(
        stderr.contains("上次在这里的任务：#1 write a haiku about rust（1 轮，"),
        "{stderr}"
    );
    assert!(stdout.contains("[resume] 回到任务 #1"), "{stdout}");
    assert!(
        stderr.contains("之前的对话") && stderr.contains("> write a haiku about rust"),
        "{stderr}"
    );
    assert!(stderr.contains("Rust ownership haiku."), "{stderr}");
    assert!(stdout.contains("Here is a second verse."), "{stdout}");
    let request = second.await.unwrap()[0]["messages"].to_string();
    assert!(
        request.contains("write a haiku about rust") && request.contains("继续"),
        "{request}"
    );
    // One conversation, not one per start.
    assert!(
        stderr.contains("会话已保存。下次在这里输入“继续”"),
        "{stderr}"
    );
}

#[tokio::test]
async fn a_retyped_unfinished_request_continues_its_task() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "hello").unwrap();
    let (url, _first) = mock_server(vec![
        tool("r", "read", json!({"path":"notes.txt"})),
        openai(json!({"content":"summary: read notes"}), "stop"),
    ])
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--max-turns", "1"]);
    repl(command, "read the notes\n/exit\n").await;
    let (url, second) = mock_server(vec![openai(
        json!({"content":"finished the notes task"}),
        "stop",
    )])
    .await;
    let (stdout, _) = repl(
        cli(root.path(), data.path(), &url),
        "read the notes\n/exit\n",
    )
    .await;
    assert!(
        stdout.contains("未完成的任务 #1；接着做，不从头开始"),
        "{stdout}"
    );
    assert!(stdout.contains("finished the notes task"), "{stdout}");
    let request = second.await.unwrap()[0]["messages"].to_string();
    assert_eq!(request.matches("read the notes").count(), 1, "{request}");
}

#[tokio::test]
async fn slash_continue_in_a_new_session_resumes_the_unfinished_task() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "hello").unwrap();
    let (url, _first) = mock_server(vec![
        tool("r", "read", json!({"path":"notes.txt"})),
        openai(json!({"content":"summary: read notes"}), "stop"),
    ])
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["--max-turns", "1"]);
    repl(command, "read the notes\n/exit\n").await;
    let (url, second) =
        mock_server(vec![openai(json!({"content":"notes task done"}), "stop")]).await;
    let (stdout, stderr) = repl(
        cli(root.path(), data.path(), &url),
        "/continue\n/continue 1\n/exit\n",
    )
    .await;
    assert!(!stderr.contains("no interrupted task"), "{stderr}");
    assert!(
        stdout.contains("最近会话") && stdout.contains("notes task done"),
        "{stdout}"
    );
    assert!(
        second.await.unwrap()[0]["messages"]
            .to_string()
            .contains("read the notes")
    );
}
