//! CLI-level tests for area E: skills, plugin commands, hooks.json, MCP over HTTP, trust.

mod common;
use common::Launch;
use std::{path::Path, time::Duration};

use serde_json::{Value, json};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    process::Command,
};

async fn read_request(socket: &mut TcpStream) -> Option<(String, Vec<u8>)> {
    let mut request = Vec::new();
    let body_start;
    loop {
        let mut chunk = [0_u8; 4096];
        let size = socket.read(&mut chunk).await.ok()?;
        if size == 0 {
            return None;
        }
        request.extend_from_slice(&chunk[..size]);
        if let Some(end) = request.windows(4).position(|part| part == b"\r\n\r\n") {
            body_start = end + 4;
            break;
        }
    }
    let headers = String::from_utf8_lossy(&request[..body_start]).into_owned();
    let length: usize = headers
        .lines()
        .find_map(|line| {
            let (name, value) = line.split_once(':')?;
            name.eq_ignore_ascii_case("content-length")
                .then(|| value.trim().parse().unwrap())
        })
        .unwrap_or(0);
    while request.len() < body_start + length {
        let mut chunk = [0_u8; 4096];
        let size = socket.read(&mut chunk).await.ok()?;
        if size == 0 {
            return None;
        }
        request.extend_from_slice(&chunk[..size]);
    }
    Some((headers, request[body_start..body_start + length].to_vec()))
}

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
            let (_, body) = read_request(&mut socket).await.unwrap();
            requests.push(serde_json::from_slice(&body).unwrap());
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

fn done(text: &str) -> String {
    openai(json!({"content": text}), "stop")
}

/// A CLI with private data and home directories, so the user's own skills never leak in.
fn cli(root: &Path, data: &Path, home: &Path, url: &str) -> Command {
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        "openai",
        "--model",
        "mock-model",
        "--base-url",
        url,
        "--permission",
        "auto",
        "--timeout",
        "30",
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.env("HOME", home).env("USERPROFILE", home);
    command.kill_on_drop(true);
    command
}

/// A command line that runs Python in the hook shell (a full path on Windows, where the
/// hook's scrubbed environment cannot use the WindowsApps alias).
fn python() -> String {
    if !cfg!(windows) {
        return "python3".into();
    }
    std::env::split_paths(&std::env::var_os("PATH").unwrap_or_default())
        .map(|dir| dir.join("python.exe"))
        .find(|path| path.is_file() && !path.to_string_lossy().contains("WindowsApps"))
        .map_or("python".into(), |path| format!("& '{}'", path.display()))
}

fn write(path: &Path, text: &str) {
    std::fs::create_dir_all(path.parent().unwrap()).unwrap();
    std::fs::write(path, text).unwrap();
}

