//! CLI-level tests for draw_image, browser, /changes (race, apply, verify-browser),
//! automations and remote supervision.

mod common;
use common::Launch;
use std::{
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
    time::Duration,
};

use serde_json::{Value, json};
use tokio::{
    io::{AsyncBufReadExt, AsyncReadExt, AsyncWriteExt, BufReader},
    net::TcpListener,
    process::Command,
};

type Responder = Arc<dyn Fn(&Value) -> String + Send + Sync>;

/// Mock OpenAI server answering every request from `respond`, concurrently.
async fn mock_server(respond: Responder) -> (String, Arc<Mutex<Vec<Value>>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let requests: Arc<Mutex<Vec<Value>>> = Arc::default();
    let seen = requests.clone();
    tokio::spawn(async move {
        loop {
            let Ok((mut socket, _)) = listener.accept().await else {
                return;
            };
            let respond = respond.clone();
            let seen = seen.clone();
            tokio::spawn(async move {
                let mut request = Vec::new();
                let body_start = loop {
                    let mut chunk = [0_u8; 4096];
                    let size = socket.read(&mut chunk).await.unwrap_or(0);
                    if size == 0 {
                        return;
                    }
                    request.extend_from_slice(&chunk[..size]);
                    if let Some(end) = request.windows(4).position(|part| part == b"\r\n\r\n") {
                        break end + 4;
                    }
                };
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
                    let size = socket.read(&mut chunk).await.unwrap_or(0);
                    if size == 0 {
                        return;
                    }
                    request.extend_from_slice(&chunk[..size]);
                }
                let body: Value =
                    serde_json::from_slice(&request[body_start..body_start + length]).unwrap();
                let response = respond(&body);
                seen.lock().unwrap().push(body);
                let header = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    response.len()
                );
                let _ = socket.write_all(header.as_bytes()).await;
                let _ = socket.write_all(response.as_bytes()).await;
                let _ = socket.shutdown().await;
            });
        }
    });
    (format!("http://{address}/v1"), requests)
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

fn has_tool_result(body: &Value) -> bool {
    body["messages"]
        .as_array()
        .is_some_and(|messages| messages.iter().any(|m| m["role"] == "tool"))
}

fn tool_results(body: &Value) -> Vec<String> {
    body["messages"]
        .as_array()
        .map(|messages| {
            messages
                .iter()
                .filter(|m| m["role"] == "tool")
                .map(|m| m["content"].as_str().unwrap_or("").to_owned())
                .collect()
        })
        .unwrap_or_default()
}

fn mentions(body: &Value, needle: &str) -> bool {
    body["messages"].to_string().contains(needle)
}

fn cli(root: &Path, data: &Path, url: &str) -> Command {
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
        "60",
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.env_remove("COVE_BROWSER");
    command.kill_on_drop(true);
    command
}

