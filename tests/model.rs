//! Model capabilities through the CLI: routing, fallback, key pool, live
//! switching, attachments, images, caching and context-overflow recovery.

mod common;
use std::{path::Path, time::Duration};

use serde_json::{Value, json};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    process::Command,
};

struct Reply {
    status: u16,
    headers: Vec<(&'static str, String)>,
    body: String,
}

fn ok(body: String) -> Reply {
    Reply {
        status: 200,
        headers: Vec::new(),
        body,
    }
}

fn status(status: u16, body: &str) -> Reply {
    Reply {
        status,
        headers: Vec::new(),
        body: body.into(),
    }
}

struct Seen {
    headers: String,
    body: Value,
}

async fn mock_server(replies: Vec<Reply>) -> (String, tokio::task::JoinHandle<Vec<Seen>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let task = tokio::spawn(async move {
        let mut seen = Vec::new();
        for reply in replies {
            let (mut socket, _) = tokio::time::timeout(Duration::from_secs(30), listener.accept())
                .await
                .unwrap()
                .unwrap();
            let mut request = Vec::new();
            let body_start;
            loop {
                let mut chunk = [0_u8; 8192];
                let size = socket.read(&mut chunk).await.unwrap();
                assert!(size > 0);
                request.extend_from_slice(&chunk[..size]);
                if let Some(end) = request.windows(4).position(|part| part == b"\r\n\r\n") {
                    body_start = end + 4;
                    break;
                }
            }
            let headers = std::str::from_utf8(&request[..body_start])
                .unwrap()
                .to_ascii_lowercase();
            let length: usize = headers
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    (name == "content-length").then(|| value.trim().parse().unwrap())
                })
                .unwrap();
            while request.len() < body_start + length {
                let mut chunk = [0_u8; 8192];
                let size = socket.read(&mut chunk).await.unwrap();
                assert!(size > 0);
                request.extend_from_slice(&chunk[..size]);
            }
            seen.push(Seen {
                headers,
                body: serde_json::from_slice(&request[body_start..body_start + length]).unwrap(),
            });
            let content_type = if reply.status == 200 {
                "text/event-stream"
            } else {
                "application/json"
            };
            let mut head = format!(
                "HTTP/1.1 {} X\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n",
                reply.status,
                reply.body.len()
            );
            for (name, value) in &reply.headers {
                head.push_str(&format!("{name}: {value}\r\n"));
            }
            head.push_str("\r\n");
            socket.write_all(head.as_bytes()).await.unwrap();
            socket.write_all(reply.body.as_bytes()).await.unwrap();
            socket.shutdown().await.unwrap();
        }
        seen
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

fn text(content: &str) -> Reply {
    ok(openai(json!({"content":content}), "stop"))
}

/// A cove command with an isolated data directory and no inherited settings.
fn cove(root: &Path, data: &Path) -> Command {
    let mut command = Command::new(common::exe());
    command
        .args(["--permission", "auto", "--timeout", "30"])
        .arg("--cwd")
        .arg(root)
        .arg("--data-dir")
        .arg(data);
    for name in [
        "COVE_API_KEY",
        "COVE_MODEL",
        "COVE_PROVIDER",
        "COVE_BASE_URL",
        "COVE_PROFILE",
        "COVE_CONFIG",
        "OPENAI_API_KEY",
        "ANTHROPIC_API_KEY",
        "DEEPSEEK_API_KEY",
    ] {
        command.env_remove(name);
    }
    command.env("COVE_NO_SETUP", "1");
    command.kill_on_drop(true);
    command
}

fn write_config(data: &Path, config: Value) {
    std::fs::write(data.join("config.json"), config.to_string()).unwrap();
}

async fn run(mut command: Command, stdin: Option<&str>) -> std::process::Output {
    command
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped());
    let mut child = command.spawn().unwrap();
    let mut input = child.stdin.take().unwrap();
    if let Some(text) = stdin {
        input.write_all(text.as_bytes()).await.unwrap();
    }
    drop(input);
    tokio::time::timeout(Duration::from_secs(60), child.wait_with_output())
        .await
        .unwrap()
        .unwrap()
}

fn assert_success(output: &std::process::Output) {
    assert!(
        output.status.success(),
        "stdout: {}\nstderr: {}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
}

fn png(width: u32, height: u32) -> Vec<u8> {
    let image = image::DynamicImage::ImageRgb8(image::RgbImage::from_pixel(
        width,
        height,
        image::Rgb([1, 2, 3]),
    ));
    let mut bytes = Vec::new();
    image
        .write_to(
            &mut std::io::Cursor::new(&mut bytes),
            image::ImageFormat::Png,
        )
        .unwrap();
    bytes
}

fn routed_config(url: &str) -> Value {
    json!({
        "model": "premium-model",
        "model_fast": "fast-model",
        "provider": {"name": "openai", "api_key": "k-test", "base_url": url}
    })
}

#[tokio::test]
async fn simple_tasks_route_to_the_fast_model_and_complex_ones_to_premium() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![text("listed"), text("refactored")]).await;
    write_config(data.path(), routed_config(&url));
    let output = run(
        cove(root.path(), data.path()),
        Some("list the files\nrefactor the parser module\n/exit\n"),
    )
    .await;
    assert_success(&output);
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(stderr.contains("[model] fast-model"), "{stderr}");
    let seen = server.await.unwrap();
    assert_eq!(seen[0].body["model"], "fast-model");
    assert_eq!(seen[1].body["model"], "premium-model");
}

