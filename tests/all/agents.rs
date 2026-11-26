//! Area F: sub-agent types, plans, regression verification, background agents and the
//! agent lifecycle (interrupt, park, wake, merge) through the real CLI and a mock provider.

use crate::common::{self, Launch};
use std::{
    collections::{HashMap, HashSet, VecDeque},
    path::Path,
    sync::{Arc, Mutex},
    time::Duration,
};

use serde_json::{Value, json};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::TcpListener,
    process::Command,
};

/// `None` holds the request open without answering (a model call that never returns).
type Queue = VecDeque<Option<String>>;

struct Mock {
    url: String,
    requests: Arc<Mutex<Vec<(String, Value)>>>,
    /// When each request arrived and when its answer was sent, by role.
    times: Arc<Mutex<Vec<(String, std::time::Instant, std::time::Instant)>>>,
    task: tokio::task::JoinHandle<()>,
}

impl Drop for Mock {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Mock {
    fn requests(&self, role: &str) -> Vec<Value> {
        self.requests
            .lock()
            .unwrap()
            .iter()
            .filter(|(name, _)| name == role)
            .map(|(_, body)| body.clone())
            .collect()
    }

    /// (arrived, answered) of the requests of `role`, in arrival order.
    fn times(&self, role: &str) -> Vec<(std::time::Instant, std::time::Instant)> {
        let mut times: Vec<_> = self
            .times
            .lock()
            .unwrap()
            .iter()
            .filter(|(name, _, _)| name == role)
            .map(|(_, arrived, answered)| (*arrived, *answered))
            .collect();
        times.sort();
        times
    }
}

/// Routes each request to a queue: background agents, plan items (by item id), or the parent.
fn role(body: &Value) -> String {
    let text = body.to_string();
    if text.contains("You run in the background") {
        return "bg".into();
    }
    if let Some(start) = text.find("You are item ") {
        let rest = &text[start + "You are item ".len()..];
        return rest
            .split_whitespace()
            .next()
            .unwrap_or_default()
            .to_owned();
    }
    if text.contains("You work inside the git worktree") {
        return "wt".into();
    }
    if text.contains("Write docs only.") {
        return "docs".into();
    }
    if text.contains("independent regression verifier") {
        return "verify".into();
    }
    if text.contains("You are a delegated read-only sub-agent (explore)") {
        return "explore".into();
    }
    if text.contains("delegated sub-agent with write access") {
        return "general".into();
    }
    "parent".into()
}

async fn mock(queues: Vec<(&str, Vec<Option<String>>)>) -> Mock {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("http://{}/v1", listener.local_addr().unwrap());
    let queues: Arc<Mutex<HashMap<String, Queue>>> = Arc::new(Mutex::new(
        queues
            .into_iter()
            .map(|(name, list)| (name.to_owned(), list.into()))
            .collect(),
    ));
    let requests = Arc::new(Mutex::new(Vec::new()));
    let log = requests.clone();
    let times = Arc::new(Mutex::new(Vec::new()));
    let timing = times.clone();
    let task = tokio::spawn(async move {
        loop {
            let Ok((mut socket, _)) = listener.accept().await else {
                return;
            };
            let queues = queues.clone();
            let log = log.clone();
            let timing = timing.clone();
            tokio::spawn(async move {
                let mut request = Vec::new();
                let body_start = loop {
                    let mut chunk = [0_u8; 4096];
                    let Ok(size) = socket.read(&mut chunk).await else {
                        return;
                    };
                    if size == 0 {
                        return;
                    }
                    request.extend_from_slice(&chunk[..size]);
                    if let Some(end) = request.windows(4).position(|part| part == b"\r\n\r\n") {
                        break end + 4;
                    }
                };
                let headers = String::from_utf8_lossy(&request[..body_start]).to_string();
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
                    let Ok(size) = socket.read(&mut chunk).await else {
                        return;
                    };
                    if size == 0 {
                        return;
                    }
                    request.extend_from_slice(&chunk[..size]);
                }
                let body: Value =
                    serde_json::from_slice(&request[body_start..body_start + length]).unwrap();
                let name = role(&body);
                let arrived = std::time::Instant::now();
                log.lock().unwrap().push((name.clone(), body));
                let response = queues
                    .lock()
                    .unwrap()
                    .get_mut(&name)
                    .and_then(VecDeque::pop_front);
                let response = match response {
                    Some(Some(response)) => response,
                    Some(None) => {
                        tokio::time::sleep(Duration::from_secs(600)).await;
                        return;
                    }
                    None => text(&format!("(no scripted response for {name})")),
                };
                // "@delay:<ms>\n<response>" answers after a pause (a slow model call).
                let response = match response.strip_prefix("@delay:") {
                    Some(rest) => {
                        let (ms, body) = rest.split_once('\n').unwrap();
                        tokio::time::sleep(Duration::from_millis(ms.parse().unwrap())).await;
                        body.to_owned()
                    }
                    None => response,
                };
                timing
                    .lock()
                    .unwrap()
                    .push((name.clone(), arrived, std::time::Instant::now()));
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
    Mock {
        url,
        requests,
        times,
        task,
    }
}

fn openai(delta: Value, reason: &str) -> String {
    format!(
        "data: {}\n\ndata: {}\n\ndata: [DONE]\n\n",
        json!({"choices":[{"delta":delta,"finish_reason":reason}]}),
        json!({"choices":[],"usage":{"prompt_tokens":20,"completion_tokens":10}})
    )
}

fn text(content: &str) -> String {
    openai(json!({"content":content}), "stop")
}

fn tool(id: &str, name: &str, arguments: Value) -> String {
    openai(
        json!({"tool_calls":[{"index":0,"id":id,"type":"function","function":{"name":name,"arguments":arguments.to_string()}}]}),
        "tool_calls",
    )
}

fn cli(root: &Path, data: &Path, url: &str) -> Command {
    cli_with(root, data, url, "30")
}

fn cli_with(root: &Path, data: &Path, url: &str, timeout: &str) -> Command {
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
        timeout,
    ]);
    command.arg("--cwd").arg(root).arg("--data-dir").arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.kill_on_drop(true);
    command
}

async fn run(command: &mut Command) -> (bool, String, String) {
    let output = command.launch().await.unwrap();
    (
        output.status.success(),
        String::from_utf8_lossy(&output.stdout).into_owned(),
        String::from_utf8_lossy(&output.stderr).into_owned(),
    )
}

/// The only foreground session (background agents' sessions live in their worktrees).
fn saved_id(data: &Path) -> String {
    let sessions: Vec<_> = std::fs::read_dir(data.join("sessions"))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| {
            let session: Value = serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
            !session["cwd"].as_str().unwrap().contains("worktrees")
        })
        .collect();
    assert_eq!(sessions.len(), 1, "{sessions:?}");
    sessions[0]
        .file_stem()
        .unwrap()
        .to_string_lossy()
        .into_owned()
}

fn git(root: &Path, args: &[&str]) -> String {
    let output = std::process::Command::new("git")
        .args(["-c", "user.name=t", "-c", "user.email=t@localhost"])
        .args(args)
        .current_dir(root)
        .output()
        .unwrap();
    assert!(output.status.success(), "git {args:?}: {output:?}");
    String::from_utf8_lossy(&output.stdout).into_owned()
}

fn repo() -> tempfile::TempDir {
    let root = tempfile::tempdir().unwrap();
    git(root.path(), &["init", "-q"]);
    std::fs::write(root.path().join("README.md"), "base\n").unwrap();
    git(root.path(), &["add", "-A"]);
    git(root.path(), &["commit", "-q", "-m", "base"]);
    root
}

fn python() -> &'static str {
    if cfg!(windows) { "python" } else { "python3" }
}

#[tokio::test]
async fn execute_plan_runs_the_todo_dag_in_parallel_and_passes_results_on() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"write module a","status":"pending"},
        {"content":"write module b","status":"pending"},
        {"content":"depends:todo-1,todo-2 integrate a and b","status":"pending"}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"max_agents":2}),
                )),
                Some(text("All three items are done.")),
            ],
        ),
        ("todo-1", vec![Some(text("module a written"))]),
        ("todo-2", vec![Some(text("module b written"))]),
        ("todo-3", vec![Some(text("integrated"))]),
    ])
    .await;
    let (ok, _, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "build it"])).await;
    assert!(ok, "{stderr}");
    // The dependent item ran last and received both results.
    let third = &server.requests("todo-3")[0];
    let body = third.to_string();
    assert!(
        body.contains("[result of todo-1]") && body.contains("module a written"),
        "{body}"
    );
    assert!(body.contains("module b written"), "{body}");
    let order: Vec<String> = server
        .requests
        .lock()
        .unwrap()
        .iter()
        .map(|(name, _)| name.clone())
        .collect();
    let position = |name: &str| order.iter().position(|role| role == name).unwrap();
    assert!(position("todo-3") > position("todo-1") && position("todo-3") > position("todo-2"));
    let report = server.requests("parent")[2].to_string();
    assert!(report.contains("[done] [todo-1]"), "{report}");
    assert!(report.contains("[done] [todo-3]"), "{report}");
    assert!(report.contains("3 done, 0 failed"), "{report}");
    // Experimental tools are off by default.
    let tools = server.requests("parent")[0]["tools"].to_string();
    assert!(
        !tools.contains("\"sleep\"") && tools.contains("execute_plan"),
        "{tools}"
    );
    // Sub-agent spend is accounted in the parent session: 3 parent + 3 item calls, plus
    // one parent call for the open-todo nudge (the model never marked the items done).
    assert!(stderr.contains("模型调用 7 次"), "{stderr}");
}