async fn run(mut command: Command) -> (bool, String, String) {
    let output = tokio::time::timeout(Duration::from_secs(120), command.launch())
        .await
        .expect("command timed out")
        .unwrap();
    (
        output.status.success(),
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

fn git(dir: &Path, args: &[&str]) -> String {
    let output = std::process::Command::new("git")
        .args([
            "-c",
            "user.name=t",
            "-c",
            "user.email=t@t",
            "-c",
            "commit.gpgsign=false",
            "-c",
            "core.autocrlf=false",
        ])
        .args(args)
        .current_dir(dir)
        .output()
        .unwrap();
    assert!(output.status.success(), "git {args:?}");
    String::from_utf8_lossy(&output.stdout).into_owned()
}

fn repo() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    git(dir.path(), &["init", "-q", "-b", "main"]);
    git(dir.path(), &["config", "core.autocrlf", "false"]);
    std::fs::write(dir.path().join("README.md"), "base\n").unwrap();
    git(dir.path(), &["add", "-A"]);
    git(dir.path(), &["commit", "-q", "-m", "init"]);
    dir
}

/// Children answering with a write of `<name>.txt` chosen from the prompt, then stopping.
fn writer() -> Responder {
    Arc::new(|body: &Value| {
        if has_tool_result(body) {
            return openai(json!({"content":"done"}), "stop");
        }
        let name = if mentions(body, "alpha") {
            "alpha"
        } else {
            "beta"
        };
        tool(
            "w1",
            "write",
            json!({"path": format!("{name}.txt"), "content": format!("{name}\n")}),
        )
    })
}

#[tokio::test]
async fn draw_image_writes_a_checkpointed_png_that_undo_removes() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.png"), "not a png").unwrap();
    let (url, requests) = mock_server(Arc::new(|body: &Value| match tool_results(body).len() {
        0 => tool(
            "d1",
            "draw_image",
            json!({"outputPath":"out.png","width":64,"height":32,"shapes":[
                {"type":"fill","color":"#eeeeee"},
                {"type":"rect","x":4,"y":4,"w":20,"h":10,"color":"rgba(255,0,0,0.5)"},
                {"type":"circle","x":40,"y":16,"r":8,"color":"#0000ff"},
                {"type":"line","x":0,"y":0,"x2":63,"y2":31,"strokeWidth":3}
            ]}),
        ),
        1 => tool(
            "d2",
            "draw_image",
            json!({"outputPath":"notes.png","width":8,"height":8,"shapes":[{"type":"fill"}]}),
        ),
        _ => openai(json!({"content":"drawn"}), "stop"),
    }))
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["-p", "draw a diagram"]);
    let (_, _, stderr) = run(command).await;
    let png = std::fs::read(root.path().join("out.png")).unwrap();
    assert!(png.starts_with(b"\x89PNG\r\n\x1a\n"), "{stderr}");
    let results = tool_results(requests.lock().unwrap().last().unwrap());
    assert!(results[0].contains("Saved PNG (64x32"), "{results:?}");
    assert!(results[1].contains("not a PNG"), "{results:?}");
    assert_eq!(
        std::fs::read_to_string(root.path().join("notes.png")).unwrap(),
        "not a png"
    );
    let tools = requests.lock().unwrap()[0]["tools"].to_string();
    assert!(tools.contains("draw_image") && tools.contains("\"browser\""));
    // The PNG went through the checkpoint: /undo removes the created file.
    let session = std::fs::read_dir(data.path().join("sessions"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let id = session.file_stem().unwrap().to_str().unwrap().to_owned();
    let mut undo = cli(root.path(), data.path(), &url);
    undo.args(["--resume", &id, "-p", "/undo confirm"]);
    let (ok, _, stderr) = run(undo).await;
    assert!(ok, "{stderr}");
    assert!(!root.path().join("out.png").exists());
}

#[tokio::test]
async fn browser_tool_refuses_loopback_targets() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, requests) = mock_server(Arc::new(|body: &Value| {
        if has_tool_result(body) {
            openai(json!({"content":"ok"}), "stop")
        } else {
            tool(
                "b1",
                "browser",
                json!({"action":"navigate","url":"http://127.0.0.1:9/admin"}),
            )
        }
    }))
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command.args(["-p", "read the admin page"]);
    run(command).await;
    let results = tool_results(requests.lock().unwrap().last().unwrap());
    assert!(results[0].contains("refusing"), "{results:?}");
}

