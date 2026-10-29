//! Area G1: project commands, diagnostics and CLI behavior, end to end.

mod common;
use common::Launch;
use std::{path::Path, process::Stdio, time::Duration};

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

fn tool(id: &str, name: &str, arguments: Value) -> String {
    openai(
        json!({"tool_calls":[{"index":0,"id":id,"type":"function","function":{"name":name,"arguments":arguments.to_string()}}]}),
        "tool_calls",
    )
}

const NOWHERE: &str = "http://127.0.0.1:9/v1";

fn cove(root: &Path, data: &Path, url: &str) -> Command {
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
    for name in [
        "GIT_AUTHOR_NAME",
        "GIT_COMMITTER_NAME",
        "GIT_AUTHOR_EMAIL",
        "GIT_COMMITTER_EMAIL",
    ] {
        command.env(name, "cove-test@localhost");
    }
    command.kill_on_drop(true);
    command
}

async fn run(mut command: Command) -> (std::process::Output, String, String) {
    let output = tokio::time::timeout(Duration::from_secs(60), command.launch())
        .await
        .unwrap()
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    (output, stdout, stderr)
}

async fn repl(mut command: Command, input: &str) -> (std::process::Output, String, String) {
    command
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    let mut child = command.spawn().unwrap();
    let mut stdin = child.stdin.take().unwrap();
    stdin.write_all(input.as_bytes()).await.unwrap();
    drop(stdin);
    let output = tokio::time::timeout(Duration::from_secs(60), child.wait_with_output())
        .await
        .unwrap()
        .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    (output, stdout, stderr)
}

fn git(root: &Path, args: &[&str]) -> String {
    let output = std::process::Command::new("git")
        .args(args)
        .current_dir(root)
        .env("GIT_AUTHOR_NAME", "t")
        .env("GIT_AUTHOR_EMAIL", "t@localhost")
        .env("GIT_COMMITTER_NAME", "t")
        .env("GIT_COMMITTER_EMAIL", "t@localhost")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8_lossy(&output.stdout).into_owned()
}

fn sessions(data: &Path) -> Vec<Value> {
    std::fs::read_dir(data.join("sessions"))
        .unwrap()
        .flatten()
        .map(|entry| serde_json::from_slice(&std::fs::read(entry.path()).unwrap()).unwrap())
        .collect()
}

#[tokio::test]
async fn help_is_grouped_and_unknown_commands_suggest_without_model_calls() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/help"]);
    let (output, stdout, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.contains("通用\n  /help") && stdout.contains("项目\n  /init"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/help commit"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.starts_with("/commit [--preview]"), "{stdout}");
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/comit all"]);
    let (output, _, stderr) = run(command).await;
    assert_eq!(output.status.code(), Some(1));
    assert!(stderr.contains("你是不是想输入 /commit"), "{stderr}");
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/clear"]);
    let (output, _, stderr) = run(command).await;
    assert!(!output.status.success() && stderr.contains("需要终端"));
}

#[tokio::test]
async fn print_mode_appends_piped_stdin_and_text_attachments() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let notes = root.path().join("notes.txt");
    std::fs::write(&notes, "attached-note-content").unwrap();
    let (url, server) = mock_server(vec![openai(json!({"content":"explained"}), "stop")]).await;
    let mut command = cove(root.path(), data.path(), &url);
    command
        .args(["-p", "explain this"])
        .arg("--file")
        .arg(&notes);
    let (output, stdout, stderr) = repl(command, "\u{feff}piped-log-line\nsecond-line\n").await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.contains("explained"));
    let requests = server.await.unwrap();
    let user = requests[0]["messages"][1]["content"].to_string();
    assert!(
        user.contains("explain this\\n\\npiped-log-line\\nsecond-line"),
        "{user}"
    );
    assert!(!user.contains('\u{feff}') && user.contains("attached-note-content"));
}