#[tokio::test]
async fn a_verify_agent_without_regression_evidence_is_a_failed_tool_result() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "check",
                    "agent",
                    json!({"description":"verify fix","prompt":"Verify the fix in lib.rs","type":"verify"}),
                )),
                Some(text("Verification did not succeed.")),
            ],
        ),
        ("verify", vec![Some(text("Looks correct to me."))]),
    ])
    .await;
    let (ok, _, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "verify"])).await;
    assert!(!ok, "a failed verify must fail the task: {stderr}");
    let verify = &server.requests("verify")[0];
    let tools: Vec<String> = verify["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|tool| tool["function"]["name"].as_str().unwrap().to_owned())
        .collect();
    assert!(tools.contains(&"regression".to_owned()), "{tools:?}");
    assert!(
        !tools
            .iter()
            .any(|tool| tool == "write" || tool == "shell" || tool.starts_with("web"))
    );
    let result = server.requests("parent")[1].to_string();
    assert!(result.contains("[exit: unverified"), "{result}");
    assert!(result.contains("NOT verified"), "{result}");
}

#[tokio::test]
async fn regression_tool_proves_the_test_fails_on_the_baseline_and_restores_files() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("lib.txt"), "bug\n").unwrap();
    std::fs::write(
        root.path().join("check.py"),
        "import sys\nsys.exit(0 if 'fixed' in open('lib.txt').read() else 1)\n",
    )
    .unwrap();
    let config = data.path().join("config.json");
    std::fs::write(
        &config,
        json!({"regression_command": format!("{} check.py {{filter}}", python())}).to_string(),
    )
    .unwrap();
    let server = mock(vec![(
        "parent",
        vec![
            Some(tool(
                "fix",
                "edit",
                json!({"path":"lib.txt","old":"bug","new":"fixed"}),
            )),
            Some(tool(
                "prove",
                "regression",
                json!({"files":["lib.txt"],"runner":"command","filter":"lib"}),
            )),
            Some(text("Fixed and proven.")),
        ],
    )])
    .await;
    let (ok, _, stderr) = run(cli(root.path(), data.path(), &server.url)
        .arg("--config")
        .arg(&config)
        .args(["-p", "fix lib"]))
    .await;
    assert!(ok, "{stderr}");
    let result = server.requests("parent")[2].to_string();
    assert!(result.contains("regression evidence: passed"), "{result}");
    assert_eq!(
        std::fs::read_to_string(root.path().join("lib.txt")).unwrap(),
        "fixed\n"
    );
}

#[tokio::test]
async fn background_agent_interrupt_restart_resume_notify_and_merge() {
    let root = repo();
    let data = tempfile::tempdir().unwrap();
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "spawn",
                    "agent",
                    json!({"description":"add feature","prompt":"Create feature.txt containing 'feature'","background":true}),
                )),
                Some(text("Started a background agent.")),
            ],
        ),
        (
            "bg",
            vec![
                // First run: the model call never returns; the process stops meanwhile.
                None,
                Some(tool(
                    "create",
                    "write",
                    json!({"path":"feature.txt","content":"feature\n"}),
                )),
                Some(text("Created feature.txt.")),
            ],
        ),
    ])
    .await;
    let (ok, _, stderr) = run(cli_with(root.path(), data.path(), &server.url, "6")
        .args(["-p", "add a feature in the background"]))
    .await;
    assert!(ok, "{stderr}");
    let spawned = server.requests("parent")[1].to_string();
    let start = spawned.find("Background agent ").unwrap() + "Background agent ".len();
    let id = spawned[start..start + 8].to_owned();
    assert!(stderr.contains("已中断"), "{stderr}");
    let session = saved_id(data.path());

    // A new process lists it as interrupted, with its worktree branch.
    let (ok, stdout, stderr) = run(cli(root.path(), data.path(), &server.url).args([
        "--resume",
        &session,
        "-p",
        "/status agents",
    ]))
    .await;
    assert!(ok, "{stderr}");
    assert!(
        stdout.contains(&id) && stdout.contains("interrupted"),
        "{stdout}"
    );
    assert!(
        stdout.contains(&format!("branch=cove/agent-{id}")),
        "{stdout}"
    );
    assert!(stdout.contains("worktree has no changes"), "{stdout}");

    // Resume continues from the transcript; the one-shot run waits and prints the notice.
    let (ok, _, stderr) = run(cli(root.path(), data.path(), &server.url).args([
        "--resume",
        &session,
        "-p",
        &format!("/status agents resume {id}"),
    ]))
    .await;
    assert!(ok, "{stderr}");
    assert!(
        stderr.contains(&format!("后台代理 {id}（general）已完成")),
        "{stderr}"
    );
    assert!(stderr.contains("Created feature.txt."), "{stderr}");
    let resumed = &server.requests("bg")[1];
    assert!(
        resumed
            .to_string()
            .contains("Create feature.txt containing"),
        "the resumed run keeps its original context"
    );
    assert!(!root.path().join("feature.txt").exists());

    // Merge applies the branch to the user's tree on request.
    let (ok, stdout, stderr) = run(cli(root.path(), data.path(), &server.url).args([
        "--resume",
        &session,
        "-p",
        &format!("/status agents merge {id}"),
    ]))
    .await;
    assert!(ok, "{stderr}");
    assert!(stdout.contains("已合并"), "{stdout}");
    // Line endings follow the user's git checkout settings.
    assert_eq!(
        std::fs::read_to_string(root.path().join("feature.txt"))
            .unwrap()
            .trim_end(),
        "feature"
    );
    assert!(git(root.path(), &["log", "--oneline"]).contains(&format!("agent {id}")));
    assert!(
        git(root.path(), &["status", "--porcelain"])
            .trim()
            .is_empty()
    );
    let (_, stdout, _) = run(cli(root.path(), data.path(), &server.url).args([
        "--resume",
        &session,
        "-p",
        "/status agents",
    ]))
    .await;
    assert!(
        stdout.contains("discarded") && stdout.contains("merged"),
        "{stdout}"
    );
}