#[tokio::test]
async fn race_runs_two_workers_and_applies_only_on_explicit_confirm() {
    let repo = repo();
    let data = tempfile::tempdir().unwrap();
    let specs = tempfile::tempdir().unwrap();
    let (url, _) = mock_server(writer()).await;
    let spec = specs.path().join("race.json");
    std::fs::write(
        &spec,
        json!({"prompts":["Write the alpha file","Write the beta file"],"verify":[["git","--version"]],"timeout":100}).to_string(),
    )
    .unwrap();
    let mut command = cli(repo.path(), data.path(), &url);
    command.args(["-p", &format!("/changes race {}", spec.display())]);
    let (ok, stdout, stderr) = run(command).await;
    assert!(ok, "{stdout}\n{stderr}");
    assert!(stdout.contains("correct=true"), "{stdout}\n{stderr}");
    // Nothing reached the user's tree yet, and the worktrees are gone.
    assert!(!repo.path().join("alpha.txt").exists());
    assert_eq!(git(repo.path(), &["worktree", "list"]).lines().count(), 1);
    let run_id = std::fs::read_dir(data.path().join("race"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .file_name()
        .into_string()
        .unwrap();
    let mut list = cli(repo.path(), data.path(), &url);
    list.args(["-p", "/changes"]);
    let (_, stdout, _) = run(list).await;
    assert!(stdout.contains(&format!("race:{run_id}:a")), "{stdout}");
    let mut preview = cli(repo.path(), data.path(), &url);
    preview.args(["-p", &format!("/changes apply race:{run_id}:a")]);
    let (ok, stdout, stderr) = run(preview).await;
    assert!(ok && stdout.contains("confirm"), "{stdout}{stderr}");
    assert!(!repo.path().join("alpha.txt").exists());
    let mut apply = cli(repo.path(), data.path(), &url);
    apply.args(["-p", &format!("/changes apply race:{run_id}:a confirm")]);
    let (ok, _, stderr) = run(apply).await;
    assert!(ok, "{stderr}");
    assert_eq!(
        std::fs::read_to_string(repo.path().join("alpha.txt")).unwrap(),
        "alpha\n"
    );
    assert!(!repo.path().join("beta.txt").exists());
    // The other candidate cannot be applied on top (dirty tree, sibling applied).
    let mut again = cli(repo.path(), data.path(), &url);
    again.args(["-p", &format!("/changes apply race:{run_id}:b confirm")]);
    let (ok, _, _) = run(again).await;
    assert!(!ok);
}

#[tokio::test]
async fn race_refuses_a_dirty_tree_without_starting_workers() {
    let repo = repo();
    let data = tempfile::tempdir().unwrap();
    let specs = tempfile::tempdir().unwrap();
    std::fs::write(repo.path().join("README.md"), "edited\n").unwrap();
    let (url, requests) = mock_server(writer()).await;
    let spec = specs.path().join("race.json");
    std::fs::write(
        &spec,
        json!({"prompts":["alpha","beta"],"verify":[["git","--version"]]}).to_string(),
    )
    .unwrap();
    let mut command = cli(repo.path(), data.path(), &url);
    command.args(["-p", &format!("/changes race {}", spec.display())]);
    let (ok, _, stderr) = run(command).await;
    assert!(!ok && stderr.contains("干净"), "{stderr}");
    assert!(requests.lock().unwrap().is_empty());
}

fn standalone(data: &Path, url: &str, args: &[&str]) -> Command {
    let mut command = Command::new(common::exe());
    command.arg("--automation").args(args);
    command.env("COVE_DATA_DIR", data);
    command.env("COVE_PROVIDER", "openai");
    command.env("COVE_MODEL", "mock-model");
    command.env("COVE_BASE_URL", url);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.env_remove("COVE_CONFIG");
    command.kill_on_drop(true);
    command
}

#[tokio::test]
async fn automation_results_land_in_the_inbox_and_apply_only_on_request() {
    let repo = repo();
    let data = tempfile::tempdir().unwrap();
    let specs = tempfile::tempdir().unwrap();
    let (url, _) = mock_server(writer()).await;
    let spec = specs.path().join("job.json");
    std::fs::write(
        &spec,
        json!({"version":1,"spec":{"id":"nightly","prompt":"Write the alpha file","every_seconds":3600,
            "event":"tests-failed","timeout_seconds":100,"max_turns":4,"verify":[["git","--version"]]}})
        .to_string(),
    )
    .unwrap();
    let project = repo.path().to_str().unwrap();
    let (ok, stdout, stderr) = run(standalone(
        data.path(),
        &url,
        &["add", project, spec.to_str().unwrap()],
    ))
    .await;
    assert!(ok && stdout.contains("尚未启动"), "{stdout}{stderr}");
    let (ok, stdout, stderr) = run(standalone(data.path(), &url, &["list", project])).await;
    assert!(ok && stdout.contains("nightly"), "{stdout}{stderr}");
    let (ok, stdout, stderr) = run(standalone(
        data.path(),
        &url,
        &["event", project, "tests-failed", "build-1"],
    ))
    .await;
    assert!(ok && stdout.contains("Succeeded"), "{stdout}{stderr}");
    // The same occurrence never runs twice, and the schedule is not yet due again.
    let (_, stdout, _) = run(standalone(
        data.path(),
        &url,
        &["event", project, "tests-failed", "build-1"],
    ))
    .await;
    assert!(stdout.contains("没有运行任何任务"), "{stdout}");
    let (_, stdout, _) = run(standalone(data.path(), &url, &["tick", project])).await;
    assert!(stdout.contains("没有运行任何任务"), "{stdout}");
    assert!(!repo.path().join("alpha.txt").exists());
    let (ok, stdout, _) = run(standalone(data.path(), &url, &["inbox", project])).await;
    assert!(ok);
    let id = stdout.split_whitespace().next().unwrap().to_owned();
    let out = specs.path().join("result.patch");
    let (ok, _, stderr) = run(standalone(
        data.path(),
        &url,
        &["inbox", project, "patch", &id, out.to_str().unwrap()],
    ))
    .await;
    assert!(ok, "{stderr}");
    assert!(std::fs::read_to_string(&out).unwrap().contains("+alpha"));
    let mut apply = cli(repo.path(), data.path(), &url);
    apply.args(["-p", &format!("/changes apply inbox:{id} confirm")]);
    let (ok, _, stderr) = run(apply).await;
    assert!(ok, "{stderr}");
    assert!(repo.path().join("alpha.txt").exists());
    let (_, stdout, _) = run(standalone(data.path(), &url, &["inbox", project])).await;
    assert!(stdout.contains("Applied"), "{stdout}");
}

#[tokio::test]
async fn remote_supervision_serves_status_with_a_private_token_until_stopped() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, _) = mock_server(Arc::new(|_: &Value| {
        openai(json!({"content":"hi"}), "stop")
    }))
    .await;
    let mut command = cli(root.path(), data.path(), &url);
    command
        .stdin(std::process::Stdio::piped())
        .stdout(std::process::Stdio::piped())
        .stderr(std::process::Stdio::piped());
    let mut child = command.spawn().unwrap();
    let mut stdin = child.stdin.take().unwrap();
    let mut stdout = BufReader::new(child.stdout.take().unwrap()).lines();
    stdin.write_all(b"/remote start\n").await.unwrap();
    let address = tokio::time::timeout(Duration::from_secs(30), async {
        loop {
            let line = stdout.next_line().await.unwrap().unwrap();
            if let Some(rest) = line.strip_prefix("远程监督地址：") {
                break rest.split_whitespace().next().unwrap().to_owned();
            }
        }
    })
    .await
    .unwrap();
    let token_file: PathBuf = data.path().join("remote").join("token");
    let token = std::fs::read_to_string(&token_file).unwrap();
    let client = reqwest::Client::builder().no_proxy().build().unwrap();
    let status: Value = client
        .get(format!("{address}/v1/status"))
        .bearer_auth(&token)
        .send()
        .await
        .unwrap()
        .json()
        .await
        .unwrap();
    assert_eq!(status["running"], json!(false));
    let unauthorized = client
        .get(format!("{address}/v1/status"))
        .send()
        .await
        .unwrap();
    assert_eq!(unauthorized.status(), 401);
    stdin.write_all(b"/remote stop\n/exit\n").await.unwrap();
    drop(stdin);
    let status = tokio::time::timeout(Duration::from_secs(30), child.wait())
        .await
        .unwrap()
        .unwrap();
    assert!(status.success());
    assert!(!token_file.exists());
}