#[tokio::test]
async fn exit_codes_distinguish_usage_errors_and_dump_prints_the_system_prompt() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.arg("-p");
    let (output, _, stderr) = run(command).await;
    assert_eq!(output.status.code(), Some(2), "{stderr}");
    assert!(stderr.contains("-p 需要提示词"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.arg("--no-such-flag");
    assert_eq!(run(command).await.0.status.code(), Some(2));
    std::fs::write(root.path().join("CLAUDE.md"), "Project rule: use tabs.").unwrap();
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.arg("--dump-system-prompt");
    let (output, stdout, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.starts_with("You are Cove") && stdout.contains("Project rule: use tabs."));
    assert!(sessions(data.path()).is_empty());
}

#[tokio::test]
async fn doctor_runs_without_credentials_and_reports_codes() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let doctor = || {
        let mut command = Command::new(common::exe());
        command
            .arg("--doctor")
            .arg("--cwd")
            .arg(root.path())
            .arg("--data-dir")
            .arg(data.path());
        for name in [
            "COVE_API_KEY",
            "OPENAI_API_KEY",
            "COVE_PROVIDER",
            "COVE_MODEL",
            "COVE_CONFIG",
            "COVE_BASE_URL",
            "COVE_PROFILE",
        ] {
            command.env_remove(name);
        }
        command
    };
    let config = |key: &str| {
        std::fs::write(
            data.path().join("config.json"),
            json!({"model":"m","provider":{"name":"openai","base_url":NOWHERE,"api_key":key}})
                .to_string(),
        )
        .unwrap();
    };
    config("");
    let (output, stdout, stderr) = run(doctor()).await;
    assert_eq!(output.status.code(), Some(1), "{stdout}{stderr}");
    assert!(stdout.contains("cove 诊断"), "{stdout}");
    assert!(stdout.contains("FAIL] config     E1005"), "{stdout}");
    assert!(stdout.contains("data_dir") && stdout.contains("shell"));
    config("sk-xxxxxxxx");
    let (output, stdout, _) = run(doctor()).await;
    assert_eq!(output.status.code(), Some(1), "{stdout}");
    assert!(stdout.contains("FAIL] api_key    E1007"), "{stdout}");
    assert!(stdout.contains("FAIL] network    E2001"), "{stdout}");
    assert!(!stdout.contains("sk-xxxxxxxx"));
    std::fs::remove_file(data.path().join("config.json")).unwrap();

    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/doctor quick"]);
    let (output, stdout, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.contains("已跳过（快速模式）") && stdout.contains("test****-key"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/doctor codes"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.contains("E2004") && stdout.contains("[network]"));
}

#[tokio::test]
async fn trace_and_error_logs_record_sizes_and_failures_but_no_text() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("secret-plan.txt"), "classified body text").unwrap();
    let (url, server) = mock_server(vec![
        tool("r1", "read", json!({"path":"secret-plan.txt"})),
        openai(json!({"content":"done reading"}), "stop"),
        // No finish event: the provider fails this call.
        format!(
            "data: {}\n\n",
            json!({"choices":[{"delta":{"content":"partial"}}]})
        ),
    ])
    .await;
    let mut command = cove(root.path(), data.path(), &url);
    command.args(["-p", "please read the confidential prompt words"]);
    let (output, _, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stderr.contains("#1"), "{stderr}");
    let mut command = cove(root.path(), data.path(), &url);
    command.args(["-p", "second task"]);
    let (output, _, _) = run(command).await;
    assert!(!output.status.success());
    server.await.unwrap();
    let trace = std::fs::read_to_string(data.path().join("trace.jsonl")).unwrap();
    for kind in ["\"model\"", "\"tool\"", "\"task\""] {
        assert!(trace.contains(kind), "{trace}");
    }
    assert!(!trace.contains("confidential") && !trace.contains("classified"));
    let errors = std::fs::read_to_string(data.path().join("errors.log")).unwrap();
    assert!(errors.contains("\"source\":\"model\""), "{errors}");
    assert!(!errors.contains("test-only-not-a-real-key"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/doctor trace 10"]);
    let (_, stdout, _) = run(command).await;
    assert!(
        stdout.contains("tool    read") && stdout.contains("失败"),
        "{stdout}"
    );
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/doctor errors"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.contains("运行错误"), "{stdout}");
}

#[tokio::test]
async fn init_drafts_with_the_model_and_applies_without_overwriting() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("README.md"), "# Widget\nMakes widgets.\n").unwrap();
    let (url, server) = mock_server(vec![openai(
        json!({"content":"```markdown\n# CLAUDE.md\n\nRun `make test`.\n```"}),
        "stop",
    )])
    .await;
    let mut command = cove(root.path(), data.path(), &url);
    command.args(["-p", "/init"]);
    let (output, stdout, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.contains("+++ CLAUDE.md") && stdout.contains("+Run `make test`."));
    assert!(!root.path().join("CLAUDE.md").exists());
    let requests = server.await.unwrap();
    let digest = requests[0]["messages"][1]["content"].to_string();
    assert!(digest.contains("Makes widgets.") && digest.contains("README.md"));
    assert!(
        requests[0]
            .get("tools")
            .is_none_or(|tools| tools.as_array().is_none_or(Vec::is_empty))
    );

    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/init apply"]);
    let (output, stdout, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.contains("已写入"));
    assert_eq!(
        std::fs::read_to_string(root.path().join("CLAUDE.md")).unwrap(),
        "# CLAUDE.md\n\nRun `make test`.\n"
    );
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/init"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.contains("已存在"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/init apply"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.contains("还没有草稿"));
}