#[tokio::test]
async fn a_finished_background_agent_notifies_the_parent_at_its_next_tool_round() {
    let root = repo();
    let data = tempfile::tempdir().unwrap();
    let wait = format!("{} -c \"import time; time.sleep(3)\"", python());
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "spawn",
                    "agent",
                    json!({"description":"look around","prompt":"List the files","type":"explore","background":true}),
                )),
                Some(tool("wait", "shell", json!({"command":wait}))),
                Some(text("Got the report.")),
            ],
        ),
        ("bg", vec![Some(text("Only README.md exists."))]),
    ])
    .await;
    let (ok, _, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "explore in background"])).await;
    assert!(ok, "{stderr}");
    let after_wait = server.requests("parent")[2].to_string();
    assert!(after_wait.contains("(explore) finished"), "{after_wait}");
    assert!(
        after_wait.contains("Only README.md exists."),
        "{after_wait}"
    );
    // Parent and background calls are both in the session total.
    assert!(stderr.contains("模型调用 4 次"), "{stderr}");
}

#[tokio::test]
async fn the_main_task_parks_and_wakes_with_a_message() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(root.path().join("notes.txt"), "todo\n").unwrap();
    let server = mock(vec![(
        "parent",
        vec![
            // Window of one call: the read runs, then the limit interrupts the task.
            Some(tool("look", "read", json!({"path":"notes.txt"}))),
            Some(text("Read notes.txt; more to do.")),
            // After waking: the model ends, the queued message is delivered, it finishes.
            Some(text("Continuing.")),
            Some(text("Finished as requested.")),
        ],
    )])
    .await;
    let (ok, _, _) = run(cli(root.path(), data.path(), &server.url).args([
        "--max-turns",
        "1",
        "-p",
        "work on notes",
    ]))
    .await;
    assert!(!ok, "the call limit interrupts the task");
    let parked = saved_id(data.path());
    let (ok, stdout, stderr) = run(cli(root.path(), data.path(), &server.url).args([
        "--resume",
        &parked,
        "-p",
        "/status agents park main",
    ]))
    .await;
    assert!(ok, "{stderr}");
    assert!(
        stdout.contains(&format!("主任务已挂起为 {}", &parked[..8])),
        "{stdout}"
    );

    // A fresh process (new session) sees the parked main task.
    let (ok, stdout, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "/status agents"])).await;
    assert!(ok, "{stderr}");
    assert!(
        stdout.contains("main(parked)") && stdout.contains("paused"),
        "{stdout}"
    );

    let (ok, stdout, stderr) = run(cli(root.path(), data.path(), &server.url).args([
        "-p",
        &format!(
            "/status agents resume {} please also summarize",
            &parked[..8]
        ),
    ]))
    .await;
    assert!(ok, "{stderr}");
    assert!(stdout.contains("Finished as requested."), "{stdout}");
    let requests = server.requests("parent");
    let woken = requests[3].to_string();
    assert!(woken.contains("please also summarize"), "{woken}");
    assert!(woken.contains("work on notes"), "full context: {woken}");
    let (_, stdout, _) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "/status agents"])).await;
    assert!(!stdout.contains("main(parked)"), "{stdout}");
}

#[tokio::test]
async fn worktree_tools_run_an_agent_inside_and_merge_back() {
    let root = repo();
    let data = tempfile::tempdir().unwrap();
    let branch = format!(
        "wt{}",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    );
    let commit = "git add -A; git -c user.name=t -c user.email=t@localhost commit -q -m inside";
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("make", "worktree", json!({"branch":branch}))),
                Some(tool(
                    "work",
                    "agent",
                    json!({"description":"work inside","prompt":"Create inside.txt and commit it","worktree":branch}),
                )),
                Some(tool(
                    "finish",
                    "exit_worktree",
                    json!({"branch":branch,"merge":true}),
                )),
                Some(text("Merged the worktree.")),
            ],
        ),
        (
            "wt",
            vec![
                Some(tool(
                    "create",
                    "write",
                    json!({"path":"inside.txt","content":"inside
"}),
                )),
                Some(tool("commit", "shell", json!({"command":commit}))),
                Some(text("Created and committed inside.txt.")),
            ],
        ),
    ])
    .await;
    let (ok, _, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "use a worktree"])).await;
    assert!(ok, "{stderr}");
    let path = root.path().parent().unwrap().join(&branch);
    assert!(!path.exists(), "worktree removed");
    assert!(root.path().join("inside.txt").exists());
    let merged = server.requests("parent")[3].to_string();
    assert!(merged.contains("deleted after merge"), "{merged}");
}

#[tokio::test]
async fn experimental_tools_and_custom_agent_types_come_from_the_config() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let config = data.path().join("config.json");
    std::fs::write(
        &config,
        json!({"experimental_tools":true,"agent_types":{"docs":{
            "description":"documentation writer","instructions":"Write docs only.","tools":["read","glob"]}}})
        .to_string(),
    )
    .unwrap();
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "note",
                    "task",
                    json!({"title":"later","description":"do it later"}),
                )),
                Some(tool("nap", "sleep", json!({"seconds":1}))),
                Some(tool(
                    "doc",
                    "agent",
                    json!({"description":"docs","prompt":"Describe the project","type":"docs"}),
                )),
                Some(text("Done.")),
            ],
        ),
        ("docs", vec![Some(text("It is empty."))]),
    ])
    .await;
    let (ok, _, stderr) = run(cli(root.path(), data.path(), &server.url)
        .arg("--config")
        .arg(&config)
        .args(["-p", "try experimental tools"]))
    .await;
    assert!(ok, "{stderr}");
    let requests = server.requests("parent");
    assert!(requests[0]["tools"].to_string().contains("team_create"));
    assert!(requests[1].to_string().contains("nothing runs it"));
    assert!(requests[2].to_string().contains("slept 1 seconds"));
    let docs = &server.requests("docs")[0];
    let tools: Vec<&str> = docs["tools"]
        .as_array()
        .unwrap()
        .iter()
        .map(|tool| tool["function"]["name"].as_str().unwrap())
        .collect();
    assert_eq!(tools, ["read", "glob"]);
    // Every sub-agent, custom types included, is asked for a short final answer.
    assert!(
        docs.to_string()
            .contains("Your final answer is read by the parent agent"),
        "{docs}"
    );
    assert!(
        requests[3]
            .to_string()
            .contains("(docs): docs] [exit: completed")
    );
}

#[tokio::test]
async fn a_sub_agent_without_progress_ends_stalled_and_the_parent_goes_on() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(
        data.path().join("config.json"),
        json!({"stall_rounds":4}).to_string(),
    )
    .unwrap();
    let read = || {
        Some(tool(
            &cove::session::new_id(),
            "read",
            json!({"path":"missing.txt"}),
        ))
    };
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "look",
                    "agent",
                    json!({"description":"find the config","prompt":"Read missing.txt and report its settings","type":"explore"}),
                )),
                Some(text("The sub-agent could not read missing.txt; it does not exist.")),
            ],
        ),
        (
            "explore",
            vec![
                read(),
                read(),
                read(),
                read(),
                read(),
                Some(text("missing.txt does not exist.")),
            ],
        ),
    ])
    .await;
    let (_, _, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "find the config"])).await;
    let explore = server.requests("explore");
    assert_eq!(explore.len(), 6, "{stderr}");
    assert!(explore[5].to_string().contains("cove stopped the task"));
    let parent = server.requests("parent");
    assert_eq!(parent.len(), 2, "{stderr}");
    assert!(
        parent[1].to_string().contains("[exit: stalled"),
        "{}",
        parent[1]
    );
}