/// A tiny local site for browser verification.
async fn site() -> String {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    tokio::spawn(async move {
        loop {
            let Ok((mut socket, _)) = listener.accept().await else {
                return;
            };
            tokio::spawn(async move {
                let mut buffer = vec![0_u8; 8192];
                let _ = socket.read(&mut buffer).await;
                let page = r#"<!doctype html><html><body><h1>Form</h1>
<input id="name"><button id="go" onclick="document.getElementById('out').textContent='Hello '+document.getElementById('name').value">Go</button>
<div id="out"></div></body></html>"#;
                let response = format!(
                    "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{page}",
                    page.len()
                );
                let _ = socket.write_all(response.as_bytes()).await;
            });
        }
    });
    format!("http://{address}/")
}

#[tokio::test]
async fn browser_verification_passes_locally_only_with_explicit_permission() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let files = tempfile::tempdir().unwrap();
    let site = site().await;
    let workflow = files.path().join("workflow.json");
    std::fs::write(
        &workflow,
        json!({"url": site, "fixtures":{"who":"fixture-42"},"steps":[
            {"action":"fill","selector":"#name","fixture":"who"},
            {"action":"click","selector":"#go"},
            {"action":"assert_text","selector":"#out","text":"Hello fixture-42"},
            {"action":"assert_visible","selector":"#out"}
        ]})
        .to_string(),
    )
    .unwrap();
    let (url, _) = mock_server(Arc::new(|_: &Value| openai(json!({"content":"x"}), "stop"))).await;
    let mut refused = cli(root.path(), data.path(), &url);
    refused.args([
        "-p",
        &format!("/changes verify-browser {}", workflow.display()),
    ]);
    let (ok, stdout, stderr) = run(refused).await;
    assert!(ok, "{stderr}");
    // Without --allow-local a loopback page is never verified (or there is no browser).
    assert!(stdout.contains("Unverified"), "{stdout}");
    let has_browser = !stdout.contains("no Chrome/Edge found");
    let mut allowed = cli(root.path(), data.path(), &url);
    allowed.args([
        "-p",
        &format!(
            "/changes verify-browser {} --allow-local",
            workflow.display()
        ),
    ]);
    let (ok, stdout, stderr) = run(allowed).await;
    assert!(ok, "{stderr}");
    if has_browser {
        assert!(stdout.contains("Pass in"), "{stdout}{stderr}");
        assert!(
            stdout.contains("desktop 1280x800: Pass") && stdout.contains("mobile 390x844: Pass"),
            "{stdout}"
        );
        let report = stdout
            .lines()
            .find_map(|line| line.strip_prefix("报告："))
            .unwrap();
        let text = std::fs::read_to_string(report).unwrap();
        assert!(
            !text.contains("fixture-42") && !text.contains("#name"),
            "{text}"
        );
        assert!(Path::new(report).with_file_name("desktop.png").exists());
    } else {
        assert!(stdout.contains("Unverified"), "{stdout}");
    }
}
