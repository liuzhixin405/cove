//! CLI-level tests for draw_image, browser, /race, /changes (apply, verify-browser)
//! and remote supervision.

use crate::common::{self, Launch};
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
    // A restore point was taken before the drawing: /undo removes the created file.
    let session = std::fs::read_dir(data.path().join("sessions"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let id = session.file_stem().unwrap().to_str().unwrap().to_owned();
    for step in ["/undo #1", "/undo confirm"] {
        let mut undo = cli(root.path(), data.path(), &url);
        undo.args(["--resume", &id, "-p", step]);
        let (ok, _, stderr) = run(undo).await;
        assert!(ok, "{step}: {stderr}");
    }
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

/// Lanes answering with a write: `beta.txt` for the lane asked for another
/// approach, `alpha.txt` for the other one.
fn racer() -> Responder {
    Arc::new(|body: &Value| {
        if has_tool_result(body) {
            return openai(json!({"content":"done"}), "stop");
        }
        let name = if mentions(body, "different approach") {
            "beta"
        } else {
            "alpha"
        };
        tool(
            "w1",
            "write",
            json!({"path": format!("{name}.txt"), "content": format!("{name}\n")}),
        )
    })
}

/// A user config whose verification command every race lane passes.
fn verifying_config(dir: &Path) -> PathBuf {
    let path = dir.join("config.json");
    std::fs::write(
        &path,
        json!({"done_verify_commands":["git --version"]}).to_string(),
    )
    .unwrap();
    path
}

fn race_id(data: &Path) -> String {
    std::fs::read_dir(data.join("race"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .file_name()
        .into_string()
        .unwrap()
}

#[tokio::test]
async fn race_offers_two_solutions_and_applies_one_only_on_explicit_confirm() {
    let repo = repo();
    let data = tempfile::tempdir().unwrap();
    let outside = tempfile::tempdir().unwrap();
    let config = verifying_config(outside.path());
    let (url, _) = mock_server(racer()).await;
    let race = |args: &[&str]| {
        let mut command = cli(repo.path(), data.path(), &url);
        command.arg("--config").arg(&config).arg("-p").args(args);
        command
    };
    let (ok, stdout, stderr) = run(race(&["/race Write the file"])).await;
    assert!(ok, "{stdout}\n{stderr}");
    // Both lanes ran on the main model (no fast model configured), B asked for
    // another approach, and both passed the shared verification.
    assert!(stdout.contains("主模型·换思路"), "{stdout}");
    assert_eq!(stdout.matches("验证 ✓").count(), 2, "{stdout}");
    assert!(stdout.contains("改动 1 个文件 +1 -0"), "{stdout}");
    // Nothing reached the user's tree yet, and the worktrees are gone.
    assert!(!repo.path().join("alpha.txt").exists());
    assert_eq!(git(repo.path(), &["worktree", "list"]).lines().count(), 1);
    // The lanes' model calls are in the ledger.
    let ledger: String = std::fs::read_dir(data.path().join("usage"))
        .unwrap()
        .flatten()
        .map(|entry| std::fs::read_to_string(entry.path()).unwrap())
        .collect();
    assert!(ledger.lines().count() >= 4, "{ledger}");
    let run_id = race_id(data.path());
    let (_, stdout, _) = run(race(&["/race"])).await;
    assert!(stdout.contains("Write the file"), "{stdout}");
    let (_, stdout, _) = run(race(&["/changes"])).await;
    assert!(stdout.contains(&format!("race:{run_id}:b")), "{stdout}");
    let (ok, stdout, stderr) = run(race(&[&format!("/changes apply race:{run_id}:b")])).await;
    assert!(ok && stdout.contains("confirm"), "{stdout}{stderr}");
    assert!(!repo.path().join("beta.txt").exists());
    let (ok, _, stderr) = run(race(&[&format!("/changes apply race:{run_id}:b confirm")])).await;
    assert!(ok, "{stderr}");
    assert_eq!(
        std::fs::read_to_string(repo.path().join("beta.txt")).unwrap(),
        "beta\n"
    );
    assert!(!repo.path().join("alpha.txt").exists());
    // The other solution cannot be applied on top (dirty tree, sibling applied).
    let (ok, _, _) = run(race(&[&format!("/changes apply race:{run_id}:a confirm")])).await;
    assert!(!ok);
}

#[tokio::test]
async fn unverified_race_solutions_are_shown_but_not_applied() {
    let repo = repo();
    let data = tempfile::tempdir().unwrap();
    let (url, _) = mock_server(racer()).await;
    let mut command = cli(repo.path(), data.path(), &url);
    command.args(["-p", "/race Write the file"]);
    let (ok, stdout, stderr) = run(command).await;
    assert!(ok, "{stdout}\n{stderr}");
    assert!(stdout.contains("未验证"), "{stdout}");
    let run_id = race_id(data.path());
    let mut apply = cli(repo.path(), data.path(), &url);
    apply.args(["-p", &format!("/changes apply race:{run_id}:a confirm")]);
    let (ok, _, _) = run(apply).await;
    assert!(!ok);
    assert!(!repo.path().join("alpha.txt").exists());
}

#[tokio::test]
async fn race_refuses_a_dirty_tree_without_starting_lanes() {
    let repo = repo();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(repo.path().join("README.md"), "edited\n").unwrap();
    let (url, requests) = mock_server(racer()).await;
    let mut command = cli(repo.path(), data.path(), &url);
    command.args(["-p", "/race Write the file"]);
    let (ok, _, stderr) = run(command).await;
    assert!(!ok && stderr.contains("干净"), "{stderr}");
    assert!(requests.lock().unwrap().is_empty());
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
        &format!("/changes verify-browser {}", workflow.to_string_lossy()),
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
            workflow.to_string_lossy()
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

const PRICING: &str = "def price(item):\n    return item['cost']\n";
const INVOICE: &str =
    "from pricing import price\n\n\ndef format_invoice(x):\n    return f\"{price(x):.2f}\"\n";

/// A repository where `invoice.py` calls `price()` and no test refers to it.
fn pricing_repo() -> tempfile::TempDir {
    let repo = repo();
    std::fs::write(repo.path().join("pricing.py"), PRICING).unwrap();
    std::fs::write(repo.path().join("invoice.py"), INVOICE).unwrap();
    git(repo.path(), &["add", "-A"]);
    git(repo.path(), &["commit", "-q", "-m", "pricing"]);
    repo
}

#[tokio::test]
async fn changes_impact_lists_the_untested_callers_of_a_commit() {
    let repo = pricing_repo();
    let data = tempfile::tempdir().unwrap();
    git(repo.path(), &["checkout", "-q", "-b", "topic"]);
    std::fs::write(
        repo.path().join("pricing.py"),
        "def price(item, currency):\n    return item['cost'], currency\n",
    )
    .unwrap();
    git(repo.path(), &["commit", "-q", "-am", "currency"]);
    let (url, requests) =
        mock_server(Arc::new(|_: &Value| openai(json!({"content":"x"}), "stop"))).await;
    for range in ["main..topic", "main...HEAD", "HEAD"] {
        let mut command = cli(repo.path(), data.path(), &url);
        command.args(["-p", &format!("/changes impact {range}")]);
        let (ok, stdout, stderr) = run(command).await;
        assert!(ok, "{stdout}\n{stderr}");
        assert!(
            stdout.contains("[影响] 改了 1 个符号，其他文件里有 1 处使用"),
            "{range}: {stdout}"
        );
        assert!(stdout.contains("签名变化 pricing.py:1 price()"), "{stdout}");
        assert!(
            stdout.contains("invoice.py:5  format_invoice()  没有测试引用"),
            "{stdout}"
        );
        assert!(!stdout.contains(r"\\?\"), "{stdout}");
    }
    let mut command = cli(repo.path(), data.path(), &url);
    command.args(["-p", "/changes impact no-such-rev..HEAD"]);
    let (ok, _, stderr) = run(command).await;
    assert!(!ok && stderr.contains("找不到提交"), "{stderr}");
    // Read-only: no model call, nothing written.
    assert!(requests.lock().unwrap().is_empty());
    assert!(git(repo.path(), &["status", "--porcelain"]).is_empty());
}

#[tokio::test]
async fn the_end_of_a_task_shows_the_impact_and_hands_untested_callers_back_once() {
    let repo = pricing_repo();
    let data = tempfile::tempdir().unwrap();
    let (url, requests) = mock_server(Arc::new(|body: &Value| {
        if mentions(body, "[change_impact;") {
            openai(json!({"content":"checked invoice.py"}), "stop")
        } else if tool_results(body).is_empty() {
            tool(
                "e1",
                "edit",
                json!({"path":"pricing.py","old":"def price(item):","new":"def price(item, currency='CNY'):"}),
            )
        } else {
            openai(json!({"content":"price() now takes a currency"}), "stop")
        }
    }))
    .await;
    let mut command = cli(repo.path(), data.path(), &url);
    command.args(["-p", "add a currency to price()"]);
    let (ok, stdout, stderr) = run(command).await;
    assert!(ok, "{stdout}\n{stderr}");
    assert!(
        stderr.contains("[影响] 改了 1 个符号，其他文件里有 1 处使用"),
        "{stderr}"
    );
    assert!(
        stderr.contains("format_invoice()  没有测试引用"),
        "{stderr}"
    );
    let requests = requests.lock().unwrap();
    let handed: Vec<&Value> = requests
        .iter()
        .filter(|body| mentions(body, "[change_impact;"))
        .collect();
    assert!(!handed.is_empty(), "{requests:?}");
    assert!(mentions(
        handed[0],
        "invoice.py:5 in format_invoice() uses price"
    ));
    // Once: the last request carries one such notice.
    let last = requests.last().unwrap()["messages"].to_string();
    assert_eq!(last.matches("[change_impact;").count(), 1, "{last}");
    // The acceptance report keeps the counts and the untested caller.
    let report: Value = std::fs::read_dir(data.path().join("acceptance"))
        .unwrap()
        .flatten()
        .map(|entry| serde_json::from_slice(&std::fs::read(entry.path()).unwrap()).unwrap())
        .next()
        .unwrap();
    assert_eq!(report["impact"]["signature"], 1, "{report}");
    assert_eq!(report["impact"]["untested"][0]["file"], "invoice.py");
    assert_eq!(report["impact"]["handed_back"], true);
}