/// Several tool calls in one response.
fn calls(list: &[(&str, &str, Value)]) -> String {
    let calls: Vec<Value> = list
        .iter()
        .enumerate()
        .map(|(index, (id, name, arguments))| {
            json!({"index":index,"id":id,"type":"function",
                "function":{"name":name,"arguments":arguments.to_string()}})
        })
        .collect();
    openai(json!({ "tool_calls": calls }), "tool_calls")
}

/// A provider block in the config (a command-line provider would drop `model_fast`):
/// DeepSeek's pro as the main model and flash as the fast one, at their list prices.
fn fast_cli(root: &Path, data: &Path, url: &str) -> Command {
    let config = data.join("config.json");
    std::fs::write(
        &config,
        json!({"provider":{"name":"openai","base_url":url},
            "model":"deepseek-v4-pro","model_fast":"deepseek-v4-flash"})
        .to_string(),
    )
    .unwrap();
    let mut command = Command::new(common::exe());
    command
        .args(["--permission", "bypass", "--timeout", "60", "--config"])
        .arg(&config)
        .arg("--cwd")
        .arg(root)
        .arg("--data-dir")
        .arg(data);
    command.env("COVE_API_KEY", "test-only-not-a-real-key");
    command.kill_on_drop(true);
    command
}

fn jsonl(path: &Path) -> Vec<Value> {
    std::fs::read_to_string(path)
        .unwrap_or_default()
        .lines()
        .filter_map(|line| serde_json::from_str(line).ok())
        .collect()
}

#[tokio::test]
async fn explore_runs_on_the_fast_model_in_parallel_and_is_billed_at_its_prices() {
    let root = repo();
    let data = tempfile::tempdir().unwrap();
    let explore = |area: &str| {
        Some(format!(
            "@delay:1500\n{}",
            text(&format!(
                "Conclusion: {area} has one caller.\nEvidence:\n- README.md:1 the caller\n- src/gone.rs:3 a stale reference\nNot determined: nothing"
            ))
        ))
    };
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(calls(&[
                    (
                        "e1",
                        "agent",
                        json!({"description":"area one","prompt":"Who calls save in area one?","type":"explore"}),
                    ),
                    (
                        "e2",
                        "agent",
                        json!({"description":"area two","prompt":"Who calls save in area two?","type":"explore"}),
                    ),
                    (
                        "g1",
                        "agent",
                        json!({"description":"note it","prompt":"Write nothing; reply done","type":"general"}),
                    ),
                ])),
                Some(text("Done.")),
            ],
        ),
        ("explore", vec![explore("area one"), explore("area two")]),
        ("general", vec![Some(text("general done"))]),
    ])
    .await;
    // "调试" routes the task to the main model.
    let (ok, _, stderr) =
        run(fast_cli(root.path(), data.path(), &server.url).args(["-p", "调试：谁调用了 save"]))
            .await;
    assert!(ok, "{stderr}");
    // The two explore agents overlapped; the editing agent ran after both.
    let explores = server.times("explore");
    assert_eq!(explores.len(), 2, "{stderr}");
    assert!(
        explores[1].0 < explores[0].1,
        "explore agents ran one after the other"
    );
    let general = server.times("general");
    assert_eq!(general.len(), 1, "{stderr}");
    assert!(general[0].0 >= explores[0].1 && general[0].0 >= explores[1].1);
    // explore asked for the fast model; general kept the task's model.
    for body in server.requests("explore") {
        assert_eq!(body["model"], "deepseek-v4-flash", "{body}");
    }
    assert_eq!(server.requests("general")[0]["model"], "deepseek-v4-pro");
    // The parent sees the evidence check and the model of each result.
    let result = server.requests("parent")[1].to_string();
    assert!(result.contains("citations: 1 ok, 1 invalid"), "{result}");
    assert!(result.contains("[未核实：文件不存在]"), "{result}");
    assert!(result.contains("model: deepseek-v4-flash"), "{result}");
    // Session and usage ledger bill the fast model's calls at the fast model's prices.
    let entries: Vec<Value> = std::fs::read_dir(data.path().join("usage"))
        .unwrap()
        .flat_map(|entry| jsonl(&entry.unwrap().path()))
        .collect();
    let fast: Vec<&Value> = entries
        .iter()
        .filter(|entry| entry["source"] == "agent" && entry["model"] == "deepseek-v4-flash")
        .collect();
    assert_eq!(fast.len(), 2, "{entries:?}");
    let ledger: f64 = fast
        .iter()
        .map(|entry| entry["cost"].as_f64().unwrap())
        .sum();
    let flash = 2.0 * (20.0 * 0.30 + 10.0 * 1.20) / 1e6;
    assert!((ledger - flash).abs() < 1e-12, "{ledger} vs {flash}");
    let session: Value = serde_json::from_slice(
        &std::fs::read(
            data.path()
                .join("sessions")
                .join(format!("{}.json", saved_id(data.path()))),
        )
        .unwrap(),
    )
    .unwrap();
    let billed = session["accounting"]["by_model"]["deepseek-v4-flash"]["cost"]
        .as_f64()
        .unwrap();
    assert!((billed - ledger).abs() < 1e-12, "{billed} vs {ledger}");
    // The trace names the model that answered and the sub-agent that asked.
    let trace = jsonl(&data.path().join("trace.jsonl"));
    let agents: HashSet<&str> = trace
        .iter()
        .filter(|line| line["kind"] == "model" && line["model"] == "deepseek-v4-flash")
        .filter_map(|line| line["agent"].as_str())
        .collect();
    assert_eq!(agents.len(), 2, "{trace:?}");
    assert!(trace.iter().any(|line| line["kind"] == "model"
        && line["model"] == "deepseek-v4-pro"
        && line.get("agent").is_none()));
}

#[tokio::test]
async fn a_long_answer_is_capped_in_the_parent_and_saved_whole() {
    let root = repo();
    let data = tempfile::tempdir().unwrap();
    let mut answer = String::from("Conclusion: long.\nEvidence:\n- README.md:1 base\n");
    while answer.len() < 20 * 1024 {
        answer.push_str("- 一行很长的补充说明，父模型在 prompt 里要了 1400 字，cove 照样截断。\n");
    }
    answer.push_str("TAIL-MARKER\n");
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "e1",
                    "agent",
                    json!({"description":"wide","prompt":"Investigate, answer in about 1400 words","type":"explore"}),
                )),
                Some(text("Done.")),
            ],
        ),
        ("explore", vec![Some(text(&answer))]),
    ])
    .await;
    // explore_model main: explore stays on the main model.
    let (ok, _, stderr) = run(fast_cli(root.path(), data.path(), &server.url).args([
        "--explore-model",
        "main",
        "-p",
        "调试：调查一下",
    ]))
    .await;
    assert!(ok, "{stderr}");
    assert_eq!(server.requests("explore")[0]["model"], "deepseek-v4-pro");
    let parent = server.requests("parent");
    let result = parent[1]["messages"]
        .as_array()
        .unwrap()
        .iter()
        .rev()
        .find(|message| message["role"] == "tool")
        .unwrap()["content"]
        .as_str()
        .unwrap()
        .to_owned();
    assert!(result.len() < 8 * 1024, "{}", result.len());
    assert!(!result.contains("TAIL-MARKER"));
    assert!(result.contains("answer cut at 6 KB"), "{result}");
    let saved: Vec<_> = std::fs::read_dir(data.path().join("tool-outputs"))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .filter(|path| path.to_string_lossy().ends_with("_agent.txt"))
        .collect();
    assert_eq!(saved.len(), 1);
    let full = std::fs::read_to_string(&saved[0]).unwrap();
    assert!(full.contains("TAIL-MARKER"));
    assert!(
        result.contains(&cove::paths::display(&saved[0])),
        "{result}"
    );
}