#[tokio::test]
async fn slash_skill_runs_as_a_task_with_steps_and_builtin_index() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    write(
        &root.path().join(".cove/skills/release/SKILL.md"),
        "---\nname: release\ndescription: Cut a release\nallowed_tools: [read, shell]\nsteps:\n  - Run the tests, then fix failures\n  - Tag the version\n---\nNever push tags without asking.\n",
    );
    let (url, server) = mock_server(vec![done("Release steps followed.")]).await;
    let output = cli(root.path(), data.path(), home.path(), &url)
        .args(["-p", "/release v1.2"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let request = server.await.unwrap()[0].to_string();
    assert!(request.contains("[Skill: release]"), "{request}");
    assert!(request.contains("1. Run the tests, then fix failures"));
    assert!(request.contains("only use these tools: read, shell"));
    assert!(request.contains("User arguments: v1.2"));
    // Built-in skills are in the system prompt index.
    assert!(request.contains("systematic-debugging"));
}

#[tokio::test]
async fn unknown_commands_suggest_and_plugin_commands_expand_arguments() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    let output = cli(
        root.path(),
        data.path(),
        home.path(),
        "http://127.0.0.1:9/v1",
    )
    .args(["-p", "/stauts"])
    .launch()
    .await
    .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("你是不是想输入 /status？"));

    let plugin = data.path().join("plugins/demo");
    write(
        &plugin.join("manifest.json"),
        r#"{"name":"demo","version":"1.0.0","description":"Demo"}"#,
    );
    write(
        &plugin.join("commands/review.md"),
        "---\ndescription: Review a file\n---\nReview $ARGUMENTS carefully.\n",
    );
    write(
        &plugin.join("skills/demo-skill/SKILL.md"),
        "---\ndescription: Demo plugin skill\n---\nPlugin body.\n",
    );
    let output = cli(
        root.path(),
        data.path(),
        home.path(),
        "http://127.0.0.1:9/v1",
    )
    .args(["-p", "/config plugins"])
    .launch()
    .await
    .unwrap();
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(
        stdout.contains("\"/review\"") && stdout.contains("\"demo\""),
        "{stdout}"
    );
    let (url, server) = mock_server(vec![done("Reviewed.")]).await;
    let output = cli(root.path(), data.path(), home.path(), &url)
        .args(["-p", "/review src/lib.rs"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let request = server.await.unwrap()[0].to_string();
    assert!(
        request.contains("Review src/lib.rs carefully."),
        "{request}"
    );
    assert!(request.contains("demo-skill"));
}

#[tokio::test]
async fn project_config_needs_trust_for_sensitive_keys() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    write(
        &root.path().join(".cove.json"),
        r#"{"system_prompt":"PROJECT-PROMPT-MARKER","disabled_skills":["spike"],"permission_mode":"bypass"}"#,
    );
    let (url, server) = mock_server(vec![done("ok")]).await;
    let output = cli(root.path(), data.path(), home.path(), &url)
        .args(["-p", "hello"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(output.status.success(), "{stderr}");
    assert!(
        stderr.contains("已忽略未受信任的键：system_prompt, permission_mode")
            || stderr.contains("已忽略未受信任的键：permission_mode, system_prompt"),
        "{stderr}"
    );
    let request = server.await.unwrap()[0].to_string();
    assert!(!request.contains("PROJECT-PROMPT-MARKER"));
    assert!(
        !request.contains("- spike:"),
        "disabled_skills is a safe key"
    );

    let output = cli(
        root.path(),
        data.path(),
        home.path(),
        "http://127.0.0.1:9/v1",
    )
    .args(["-p", "/config trust"])
    .launch()
    .await
    .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let store = std::fs::read_to_string(data.path().join("trusted_projects.json")).unwrap();
    assert!(
        store.contains("dir:") && store.contains("\"trusted\""),
        "{store}"
    );

    let (url, server) = mock_server(vec![done("ok")]).await;
    let output = cli(root.path(), data.path(), home.path(), &url)
        .args(["-p", "hello"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(
        server.await.unwrap()[0]
            .to_string()
            .contains("PROJECT-PROMPT-MARKER")
    );

    // An edited file is untrusted again.
    write(
        &root.path().join(".cove.json"),
        r#"{"system_prompt":"EDITED"}"#,
    );
    let output = cli(
        root.path(),
        data.path(),
        home.path(),
        "http://127.0.0.1:9/v1",
    )
    .args(["-p", "/config skills"])
    .launch()
    .await
    .unwrap();
    assert!(String::from_utf8_lossy(&output.stderr).contains("已忽略未受信任的键：system_prompt"));
}

#[tokio::test]
async fn hooks_json_pre_tool_use_blocks_by_json_and_exit_code_two() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    let script = "import sys,json; d=json.load(sys.stdin); p=d['tool_input']['path']; ok=d['hook_event_name']=='PreToolUse'; print(json.dumps({'continue': False, 'message': 'no '+p})) if p=='a.txt' and ok else (sys.stderr.write('denied '+p), sys.exit(2))";
    write(
        &data.path().join("hooks.json"),
        &json!({"hooks":{"PreToolUse":[{"matcher":"Write","command":format!("{} -c \"{script}\"", python())}]}}).to_string(),
    );
    let (url, server) = mock_server(vec![
        tool("w1", "write", json!({"path":"a.txt","content":"x"})),
        tool("w2", "write", json!({"path":"b.txt","content":"y"})),
        tool("r1", "read", json!({"path":"."})),
        done("Both writes were blocked."),
    ])
    .await;
    let output = cli(root.path(), data.path(), home.path(), &url)
        .args(["--trust-extensions", "-p", "write files"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    let mut stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    for entry in std::fs::read_dir(data.path().join("sessions")).unwrap() {
        let text = std::fs::read_to_string(entry.unwrap().path()).unwrap_or_default();
        if let Ok(session) = serde_json::from_str::<Value>(&text) {
            stderr.push_str(&session["hook_runs"].to_string());
        }
    }
    assert!(
        requests[1]
            .to_string()
            .contains("Blocked by hook hooks.json/PreToolUse-0: no a.txt"),
        "{stderr} {}",
        requests[1]
    );
    assert!(
        requests[2].to_string().contains("denied b.txt"),
        "{}",
        requests[2]
    );
    assert!(!root.path().join("a.txt").exists() && !root.path().join("b.txt").exists());
}

/// Minimal streamable-HTTP MCP server with one `echo` tool.
async fn mcp_server() -> (String, tokio::task::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        loop {
            let Ok((mut socket, _)) = listener.accept().await else {
                return;
            };
            tokio::spawn(async move {
                while let Some((headers, body)) = read_request(&mut socket).await {
                    if headers.starts_with("DELETE") {
                        let _ = socket
                            .write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
                            .await;
                        continue;
                    }
                    let message: Value = serde_json::from_slice(&body).unwrap_or_default();
                    let result = match message["method"].as_str().unwrap_or_default() {
                        "initialize" => {
                            json!({"protocolVersion":message["params"]["protocolVersion"],
                            "capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"mock","version":"1"}})
                        }
                        "tools/list" => {
                            json!({"tools":[{"name":"echo","description":"Echo text back",
                            "inputSchema":{"type":"object","properties":{"text":{"type":"string"}}}}]})
                        }
                        "tools/call" => {
                            json!({"content":[{"type":"text","text":format!("echoed {}", message["params"]["arguments"]["text"].as_str().unwrap_or_default())}]})
                        }
                        _ => {
                            let _ = socket
                                .write_all(b"HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\n\r\n")
                                .await;
                            continue;
                        }
                    };
                    let reply =
                        json!({"jsonrpc":"2.0","id":message["id"],"result":result}).to_string();
                    let header = format!(
                        "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nMcp-Session-Id: s-1\r\nContent-Length: {}\r\n\r\n",
                        reply.len()
                    );
                    let _ = socket.write_all(header.as_bytes()).await;
                    let _ = socket.write_all(reply.as_bytes()).await;
                }
            });
        }
    });
    (format!("http://{address}/mcp"), task)
}

#[tokio::test]
async fn go_mcp_servers_over_streamable_http_list_tools_in_description() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    let (mcp_url, mcp) = mcp_server().await;
    let config = data.path().join("config.json");
    write(&config, &json!({"mcp_servers":{"remote":{"type":"http","url":mcp_url,"env":{"TOKEN":"never-persist-me"}}}}).to_string());
    let (url, server) = mock_server(vec![
        tool(
            "m1",
            "mcp",
            json!({"action":"call","server":"remote","tool":"echo","arguments":{"text":"hi"}}),
        ),
        done("Echo worked."),
    ])
    .await;
    let output = cli(root.path(), data.path(), home.path(), &url)
        .arg("--config")
        .arg(&config)
        .args(["--trust-extensions", "-p", "use the echo tool"])
        .launch()
        .await
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.await.unwrap();
    let second = requests[1].to_string();
    assert!(second.contains("echoed hi"), "{second}");
    assert!(second.contains("remote/echo: Echo text back"), "{second}");
    assert!(!requests[0].to_string().contains("remote/echo"));
    for entry in std::fs::read_dir(data.path().join("sessions")).unwrap() {
        let text = std::fs::read_to_string(entry.unwrap().path()).unwrap_or_default();
        assert!(!text.contains("never-persist-me"));
    }
    mcp.abort();

    // Without trust the server is listed but never contacted.
    let output = cli(
        root.path(),
        data.path(),
        home.path(),
        "http://127.0.0.1:9/v1",
    )
    .arg("--config")
    .arg(&config)
    .args(["-p", "/config mcp connect remote"])
    .launch()
    .await
    .unwrap();
    assert!(!output.status.success());
    assert!(String::from_utf8_lossy(&output.stderr).contains("--trust-extensions"));
}

#[tokio::test]
async fn config_skills_create_show_and_list() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    for command in [
        "/config skills create my-flow",
        "/config skills show my-flow",
        "/config skills list",
    ] {
        let output = cli(
            root.path(),
            data.path(),
            home.path(),
            "http://127.0.0.1:9/v1",
        )
        .args(["-p", command])
        .launch()
        .await
        .unwrap();
        assert!(
            output.status.success(),
            "{command}: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        let stdout = String::from_utf8_lossy(&output.stdout);
        assert!(stdout.contains("my-flow"), "{command}: {stdout}");
        if command.ends_with("list") {
            assert!(stdout.contains("\"builtin\""));
        }
    }
    assert!(data.path().join("skills/my-flow/SKILL.md").is_file());
    let output = cli(
        root.path(),
        data.path(),
        home.path(),
        "http://127.0.0.1:9/v1",
    )
    .args(["-p", "/config skills create my-flow"])
    .launch()
    .await
    .unwrap();
    assert!(!output.status.success());
}

/// One `-p` command with everything allowed (installing writes the data directory).
async fn command_output(
    root: &Path,
    data: &Path,
    home: &Path,
    line: &str,
) -> (bool, String, String) {
    let mut command = Command::new(common::exe());
    command
        .args(["--provider", "openai", "--model", "mock-model"])
        .args([
            "--base-url",
            "http://127.0.0.1:9/v1",
            "--permission",
            "bypass",
        ])
        .arg("--cwd")
        .arg(root)
        .arg("--data-dir")
        .arg(data)
        .args(["-p", line])
        .env("COVE_API_KEY", "test-only-not-a-real-key")
        .env("HOME", home)
        .env("USERPROFILE", home)
        .kill_on_drop(true);
    let output = command.launch().await.unwrap();
    (
        output.status.success(),
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

#[tokio::test]
async fn the_built_in_marketplace_lists_and_installs_offline() {
    let (root, data, home) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    let (ok, stdout, stderr) = command_output(
        root.path(),
        data.path(),
        home.path(),
        "/config skills marketplace dock",
    )
    .await;
    assert!(ok, "{stderr}");
    assert!(
        stdout.contains("\"dockerize\"") && !stdout.contains("security-audit"),
        "{stdout}"
    );
    let (ok, stdout, stderr) = command_output(
        root.path(),
        data.path(),
        home.path(),
        "/config skills install dockerize",
    )
    .await;
    assert!(ok, "{stderr}");
    assert!(
        stdout.contains("built-in marketplace: skills/dockerize/SKILL.md"),
        "{stdout}"
    );
    let installed = std::fs::read_to_string(data.path().join("skills/dockerize/SKILL.md")).unwrap();
    assert!(installed.contains("name: dockerize"));
}

#[tokio::test]
async fn a_configured_local_marketplace_replaces_the_built_in_one() {
    let (root, data, home, market) = (
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
        tempfile::tempdir().unwrap(),
    );
    write(
        &market.path().join("registry.json"),
        &json!([
            {"name": "team-style", "description": "Our style guide", "url": "skills/team-style/SKILL.md"},
            {"name": "escape", "description": "bad", "url": "../outside/SKILL.md"}
        ])
        .to_string(),
    );
    write(
        &market.path().join("skills/team-style/SKILL.md"),
        "---\nname: team-style\ndescription: Our style guide\n---\nUse tabs.\n",
    );
    write(
        &data.path().join("config.json"),
        &json!({"skills_registry": market.path()}).to_string(),
    );
    let (ok, stdout, stderr) = command_output(
        root.path(),
        data.path(),
        home.path(),
        "/config skills marketplace",
    )
    .await;
    assert!(ok, "{stderr}");
    assert!(
        stdout.contains("team-style") && !stdout.contains("dockerize"),
        "{stdout}"
    );
    let (ok, _, stderr) = command_output(
        root.path(),
        data.path(),
        home.path(),
        "/config skills install team-style",
    )
    .await;
    assert!(ok, "{stderr}");
    assert!(data.path().join("skills/team-style/SKILL.md").is_file());
    let (ok, _, stderr) = command_output(
        root.path(),
        data.path(),
        home.path(),
        "/config skills install escape",
    )
    .await;
    assert!(!ok && stderr.contains("市场内的路径"), "{stderr}");
}
