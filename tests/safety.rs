//! CLI tests for permission modes, safety blocks, policies, guards and completion gates.

mod common;
use common::Launch;
use std::{path::Path, time::Duration};

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
            let Ok(Ok((mut socket, _))) =
                tokio::time::timeout(Duration::from_secs(20), listener.accept()).await
            else {
                break;
            };
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

fn text(content: &str) -> String {
    openai(json!({ "content": content }), "stop")
}

fn cli(root: &Path, data: &Path, url: &str, mode: &str) -> Command {
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        "openai",
        "--model",
        "mock-model",
        "--base-url",
        url,
        "--timeout",
        "30",
        "--permission",
        mode,
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command
        .env_remove("COVE_CONFIG")
        .env_remove("COVE_PROFILE");
    command.kill_on_drop(true);
    command
}

fn python() -> &'static str {
    if cfg!(windows) { "python" } else { "python3" }
}

fn tool_result(request: &Value) -> String {
    request.to_string()
}

#[tokio::test]
async fn catastrophic_commands_are_refused_even_in_bypass_mode() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool("rm", "shell", json!({"command":"rm -rf ~"})),
        text("I will not run that; the request was unsafe and nothing was changed."),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "clean up"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    assert_eq!(
        requests.len(),
        2,
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let result = tool_result(&requests[1]);
    assert!(result.contains("blocked in every mode"), "{result}");
}

#[tokio::test]
async fn auto_mode_allows_project_edits_but_asks_for_unknown_shell() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool("w", "write", json!({"path":"a.txt","content":"auto"})),
        tool("s", "shell", json!({"command":"echo x > b.txt"})),
        text("unreachable"),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["-p", "write"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    server.abort();
    assert_eq!(
        std::fs::read_to_string(root.path().join("a.txt")).unwrap(),
        "auto"
    );
    assert!(!root.path().join("b.txt").exists());
    assert!(!output.status.success());
    assert!(
        stderr.contains("--permission auto") && stderr.contains("bypass"),
        "{stderr}"
    );
}

#[tokio::test]
async fn default_mode_runs_read_only_shell_without_asking() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("visible.txt"), "x").unwrap();
    let (url, server) = mock_server(vec![
        tool("ls", "shell", json!({"command":"ls"})),
        text("Listed the directory: visible.txt is there."),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "default")
        .args(["-p", "list files"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(tool_result(&requests[1]).contains("visible.txt"));
}

#[tokio::test]
async fn policy_deny_rules_refuse_and_the_model_continues() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(
        data.path().join("policies.json"),
        json!([{"id":"no-lock","tool_pattern":"write","action":"deny","param_match":{"path":"*.lock"}}]).to_string(),
    )
    .unwrap();
    let (url, server) = mock_server(vec![
        tool("w", "write", json!({"path":"deps.lock","content":"x"})),
        text("The lock file is protected by policy, so I left it unchanged."),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "edit lock"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    assert!(!root.path().join("deps.lock").exists());
    assert!(
        tool_result(&requests[1]).contains("denied by policy rule"),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
}

#[tokio::test]
async fn plan_mode_refuses_changes_with_guidance() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool("w", "write", json!({"path":"a.txt","content":"x"})),
        text("Plan: create a.txt with the requested content once plan mode is lifted."),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "plan")
        .args(["-p", "make a file"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    assert!(!root.path().join("a.txt").exists());
    assert!(
        tool_result(&requests[1]).contains("plan mode"),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
}

#[tokio::test]
async fn guardrail_blocks_an_exact_call_that_keeps_failing() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let mut responses: Vec<String> = (0..6)
        .map(|i| tool(&format!("r{i}"), "read", json!({"path":"missing.txt"})))
        .collect();
    responses.push(text(
        "The file does not exist, so there is nothing to read.",
    ));
    let (url, server) = mock_server(responses).await;
    let _ = cli(root.path(), data.path(), &url, "default")
        .args(["-p", "read it"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 7);
    assert!(tool_result(&requests[3]).contains("[guardrail]"));
    assert!(tool_result(&requests[6]).contains("blocked"));
}

#[tokio::test]
async fn verification_reuses_the_models_own_passing_check_and_records_acceptance() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let check = format!(
        "{} -c \"import sys; sys.exit(0 if open('notes.txt').read() == 'fixed' else 3)\"",
        python()
    );
    std::fs::write(
        data.path().join("config.json"),
        json!({"done_verify_commands":[check]}).to_string(),
    )
    .unwrap();
    let (url, server) = mock_server(vec![
        tool("w", "write", json!({"path":"notes.txt","content":"fixed"})),
        tool("c", "shell", json!({"command":check})),
        text("Wrote notes.txt and the check passed."),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "fix notes"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    server.await.unwrap();
    assert!(output.status.success(), "{stderr}");
    assert!(
        stderr.contains("already run successfully after the last edit"),
        "{stderr}"
    );
    let ledger = std::fs::read_to_string(data.path().join("verify_ledger.jsonl")).unwrap();
    assert!(ledger.contains("\"reused\""), "{ledger}");
    let session = std::fs::read_dir(data.path().join("acceptance"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let id = session.file_stem().unwrap().to_string_lossy().into_owned();
    let (url, _server) = mock_server(vec![]).await;
    let status = cli(root.path(), data.path(), &url, "default")
        .args(["--resume", &id, "-p", "/status acceptance"])
        .launch()
        .await
        .unwrap();
    let stdout = String::from_utf8_lossy(&status.stdout);
    assert!(
        stdout.contains("verification: passed") && stdout.contains("reused"),
        "{stdout}{}",
        String::from_utf8_lossy(&status.stderr)
    );
}

#[tokio::test]
async fn secrets_in_the_prompt_are_warned_about() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![text("Noted.")]).await;
    let output = cli(root.path(), data.path(), &url, "default")
        .args(["-p", "use key sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD"])
        .launch()
        .await
        .unwrap();
    server.await.unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(
        stderr.contains("[safety] 警告")
            && !stderr.contains("sk-abcdefghijklmnopqrstuvwxyz0123456789ABCD"),
        "{stderr}"
    );
}

#[tokio::test]
async fn mode_command_saves_the_mode_to_the_config() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, _server) = mock_server(vec![]).await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "/mode auto save"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let config: Value =
        serde_json::from_str(&std::fs::read_to_string(data.path().join("config.json")).unwrap())
            .unwrap();
    assert_eq!(config["permission_mode"], "auto");
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "/mode"])
        .launch()
        .await
        .unwrap();
    assert!(String::from_utf8_lossy(&output.stdout).contains("权限模式：bypass"));
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "/mode review"])
        .launch()
        .await
        .unwrap();
    assert!(
        !output.status.success(),
        "review needs an interactive terminal"
    );
}