#[tokio::test]
async fn a_plan_item_with_writes_is_kept_to_them_and_reports_what_it_changed() {
    let root = repo();
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"write a","status":"pending","writes":["a.txt::intro"],"estimate_minutes":2},
        {"content":"writes:b.txt write b","status":"pending"}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool("run", "execute_plan", json!({"parallel":true}))),
                Some(text("Reported.")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(tool(
                    "w1",
                    "write",
                    json!({"path":"c.txt","content":"no\n"}),
                )),
                Some(tool("w2", "write", json!({"path":"a.txt","content":"a\n"}))),
                Some(tool("s1", "shell", json!({"command":"echo x > gen.txt"}))),
                Some(tool(
                    "s2",
                    "shell",
                    json!({"command":"npm install left-pad"}),
                )),
                Some(text("a written")),
            ],
        ),
        ("todo-2", vec![Some(text("b not needed"))]),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "split it"])).await;
    let item = server.requests("todo-1");
    let prompt = item[0].to_string();
    assert!(
        prompt.contains("This item may change only: a.txt::intro"),
        "{prompt}"
    );
    let refused = item[1].to_string();
    assert!(
        refused.contains("c.txt is outside this plan item's writes (a.txt::intro)"),
        "{refused}"
    );
    let shell = item[3].to_string();
    assert!(
        shell.contains("changed files outside this plan item's writes: gen.txt"),
        "{shell}"
    );
    let npm = item[4].to_string();
    assert!(
        npm.contains("such steps run after all items finish"),
        "{npm}"
    );
    assert!(!root.path().join("c.txt").exists());
    assert!(root.path().join("a.txt").exists() && root.path().join("gen.txt").exists());
    let report = server.requests("parent")[2].to_string();
    assert!(report.contains("written: a.txt"), "{report}");
    assert!(report.contains("c.txt (write refused)"), "{report}");
    assert!(report.contains("gen.txt (changed by shell)"), "{report}");
    assert!(
        report.contains("npm install left-pad (command refused)"),
        "{report}"
    );
    assert!(report.contains("Outside writes (check them"), "{report}");
    assert!(report.contains("todo-1: c.txt (write refused)"), "{report}");
    // The second item declared writes through the content prefix and stayed inside.
    assert!(report.contains("[done] [todo-2] write b"), "{report}");
    // The failed item is not retried: its refusals would repeat.
    assert_eq!(server.requests("todo-1").len(), 5);
}

#[tokio::test]
async fn overlapping_writes_of_parallel_items_reject_the_plan_until_they_depend() {
    for (depends, rejected) in [(json!([]), true), (json!(["todo-1"]), false)] {
        let root = repo();
        let data = tempfile::tempdir().unwrap();
        let todos = json!({"todos":[
            {"content":"part one","status":"pending","writes":["src/x.rs::one"]},
            {"content":"part two","status":"pending","writes":["src/*.rs"],"depends":depends}
        ]});
        let server = mock(vec![
            (
                "parent",
                vec![
                    Some(tool("plan", "todo", todos)),
                    Some(tool("run", "execute_plan", json!({"parallel":true}))),
                    Some(text("ok")),
                ],
            ),
            ("todo-1", vec![Some(text("one"))]),
            ("todo-2", vec![Some(text("two"))]),
        ])
        .await;
        let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "go"])).await;
        let report = server.requests("parent")[2].to_string();
        if rejected {
            assert!(
                report.contains("plan rejected before any item ran"),
                "{report}"
            );
            assert!(
                report.contains("todo-1 and todo-2 both write src/x.rs"),
                "{report}"
            );
            assert!(server.requests("todo-1").is_empty());
        } else {
            assert!(report.contains("2 done, 0 failed"), "{report}");
            assert!(report.contains("Outside writes: none."), "{report}");
        }
    }
}

#[tokio::test]
async fn a_long_parallel_plan_suggests_task_split_once_and_records_its_estimate() {
    // (config, minutes per item, hinted, executed)
    let cases = [
        (json!({}), 4, true, true),
        (json!({}), 2, false, false),
        (json!({"split_suggest_minutes": 0}), 4, false, false),
    ];
    for (config, minutes, hinted, executed) in cases {
        let root = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        std::fs::write(data.path().join("config.json"), config.to_string()).unwrap();
        let todos = json!({"todos":[
            {"content":"a","status":"pending","estimate_minutes":minutes},
            {"content":"b","status":"pending","estimate_minutes":minutes}
        ]});
        let mut replies = vec![
            Some(tool("plan", "todo", todos.clone())),
            Some(tool("again", "todo", todos)),
        ];
        if executed {
            replies.push(Some(tool("run", "execute_plan", json!({"parallel":true}))));
        }
        replies.push(Some(text("planned")));
        let server = mock(vec![
            ("parent", replies),
            ("todo-1", vec![Some(text("a done"))]),
            ("todo-2", vec![Some(text("b done"))]),
        ])
        .await;
        let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "plan"])).await;
        let parent = server.requests("parent");
        let first = parent[1].to_string();
        assert_eq!(first.contains("[task-split]"), hinted, "{first}");
        if hinted {
            assert!(first.contains("about 8 min"), "{first}");
            assert!(first.contains("no estimate history yet"), "{first}");
            let messages = parent[2]["messages"].as_array().unwrap();
            let last = messages.last().unwrap().to_string();
            assert!(!last.contains("[task-split]"), "only once: {last}");
        }
        // Only an executed plan is recorded; planning alone records nothing.
        let records = std::fs::read_to_string(data.path().join("estimates.jsonl"));
        if executed {
            let records = records.unwrap();
            assert!(
                records.contains(&format!("\"estimate_minutes\":{}", minutes * 2)),
                "{records}"
            );
            assert!(records.contains("\"executed\":true"), "{records}");
            assert!(records.contains("\"parallel\":true"), "{records}");
        } else {
            assert!(records.is_err(), "{records:?}");
        }
    }
}

const LIB: &str = "fn one() {\n    1\n}\n\nfn two() {\n    2\n}\n";

/// A git repository whose `src/lib.rs` holds two functions, plus `files`.
fn lib_repo(files: &[(&str, &str)]) -> tempfile::TempDir {
    let root = repo();
    std::fs::create_dir(root.path().join("src")).unwrap();
    std::fs::write(root.path().join("src/lib.rs"), LIB).unwrap();
    for (path, content) in files {
        std::fs::write(root.path().join(path), content).unwrap();
    }
    git(root.path(), &["add", "-A"]);
    git(root.path(), &["commit", "-q", "-m", "lib"]);
    root
}

fn edit(id: &str, old: &str, new: &str) -> String {
    tool(id, "edit", json!({"path":"src/lib.rs","old":old,"new":new}))
}