#[tokio::test]
async fn a_missing_fast_model_falls_back_to_the_main_model() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        status(
            404,
            r#"{"error":{"message":"model fast-model does not exist"}}"#,
        ),
        text("answered by premium"),
    ])
    .await;
    write_config(data.path(), routed_config(&url));
    let mut command = cove(root.path(), data.path());
    command.args(["-p", "say hi"]);
    let output = run(command, None).await;
    assert_success(&output);
    assert!(String::from_utf8_lossy(&output.stderr).contains("改用 premium-model 继续"));
    let seen = server.await.unwrap();
    assert_eq!(seen[0].body["model"], "fast-model");
    assert_eq!(seen[1].body["model"], "premium-model");
}

#[tokio::test]
async fn the_key_pool_skips_rejected_and_rate_limited_keys() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        status(401, r#"{"error":{"message":"invalid key"}}"#),
        Reply {
            status: 429,
            headers: vec![("retry-after", "30".into())],
            body: r#"{"error":{"message":"slow down"}}"#.into(),
        },
        text("third key works"),
    ])
    .await;
    write_config(
        data.path(),
        json!({"model":"m","provider":{"name":"openai","api_keys":["key-one","key-two","key-three"],"base_url":url}}),
    );
    let mut command = cove(root.path(), data.path());
    command.args(["-p", "hello"]);
    let started = std::time::Instant::now();
    let output = run(command, None).await;
    assert_success(&output);
    // No 30 s wait: the rate-limited key rests while another key is used.
    assert!(started.elapsed() < Duration::from_secs(25));
    let seen = server.await.unwrap();
    assert!(seen[0].headers.contains("authorization: bearer key-one"));
    assert!(seen[1].headers.contains("authorization: bearer key-two"));
    assert!(seen[2].headers.contains("authorization: bearer key-three"));
}

#[tokio::test]
async fn live_model_and_config_changes_apply_and_are_saved() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![text("one"), text("two")]).await;
    let mut command = cove(root.path(), data.path());
    command
        .args([
            "--provider",
            "openai",
            "--model",
            "start-model",
            "--base-url",
            &url,
        ])
        .env("COVE_API_KEY", "env-key-123456789");
    let output = run(
        command,
        Some("/model other-model\nfirst\n/config max_output_tokens 321\n/config system Prefer tabs.\n/config api_key\n/config budget\nsecond\n/model\n/exit\n"),
    )
    .await;
    assert_success(&output);
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(stdout.contains("env-****6789"), "{stdout}");
    assert!(!stdout.contains("env-key-123456789"));
    let seen = server.await.unwrap();
    assert_eq!(seen[0].body["model"], "other-model");
    assert_eq!(seen[1].body["max_tokens"], 321);
    assert!(seen[1].body.to_string().contains("Prefer tabs."));
    let saved: Value =
        serde_json::from_slice(&std::fs::read(data.path().join("config.json")).unwrap()).unwrap();
    assert_eq!(saved["model"], "other-model");
    assert_eq!(saved["max_output_tokens"], 321);
    assert_eq!(saved["system_prompt"], "Prefer tabs.");
    assert_eq!(saved["provider"]["name"], "openai");
    // The environment key is never written to the file.
    assert!(saved["provider"].get("api_key").is_none());
}

#[tokio::test]
async fn profiles_can_be_saved_switched_and_shown_redacted() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![text("from work")]).await;
    write_config(
        data.path(),
        json!({
            "model": "home-model",
            "provider": {"name": "openai", "api_key": "secret-key-0123456789", "base_url": url},
            "profiles": {"work": {"model": "work-model", "provider": {"name": "openai", "api_key": "work-key-0123456789", "base_url": url}}}
        }),
    );
    let output = run(
        cove(root.path(), data.path()),
        Some("/model profile save copy\n/model profile show copy\n/model profile switch work\n/model profile list\nhello\n/exit\n"),
    )
    .await;
    assert_success(&output);
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(
        stdout.contains("secr****6789") && !stdout.contains("secret-key-0123456789"),
        "{stdout}"
    );
    assert!(stdout.contains("* work"), "{stdout}");
    let seen = server.await.unwrap();
    assert_eq!(seen[0].body["model"], "work-model");
    assert!(seen[0].headers.contains("bearer work-key-0123456789"));
    let saved: Value =
        serde_json::from_slice(&std::fs::read(data.path().join("config.json")).unwrap()).unwrap();
    assert_eq!(saved["active_profile"], "work");
    assert_eq!(saved["profiles"]["copy"]["model"], "home-model");
}