#[tokio::test]
async fn commit_takes_staged_content_or_a_selection_and_never_pushes() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(
        data.path().join("config.json"),
        r#"{"done_verify_commands":["git --version"]}"#,
    )
    .unwrap();
    git(root.path(), &["init", "-q"]);
    std::fs::write(root.path().join("a.txt"), "a1\n").unwrap();
    std::fs::write(root.path().join("b.txt"), "b1\n").unwrap();
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/commit --all first commit"]);
    let (output, stdout, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert!(stdout.contains("2 files changed") && stdout.contains("未推送"));
    assert!(stderr.contains("通过   git --version"), "{stderr}");
    assert_eq!(
        git(root.path(), &["log", "--format=%s"]).trim(),
        "first commit"
    );
    assert!(
        git(root.path(), &["status", "--porcelain"])
            .trim()
            .is_empty()
    );

    std::fs::write(root.path().join("a.txt"), "a2\n").unwrap();
    std::fs::write(root.path().join("b.txt"), "b2\n").unwrap();
    git(root.path(), &["add", "a.txt"]);
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/diff git"]);
    let (_, stdout, _) = run(command).await;
    assert!(
        stdout.contains("# Staged") && stdout.contains("# Unstaged"),
        "{stdout}"
    );
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/diff git --stat"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.contains("a.txt") && stdout.contains("b.txt"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/commit --preview"]);
    let (_, stdout, _) = run(command).await;
    assert!(stdout.contains("未创建提交") && !stdout.contains("b.txt"));
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/commit"]);
    let (output, _, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert_eq!(
        git(root.path(), &["log", "-1", "--format=%s"]).trim(),
        "auto-commit"
    );
    assert_eq!(
        git(root.path(), &["status", "--porcelain"]).trim(),
        "M b.txt"
    );

    std::fs::write(root.path().join("a.txt"), "a3\n").unwrap();
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/commit --only b.txt -- only b"]);
    let (output, _, stderr) = run(command).await;
    assert!(output.status.success(), "{stderr}");
    assert_eq!(
        git(root.path(), &["show", "--name-only", "--format=%s", "HEAD"]).trim(),
        "only b\n\nb.txt"
    );
    // The user's index learned the committed b.txt; a.txt stays unstaged.
    assert_eq!(
        git(root.path(), &["status", "--porcelain"]).trim(),
        "M a.txt"
    );
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-p", "/commit --only ../x --"]);
    let (output, _, stderr) = run(command).await;
    assert!(!output.status.success() && stderr.contains("必须在仓库内"));
}

#[tokio::test]
async fn expand_shows_the_full_output_of_a_shortened_tool_result() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let body: String = (1..=40).map(|n| format!("row-{n}\n")).collect();
    std::fs::write(root.path().join("long.txt"), body).unwrap();
    let (url, server) = mock_server(vec![
        tool("r1", "read", json!({"path":"long.txt"})),
        openai(json!({"content":"read it"}), "stop"),
    ])
    .await;
    let (output, stdout, stderr) = repl(
        cove(root.path(), data.path(), &url),
        "read the file\n/expand\n/expand 1 all\n/expand 99\n/exit\n",
    )
    .await;
    assert!(output.status.success(), "{stderr}");
    assert!(stderr.contains("行（/expand 1）"), "{stderr}");
    assert!(!stderr.contains("row-40"));
    assert!(
        stdout.contains("#1 read [ok]") && stdout.contains("row-40"),
        "{stdout}"
    );
    assert!(stdout.contains("没有 #99"));
    server.await.unwrap();
}

#[tokio::test]
async fn cd_and_restart_start_the_binary_again() {
    let root = tempfile::tempdir().unwrap();
    let other = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (output, _, stderr) = repl(
        cove(root.path(), data.path(), NOWHERE),
        &format!("/cd {}\n", other.path().display()),
    )
    .await;
    assert!(output.status.success(), "{stderr}");
    assert!(stderr.contains("[cd] 正在"), "{stderr}");
    assert_eq!(stderr.matches("[session]").count(), 2, "{stderr}");
    let other_root = other.path().canonicalize().unwrap();
    assert!(
        sessions(data.path())
            .iter()
            .any(|session| Path::new(session["cwd"].as_str().unwrap()) == other_root),
        "{stderr}"
    );

    let data = tempfile::tempdir().unwrap();
    let (output, _, stderr) = repl(cove(root.path(), data.path(), NOWHERE), "/restart\n").await;
    assert!(output.status.success(), "{stderr}");
    let ids: Vec<&str> = stderr
        .lines()
        .filter_map(|line| line.strip_prefix("[session] "))
        .collect();
    assert_eq!(ids.len(), 2, "{stderr}");
    assert_eq!(ids[0], ids[1]);
    assert_eq!(sessions(data.path()).len(), 1);
    // -r is --resume.
    let mut command = cove(root.path(), data.path(), NOWHERE);
    command.args(["-r", ids[0], "-p", "/status"]);
    let (output, stdout, _) = run(command).await;
    assert!(output.status.success() && stdout.contains(ids[0]));
}