/// The content of the last tool result in `body`.
fn last_result(body: &Value) -> String {
    body["messages"]
        .as_array()
        .unwrap()
        .iter()
        .rev()
        .find(|message| message["role"] == "tool")
        .map(|message| message["content"].to_string())
        .unwrap_or_default()
}

/// Files left under `<data>/overlays`.
fn overlay_files(data: &Path) -> Vec<std::path::PathBuf> {
    let mut found = Vec::new();
    let mut stack = vec![data.join("overlays")];
    while let Some(dir) = stack.pop() {
        let Ok(entries) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in entries.flatten() {
            if entry.path().is_dir() {
                stack.push(entry.path());
            } else {
                found.push(entry.path());
            }
        }
    }
    found
}

#[tokio::test]
async fn an_overlay_plan_runs_items_on_one_file_in_parallel_and_merges_them_at_the_end() {
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"change one","status":"pending","writes":["src/lib.rs::one"],"estimate_minutes":3},
        {"content":"change two","status":"pending","writes":["src/lib.rs::two"],"estimate_minutes":3},
        {"content":"depends:todo-1 notes","status":"pending","writes":["notes.md"]}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(tool(
                    "verify",
                    "shell",
                    json!({"command":"git diff --stat"}),
                )),
                Some(text("Merged and verified.")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(format!(
                    "@delay:600\n{}",
                    edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")
                )),
                Some(tool("b1", "shell", json!({"command":"cargo build"}))),
                Some(tool("b2", "shell", json!({"command":"echo x > gen.txt"}))),
                Some(text("one changed")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(format!(
                    "@delay:600\n{}",
                    edit("e2", "fn two() {\n    2\n}", "fn two() {\n    20\n}")
                )),
                Some(text("two changed")),
            ],
        ),
        (
            "todo-3",
            vec![
                Some(tool(
                    "s3",
                    "shell",
                    json!({"command":"git status --porcelain --untracked-files=all"}),
                )),
                Some(tool("r3", "read", json!({"path":"src/lib.rs"}))),
                Some(tool(
                    "w3",
                    "write",
                    json!({"path":"notes.md","content":"notes\n"}),
                )),
                Some(text("notes written")),
            ],
        ),
    ])
    .await;
    let (_, _, stderr) =
        run(cli(root.path(), data.path(), &server.url).args(["-p", "split it"])).await;
    // Both items on src/lib.rs ran at the same time.
    let one = server.times("todo-1");
    let two = server.times("todo-2");
    assert!(two[0].0 < one[0].1 && one[0].0 < two[0].1, "not parallel");
    let first = server.requests("todo-1")[0].to_string();
    assert!(
        first.contains("[overlay] Your file changes go into a private layer"),
        "{first}"
    );
    // Builds and file-writing commands are refused inside the items.
    let item = server.requests("todo-1");
    let build = last_result(&item[2]);
    assert!(
        build.contains("overlay sub-tasks cannot change files on disk"),
        "{build}"
    );
    let redirect = last_result(&item[3]);
    assert!(
        redirect.contains("overlay sub-tasks cannot change files on disk"),
        "{redirect}"
    );
    assert!(!root.path().join("gen.txt").exists());
    // While the items ran, the main workspace was untouched; todo-3 read through todo-1's layer.
    let third = server.requests("todo-3");
    let status = last_result(&third[1]);
    assert!(!status.contains("lib.rs"), "{status}");
    assert!(
        status.contains("does not have this sub-task's overlay changes"),
        "{status}"
    );
    let read = last_result(&third[2]);
    assert!(read.contains("10") && !read.contains("20"), "{read}");
    // Merged at the end: one direct write, one line merge (the line merge goes first; the
    // anchor replay only settles what it conflicts on), the new file.
    assert_eq!(
        std::fs::read_to_string(root.path().join("src/lib.rs")).unwrap(),
        "fn one() {\n    10\n}\n\nfn two() {\n    20\n}\n"
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("notes.md"))
            .unwrap()
            .replace("\r\n", "\n"),
        "notes\n"
    );
    let report = last_result(&server.requests("parent")[2]);
    assert!(report.contains("3 done, 0 failed"), "{report}");
    assert!(
        report.contains("src/lib.rs [todo-1]: direct write"),
        "{report}"
    );
    assert!(
        report.contains("src/lib.rs [todo-2]: line merge"),
        "{report}"
    );
    assert!(
        report.contains("notes.md [todo-3]: direct write"),
        "{report}"
    );
    assert!(report.contains("Implicit dependencies: none."), "{report}");
    assert!(
        report.contains("Overlay layers: merged and removed."),
        "{report}"
    );
    // The build runs once, after the merge, on the merged workspace.
    let verified = last_result(&server.requests("parent")[3]);
    assert!(verified.contains("lib.rs"), "{verified}");
    assert!(
        stderr.contains("[合并] 覆盖层合并进工作区：写入 3 个文件"),
        "{stderr}"
    );
    assert!(overlay_files(data.path()).is_empty());
}

#[tokio::test]
async fn an_overlay_conflict_is_redone_once_and_what_still_conflicts_is_left_out() {
    for redone in [true, false] {
        let root = lib_repo(&[]);
        let data = tempfile::tempdir().unwrap();
        // Distinct symbols let them run side by side, but both change the same line.
        let todos = json!({"todos":[
            {"content":"one is ten","status":"pending","writes":["src/lib.rs::one"]},
            {"content":"one is eleven","status":"pending","writes":["src/lib.rs::one_again"]}
        ]});
        let redo = if redone {
            vec![
                Some(edit("r1", "    10\n", "    10 + 1\n")),
                Some(text("redone on top of todo-1")),
            ]
        } else {
            vec![Some(text("cannot redo it"))]
        };
        let server = mock(vec![
            (
                "parent",
                vec![
                    Some(tool("plan", "todo", todos)),
                    Some(tool(
                        "run",
                        "execute_plan",
                        json!({"parallel":true,"isolation":"overlay"}),
                    )),
                    Some(text("done")),
                ],
            ),
            (
                "todo-1",
                vec![
                    Some(edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")),
                    Some(text("ten")),
                ],
            ),
            (
                "todo-2",
                vec![
                    Some(edit("e2", "fn one() {\n    1\n}", "fn one() {\n    11\n}")),
                    Some(text("eleven")),
                ],
            ),
            ("todo-2-redo", redo),
        ])
        .await;
        let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "conflict"])).await;
        let prompt = server.requests("todo-2-redo")[0].to_string();
        assert!(prompt.contains("[conflict] src/lib.rs"), "{prompt}");
        assert!(prompt.contains("Original goal: one is eleven"), "{prompt}");
        assert!(
            prompt.contains("This item may change only: src/lib.rs"),
            "{prompt}"
        );
        let report = last_result(&server.requests("parent")[2]);
        assert!(
            report.contains("src/lib.rs [todo-1]: direct write"),
            "{report}"
        );
        let content = std::fs::read_to_string(root.path().join("src/lib.rs")).unwrap();
        if redone {
            assert!(
                report.contains("src/lib.rs [todo-2]: merged after redo"),
                "{report}"
            );
            assert!(content.contains("    10 + 1\n"), "{content}");
            assert!(
                report.contains("Overlay layers: merged and removed."),
                "{report}"
            );
        } else {
            // Not written: the workspace keeps todo-1's merge, the report points at a copy.
            assert_eq!(content, LIB.replace("    1\n", "    10\n"));
            assert!(
                report.contains("src/lib.rs [todo-2]: conflict unresolved"),
                "{report}"
            );
            assert!(
                report.contains("the redo did not change this file"),
                "{report}"
            );
            assert!(report.contains("copy for you:"), "{report}");
            assert!(report.contains("conflicts"), "{report}");
            assert!(report.contains("stay pending: todo-2"), "{report}");
            assert!(report.contains("kept until this task ends"), "{report}");
        }
        // Kept layers and conflict copies go when the task ends.
        assert!(overlay_files(data.path()).is_empty(), "{redone}");
    }
}