#[tokio::test]
async fn attachments_send_images_and_text_files_once() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("shot.png"), png(4, 4)).unwrap();
    std::fs::write(root.path().join("notes.txt"), "remember the milk").unwrap();
    let (url, server) = mock_server(vec![text("seen"), text("plain")]).await;
    let mut command = cove(root.path(), data.path());
    command
        .args([
            "--provider",
            "openai",
            "--model",
            "gpt-4o",
            "--base-url",
            &url,
        ])
        .env("COVE_API_KEY", "k");
    let output = run(
        command,
        Some("/attach notes.txt\n/attach list\ndescribe @shot.png please\nand again\n/exit\n"),
    )
    .await;
    assert_success(&output);
    let seen = server.await.unwrap();
    let user = &seen[0].body["messages"][1];
    let parts = user["content"].as_array().unwrap();
    assert!(
        parts[0]["text"]
            .as_str()
            .unwrap()
            .starts_with("describe please")
    );
    let all = user.to_string();
    assert!(all.contains("remember the milk"));
    assert!(all.contains("data:image/png;base64,"));
    // Attachments are consumed by the message they were sent with.
    let last = seen[1].body["messages"]
        .as_array()
        .unwrap()
        .last()
        .unwrap()
        .clone();
    assert!(last["content"].as_str().unwrap().starts_with("and again"));
}

#[tokio::test]
async fn non_vision_models_get_a_note_and_the_read_tool_returns_images() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("chart.png"), png(8, 8)).unwrap();
    let call = ok(openai(
        json!({"tool_calls":[{"index":0,"id":"r1","type":"function","function":{"name":"read","arguments":"{\"path\":\"chart.png\"}"}}]}),
        "tool_calls",
    ));
    let (url, server) = mock_server(vec![call, text("a chart"), text("no vision")]).await;
    let mut command = cove(root.path(), data.path());
    command
        .args([
            "--provider",
            "openai",
            "--model",
            "gpt-4o",
            "--base-url",
            &url,
        ])
        .env("COVE_API_KEY", "k");
    let output = run(
        command,
        Some("look at the chart\n/model deepseek-v4-pro\nagain\n/exit\n"),
    )
    .await;
    assert_success(&output);
    let seen = server.await.unwrap();
    let messages = seen[1].body["messages"].as_array().unwrap();
    let tool = messages.iter().position(|m| m["role"] == "tool").unwrap();
    assert_eq!(messages[tool + 1]["role"], "user");
    assert!(
        messages[tool + 1]
            .to_string()
            .contains("data:image/png;base64,")
    );
    // deepseek-v4-pro cannot see images and no vision model is configured.
    let later = seen[2].body.to_string();
    assert!(!later.contains("data:image/png"));
    assert!(later.contains("image omitted"));
}