#[tokio::test]
async fn overlapping_overlay_items_are_stacked_instead_of_rejected() {
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"change one","status":"pending","writes":["src/lib.rs::one"]},
        {"content":"change the file","status":"pending","writes":["src/lib.rs"]}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(text("done")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")),
                Some(text("ten")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(tool("r2", "read", json!({"path":"src/lib.rs"}))),
                Some(edit(
                    "e2",
                    "    10\n}\n\nfn two() {\n    2\n",
                    "    10\n}\n\nfn two() {\n    20\n",
                )),
                Some(text("built on todo-1")),
            ],
        ),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "stack"])).await;
    // todo-2 started after todo-1 ended and saw its change through the stacked layer.
    let one = server.times("todo-1");
    let two = server.times("todo-2");
    assert!(
        two[0].0 >= one.last().unwrap().1,
        "stacked items ran at once"
    );
    let read = last_result(&server.requests("todo-2")[1]);
    assert!(read.contains("10"), "{read}");
    let report = last_result(&server.requests("parent")[2]);
    assert!(
        report.contains("Implicit dependencies (added instead of rejecting the plan)"),
        "{report}"
    );
    assert!(
        report.contains("todo-2 after todo-1: both write src/lib.rs"),
        "{report}"
    );
    assert!(report.contains("2 done, 0 failed"), "{report}");
    assert_eq!(
        std::fs::read_to_string(root.path().join("src/lib.rs")).unwrap(),
        "fn one() {\n    10\n}\n\nfn two() {\n    20\n}\n"
    );
}

#[tokio::test]
async fn an_overlay_plan_needs_writes_and_falls_back_to_main_without_a_confirmation() {
    // Every item must declare writes.
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "plan",
                    "todo",
                    json!({"todos":[
                        {"content":"one","status":"pending","writes":["src/lib.rs::one"]},
                        {"content":"two","status":"pending"}
                    ]}),
                )),
                Some(tool("run", "execute_plan", json!({"isolation":"overlay"}))),
                Some(text("ok")),
            ],
        ),
        ("todo-1", vec![Some(text("one"))]),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "go"])).await;
    let report = last_result(&server.requests("parent")[2]);
    assert!(report.contains("needs writes on every item"), "{report}");
    assert!(report.contains("missing on: todo-2"), "{report}");
    assert!(server.requests("todo-1").is_empty());

    // -p in the default permission mode cannot confirm: the plan runs as isolation=main.
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool(
                    "plan",
                    "todo",
                    json!({"todos":[
                        {"content":"one","status":"pending","writes":["src/lib.rs::one"]}
                    ]}),
                )),
                Some(tool("run", "execute_plan", json!({"isolation":"overlay"}))),
                Some(text("ok")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")),
                Some(text("one")),
            ],
        ),
    ])
    .await;
    let mut command = Command::new(common::exe());
    command.args([
        "--provider",
        "openai",
        "--model",
        "mock-model",
        "--base-url",
        &server.url,
        "--permission",
        "default",
        "--timeout",
        "30",
    ]);
    command
        .arg("--cwd")
        .arg(root.path())
        .arg("--data-dir")
        .arg(data.path())
        .env("COVE_API_KEY", "test-only-not-a-real-key")
        .kill_on_drop(true)
        .args(["-p", "go"]);
    let _ = run(&mut command).await;
    let report = last_result(&server.requests("parent")[2]);
    assert!(
        report.contains("needs the user's confirmation") && report.contains("(this run: default)"),
        "{report}"
    );
    assert!(report.contains("the plan ran with isolation="), "{report}");
    assert!(
        !report.contains("Merge into the main workspace"),
        "{report}"
    );
    assert!(
        std::fs::read_to_string(root.path().join("src/lib.rs"))
            .unwrap()
            .contains("    10\n")
    );
}

fn edit_in(id: &str, path: &str, old: &str, new: &str) -> String {
    tool(id, "edit", json!({"path":path,"old":old,"new":new}))
}

#[tokio::test]
async fn overlay_items_writing_shared_files_run_in_parallel_without_claims() {
    let root = lib_repo(&[("src/a.rs", LIB), ("src/b.rs", LIB)]);
    let data = tempfile::tempdir().unwrap();
    // Each item writes both files, in opposite orders: claims would serialize them and
    // call the crossing a deadlock.
    let todos = json!({"todos":[
        {"content":"ones","status":"pending","writes":["src/a.rs::one","src/b.rs::one"]},
        {"content":"twos","status":"pending","writes":["src/a.rs::two","src/b.rs::two"]}
    ]});
    let one = "fn one() {\n    1\n}";
    let two = "fn two() {\n    2\n}";
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(text("done")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit_in("a1", "src/a.rs", one, "fn one() {\n    10\n}")),
                Some(format!(
                    "@delay:800\n{}",
                    edit_in("b1", "src/b.rs", one, "fn one() {\n    10\n}")
                )),
                Some(text("ones changed")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(edit_in("b2", "src/b.rs", two, "fn two() {\n    20\n}")),
                Some(format!(
                    "@delay:800\n{}",
                    edit_in("a2", "src/a.rs", two, "fn two() {\n    20\n}")
                )),
                Some(text("twos changed")),
            ],
        ),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "cross"])).await;
    // The slow second edits of both items overlapped.
    let one_times = server.times("todo-1");
    let two_times = server.times("todo-2");
    assert!(
        two_times[1].0 < one_times[1].1 && one_times[1].0 < two_times[1].1,
        "not parallel"
    );
    let report = last_result(&server.requests("parent")[2]);
    assert!(report.contains("2 done, 0 failed"), "{report}");
    assert!(!report.contains("write conflict"), "{report}");
    for file in ["src/a.rs", "src/b.rs"] {
        assert_eq!(
            std::fs::read_to_string(root.path().join(file)).unwrap(),
            "fn one() {\n    10\n}\n\nfn two() {\n    20\n}\n",
            "{file}"
        );
    }
}

#[tokio::test]
async fn an_item_on_two_parallel_layers_of_one_file_sees_both_changes() {
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"change one","status":"pending","writes":["src/lib.rs::one"]},
        {"content":"change two","status":"pending","writes":["src/lib.rs::two"]},
        {"content":"depends:todo-1,todo-2 notes","status":"pending","writes":["notes.md"]}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(text("done")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")),
                Some(text("ten")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(edit("e2", "fn two() {\n    2\n}", "fn two() {\n    20\n}")),
                Some(text("twenty")),
            ],
        ),
        (
            "todo-3",
            vec![
                Some(tool("r3", "read", json!({"path":"src/lib.rs"}))),
                Some(tool("g3", "grep", json!({"pattern":"10|20","path":"src"}))),
                Some(tool(
                    "w3",
                    "write",
                    json!({"path":"notes.md","content":"notes\n"}),
                )),
                Some(text("notes written")),
            ],
        ),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "diamond"])).await;
    let third = server.requests("todo-3");
    let read = last_result(&third[1]);
    assert!(read.contains("10") && read.contains("20"), "{read}");
    let grep = last_result(&third[2]);
    assert!(
        grep.contains("src/lib.rs:2:    10") && grep.contains("src/lib.rs:6:    20"),
        "{grep}"
    );
    let report = last_result(&server.requests("parent")[2]);
    assert!(report.contains("3 done, 0 failed"), "{report}");
    assert!(
        report.contains("Overlay layers: merged and removed."),
        "{report}"
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("src/lib.rs")).unwrap(),
        "fn one() {\n    10\n}\n\nfn two() {\n    20\n}\n"
    );
    assert!(root.path().join("notes.md").is_file());
    assert!(overlay_files(data.path()).is_empty());
}

#[tokio::test]
async fn an_item_on_two_conflicting_layers_is_deferred_with_its_dependents() {
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"one is ten","status":"pending","writes":["src/lib.rs::one"]},
        {"content":"one is eleven","status":"pending","writes":["src/lib.rs::one_again"]},
        {"content":"depends:todo-1,todo-2 notes","status":"pending","writes":["notes.md"]},
        {"content":"depends:todo-3 docs","status":"pending","writes":["docs.md"]}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(text("done")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")),
                Some(text("ten")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(edit("e2", "fn one() {\n    1\n}", "fn one() {\n    11\n}")),
                Some(text("eleven")),
            ],
        ),
        ("todo-2-redo", vec![Some(text("cannot redo it"))]),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "deferred"])).await;
    assert!(server.requests("todo-3").is_empty());
    assert!(server.requests("todo-4").is_empty());
    let report = last_result(&server.requests("parent")[2]);
    assert!(
        report
            .contains("deferred: the items it builds on (todo-1, todo-2) both changed src/lib.rs"),
        "{report}"
    );
    assert!(
        report.contains("dependency todo-3 did not succeed"),
        "{report}"
    );
    assert!(report.contains("2 done, 0 failed, 2 skipped"), "{report}");
    assert!(
        report.contains("src/lib.rs [todo-1]: direct write"),
        "{report}"
    );
}

#[tokio::test]
async fn an_overlay_merge_cancelled_during_a_redo_reports_what_reached_the_workspace() {
    let root = lib_repo(&[]);
    let data = tempfile::tempdir().unwrap();
    let todos = json!({"todos":[
        {"content":"one is ten","status":"pending","writes":["src/lib.rs::one"]},
        {"content":"one is eleven","status":"pending","writes":["src/lib.rs::one_again"]},
        {"content":"notes","status":"pending","writes":["notes.md"]}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(text("done")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit("e1", "fn one() {\n    1\n}", "fn one() {\n    10\n}")),
                Some(text("ten")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(edit("e2", "fn one() {\n    1\n}", "fn one() {\n    11\n}")),
                Some(text("eleven")),
            ],
        ),
        (
            "todo-3",
            vec![
                Some(tool(
                    "w3",
                    "write",
                    json!({"path":"notes.md","content":"notes\n"}),
                )),
                Some(text("notes written")),
            ],
        ),
        // The redo hangs until the task deadline cancels it.
        ("todo-2-redo", vec![None]),
    ])
    .await;
    let _ = run(cli_with(root.path(), data.path(), &server.url, "15").args(["-p", "cancel"])).await;
    assert_eq!(server.requests("todo-2-redo").len(), 1);
    // todo-1 reached the workspace; todo-3 (after todo-2 in the merge order) did not.
    assert_eq!(
        std::fs::read_to_string(root.path().join("src/lib.rs")).unwrap(),
        LIB.replace("    1\n", "    10\n")
    );
    assert!(!root.path().join("notes.md").exists());
    // The partial report is the recorded result of the call.
    let sessions: String = std::fs::read_dir(data.path().join("sessions"))
        .unwrap()
        .flatten()
        .map(|entry| std::fs::read_to_string(entry.path()).unwrap_or_default())
        .collect();
    for wanted in [
        "src/lib.rs [todo-1]: direct write",
        "src/lib.rs [todo-2]: conflict unresolved",
        "the redo stopped",
        "Merging stopped",
        "/undo reverts it",
        "todo-2 was being merged",
        "Not merged, still pending",
    ] {
        assert!(sessions.contains(wanted), "{wanted}: {sessions}");
    }
    // The layers of the unmerged items survive the end of the task.
    let kept: Vec<String> = overlay_files(data.path())
        .iter()
        .map(|path| path.to_string_lossy().replace('\\', "/"))
        .collect();
    assert!(
        kept.iter()
            .any(|path| path.contains("/todo-3/overlay.json")),
        "{kept:?}"
    );
    assert!(
        kept.iter()
            .any(|path| path.contains("/todo-2/overlay.json")),
        "{kept:?}"
    );
}

#[tokio::test]
async fn overlay_items_share_one_check_slot_in_turn_and_see_their_own_layer() {
    let script = "import sys, time\nlog = sys.argv[1]\nwith open(log, 'a') as f:\n    f.write('start' + chr(10))\nprint(open('src/lib.rs').read())\ntime.sleep(0.8)\nwith open(log, 'a') as f:\n    f.write('end' + chr(10))\n";
    let root = lib_repo(&[("check.py", script)]);
    let data = tempfile::tempdir().unwrap();
    let log = data.path().join("check.log");
    let command = format!(
        "{} check.py {}",
        python(),
        log.to_string_lossy().replace('\\', "/")
    );
    std::fs::write(
        data.path().join("config.json"),
        json!({ "overlay_check": command }).to_string(),
    )
    .unwrap();
    let todos = json!({"todos":[
        {"content":"one","status":"pending","writes":["src/lib.rs::one"]},
        {"content":"two","status":"pending","writes":["src/lib.rs::two"]}
    ]});
    let server = mock(vec![
        (
            "parent",
            vec![
                Some(tool("plan", "todo", todos)),
                Some(tool(
                    "run",
                    "execute_plan",
                    json!({"parallel":true,"isolation":"overlay"}),
                )),
                Some(text("done")),
            ],
        ),
        (
            "todo-1",
            vec![
                Some(edit(
                    "e1",
                    "fn one() {\n    1\n}",
                    "fn one() {\n    1111\n}",
                )),
                Some(tool("c1", "check", json!({}))),
                Some(text("checked")),
            ],
        ),
        (
            "todo-2",
            vec![
                Some(edit(
                    "e2",
                    "fn two() {\n    2\n}",
                    "fn two() {\n    2222\n}",
                )),
                Some(tool("c2", "check", json!({}))),
                Some(text("checked")),
            ],
        ),
    ])
    .await;
    let _ = run(cli(root.path(), data.path(), &server.url).args(["-p", "check"])).await;
    let tools = server.requests("todo-1")[0]["tools"].to_string();
    assert!(tools.contains("\"check\""), "{tools}");
    let one = last_result(&server.requests("todo-1")[2]);
    let two = last_result(&server.requests("todo-2")[2]);
    for (result, mine, theirs) in [(&one, "1111", "2222"), (&two, "2222", "1111")] {
        assert!(result.contains("exit 0 (passed)"), "{result}");
        assert!(
            result.contains(mine) && !result.contains(theirs),
            "{result}"
        );
    }
    // One check at a time: the runs never overlapped.
    let lines = std::fs::read_to_string(&log).unwrap();
    assert_eq!(lines.replace("\r\n", "\n"), "start\nend\nstart\nend\n");
    // The slot keeps its copy for the next check.
    assert!(
        data.path()
            .join("check-slots")
            .read_dir()
            .unwrap()
            .next()
            .is_some()
    );
    assert_eq!(
        std::fs::read_to_string(root.path().join("src/lib.rs")).unwrap(),
        "fn one() {\n    1111\n}\n\nfn two() {\n    2222\n}\n"
    );
}