#[tokio::test]
async fn anthropic_requests_use_prompt_caching_and_bill_cache_reads() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let frames = [
        json!({"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":900000,"cache_creation_input_tokens":0}}}),
        json!({"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}),
        json!({"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}),
        json!({"type":"message_stop"}),
    ];
    let body: String = frames.iter().map(|v| format!("data: {v}\n\n")).collect();
    let (url, server) = mock_server(vec![ok(body)]).await;
    write_config(
        data.path(),
        json!({"model":"claude-sonnet-4-5","thinking":"adaptive","effort":"medium",
               "provider":{"name":"anthropic","api_key":"k","base_url":url}}),
    );
    let output = run(
        cove(root.path(), data.path()),
        Some("hello\n/model\n/exit\n"),
    )
    .await;
    assert_success(&output);
    let stdout = String::from_utf8_lossy(&output.stdout);
    // 900k cache reads at $0.30/M plus 100 plain at $3/M, not 900k at $3/M.
    assert!(stdout.contains("cost=$0.2703"), "{stdout}");
    let seen = server.await.unwrap();
    assert!(
        seen[0]
            .headers
            .contains("anthropic-beta: token-efficient-tools-2025-11-18,prompt-caching-2024-07-31")
    );
    assert_eq!(
        seen[0].body["system"][0]["cache_control"]["type"],
        "ephemeral"
    );
    assert_eq!(seen[0].body["thinking"]["type"], "adaptive");
    assert_eq!(seen[0].body["output_config"]["effort"], "medium");
}

#[tokio::test]
async fn context_overflow_compacts_retries_and_learns_the_window() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        text("first answer"),
        status(
            400,
            r#"{"error":{"message":"This model's maximum context length is 20000 tokens."}}"#,
        ),
        text("short summary"),
        text("second answer"),
    ])
    .await;
    let mut command = cove(root.path(), data.path());
    command
        .args([
            "--provider",
            "openai",
            "--model",
            "mock-model",
            "--base-url",
            &url,
        ])
        .env("COVE_API_KEY", "k");
    let input = format!("{}\nsecond task\n/exit\n", "describe this: ".repeat(300));
    let output = run(command, Some(&input)).await;
    assert_success(&output);
    let stdout = String::from_utf8_lossy(&output.stdout);
    assert!(stdout.contains("second answer"), "{stdout}");
    let seen = server.await.unwrap();
    assert_eq!(seen.len(), 4);
    assert!(
        seen[2].body["messages"][1]["content"]
            .as_str()
            .unwrap()
            .contains("Summarize")
    );
    let saved: Value =
        serde_json::from_slice(&std::fs::read(data.path().join("config.json")).unwrap()).unwrap();
    assert_eq!(saved["model_context_windows"]["mock-model"], 20000);
}

#[tokio::test]
async fn a_session_resumes_on_another_model_and_limits_are_reported() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let limited = Reply {
        status: 200,
        headers: vec![
            ("x-ratelimit-limit-requests", "100".into()),
            ("x-ratelimit-remaining-requests", "99".into()),
            ("x-ratelimit-reset-requests", "30s".into()),
        ],
        body: openai(json!({"content":"first"}), "stop"),
    };
    let (url, server) = mock_server(vec![limited, text("second")]).await;
    let start = |model: &str| {
        let mut command = cove(root.path(), data.path());
        command
            .args(["--provider", "openai", "--model", model, "--base-url", &url])
            .env("COVE_API_KEY", "k");
        command
    };
    let output = run(start("model-a"), Some("hello\n/status limits\n/exit\n")).await;
    assert_success(&output);
    assert!(
        String::from_utf8_lossy(&output.stdout).contains("requests: 99/100 remaining, resets in")
    );
    let id = std::fs::read_dir(data.path().join("sessions"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path()
        .file_stem()
        .unwrap()
        .to_string_lossy()
        .into_owned();
    let mut resumed = start("model-b");
    resumed.args(["--resume", &id]);
    let output = run(resumed, Some("again\n/exit\n")).await;
    assert_success(&output);
    let seen = server.await.unwrap();
    assert_eq!(seen[1].body["model"], "model-b");
    // The resumed history from model-a is sent to model-b.
    assert!(seen[1].body.to_string().contains("first"));
}

#[tokio::test]
async fn images_move_to_a_configured_vision_model() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("ui.png"), png(4, 4)).unwrap();
    let (url, server) = mock_server(vec![text("seen")]).await;
    write_config(
        data.path(),
        json!({"model":"deepseek-v4-pro","model_fast":"deepseek-v4-flash",
               "provider":{"name":"deepseek","api_key":"k","base_url":url}}),
    );
    let mut command = cove(root.path(), data.path());
    command.args(["-p", "refactor the layout shown in @ui.png"]);
    let output = run(command, None).await;
    assert_success(&output);
    let seen = server.await.unwrap();
    // "refactor" routes to the premium model, which cannot see; the image goes to flash.
    assert_eq!(seen[0].body["model"], "deepseek-v4-flash");
    assert!(seen[0].body.to_string().contains("data:image/png;base64,"));
}

#[tokio::test]
async fn reasoning_is_shown_only_when_configured() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let reply = || {
        ok(openai(
            json!({"reasoning_content":"hidden thoughts","content":"answer"}),
            "stop",
        ))
    };
    let (url, server) = mock_server(vec![reply(), reply()]).await;
    let config = |show: bool| json!({"model":"m","show_reasoning":show,"provider":{"name":"openai","api_key":"k","base_url":url}});
    write_config(data.path(), config(false));
    let mut command = cove(root.path(), data.path());
    command.args(["-p", "think"]);
    let output = run(command, None).await;
    assert_success(&output);
    assert!(!String::from_utf8_lossy(&output.stderr).contains("hidden thoughts"));
    write_config(data.path(), config(true));
    let mut command = cove(root.path(), data.path());
    command.args(["-p", "think"]);
    let output = run(command, None).await;
    assert_success(&output);
    assert!(String::from_utf8_lossy(&output.stderr).contains("hidden thoughts"));
    server.await.unwrap();
}
