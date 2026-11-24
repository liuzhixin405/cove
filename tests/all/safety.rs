//! CLI tests for permission modes, safety blocks, policies, guards and completion gates.

use crate::common::{self, Launch};
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
    command.env_remove("COVE_CONFIG").env_remove("COVE_PROFILE");
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
async fn auto_mode_without_a_terminal_refuses_the_call_and_the_task_goes_on() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool("w", "write", json!({"path":"a.txt","content":"auto"})),
        tool("s", "shell", json!({"command":"echo x > b.txt"})),
        text("Wrote a.txt; creating b.txt needs your approval, run: echo x > b.txt"),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["-p", "write"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("a.txt")).unwrap(),
        "auto"
    );
    assert!(!root.path().join("b.txt").exists());
    // The refusal reached the model, which answered: the task was not stopped.
    assert_eq!(requests.len(), 3, "{stderr}");
    assert!(
        tool_result(&requests[2]).contains("nobody can be asked"),
        "{}",
        tool_result(&requests[2])
    );
    assert!(output.status.success(), "{stderr}");
    assert!(
        stderr.contains("未执行") && stderr.contains("--permission bypass"),
        "{stderr}"
    );
}

/// docs/optimizations/0038: a refused install is not run through a script the task writes.
#[tokio::test]
async fn after_a_refusal_a_script_the_task_wrote_is_not_run_either() {
    let script = "open('ran.txt', 'w').write('ran')\n";
    let sequence = |refuse_first: bool| {
        let mut responses = Vec::new();
        if refuse_first {
            responses.push(tool(
                "pip",
                "shell",
                json!({"command":"python -m pip install cove-0038-no-such-package"}),
            ));
        }
        responses.push(tool(
            "w",
            "write",
            json!({"path":"_install.py","content":script}),
        ));
        responses.push(tool(
            "run",
            "shell",
            json!({"command":format!("{} _install.py", python())}),
        ));
        responses.push(text("Done."));
        responses
    };
    // Without a refusal, writing a script and running it needs no approval.
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(sequence(false)).await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["-p", "run a script"])
        .launch()
        .await
        .unwrap();
    let requests = server.await.unwrap();
    assert_eq!(
        requests.len(),
        3,
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    assert!(root.path().join("ran.txt").exists());

    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(sequence(true)).await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["-p", "install pytest"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 4, "{stderr}");
    assert!(
        tool_result(&requests[1]).contains("nobody can be asked"),
        "{}",
        tool_result(&requests[1])
    );
    // The script was written, but running it was refused like the install.
    assert!(root.path().join("_install.py").exists());
    assert!(!root.path().join("ran.txt").exists(), "{stderr}");
    let refused = tool_result(&requests[3]);
    assert!(
        refused.contains("nobody can be asked") && refused.contains("不要用脚本绕过"),
        "{refused}"
    );
    assert!(
        stderr.contains("未执行") && stderr.contains("_install.py"),
        "{stderr}"
    );
}

#[tokio::test]
async fn inline_code_runs_in_auto_until_something_is_refused() {
    // docs/optimizations/0043: `python -c` runs like a script the task wrote.
    let inline = format!(r#"{} -c "open('ran.txt', 'w').write('ran')""#, python());
    let sequence = |refuse_first: bool| {
        let mut responses = Vec::new();
        if refuse_first {
            responses.push(tool(
                "pip",
                "shell",
                json!({"command":"python -m pip install cove-0043-no-such-package"}),
            ));
        }
        responses.push(tool("run", "shell", json!({"command":inline})));
        responses.push(text("Done."));
        responses
    };
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(sequence(false)).await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["-p", "check a value"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2, "{stderr}");
    assert!(root.path().join("ran.txt").exists(), "{stderr}");
    assert!(!stderr.contains("未执行"), "{stderr}");

    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(sequence(true)).await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["-p", "install a package"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 3, "{stderr}");
    // After the install was refused, the inline code is refused too and does not run.
    assert!(!root.path().join("ran.txt").exists(), "{stderr}");
    let refused = tool_result(&requests[2]);
    assert!(
        refused.contains("nobody can be asked") && refused.contains("内联代码绕过"),
        "{refused}"
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
async fn memory_reads_without_scope_using_the_short_id_of_the_index() {
    use cove::{
        memory::store::{Memory, MemoryArgs, Scope},
        safety::permission::{LocalAuthorizer, Mode},
    };
    use std::sync::Arc;
    use tokio_util::sync::CancellationToken;

    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let memory = Memory::new(
        data.path(),
        root.path(),
        Arc::new(LocalAuthorizer {
            mode: Mode::Auto,
            input: None,
        }),
    )
    .unwrap();
    let saved = memory
        .execute(
            MemoryArgs::Add {
                scope: Scope::Global,
                title: "analysis".into(),
                content: "previous analysis is available".into(),
                source: "test".into(),
            },
            &CancellationToken::new(),
        )
        .unwrap();
    let id = saved["saved"].as_str().unwrap();
    let responses = vec![
        tool("read", "memory", json!({"action":"read", "id":&id[..8]})),
        text("Analysis recovered."),
    ];
    let (url, server) = mock_server(responses).await;
    let output = cli(root.path(), data.path(), &url, "auto")
        .args(["--no-auto", "-p", "read the saved analysis"])
        .launch()
        .await
        .unwrap();
    assert!(output.status.success());
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 2);
    let definition = requests[0]["tools"]
        .as_array()
        .unwrap()
        .iter()
        .find(|tool| tool["function"]["name"] == "memory")
        .unwrap();
    assert_eq!(
        definition["function"]["parameters"]["required"],
        json!(["action"])
    );
    assert!(tool_result(&requests[1]).contains("previous analysis is available"));
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
    assert!(stderr.contains("验证通过"), "{stderr}");
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

const PAD: &str = "\n\n补充：这一段只是为了让回答足够长，不涉及任何具体的代码位置，也不提到任何文件或者符号的名字，读者可以忽略。补充：这一段只是为了让回答足够长，不涉及任何具体的代码位置，也不提到任何文件或者符号的名字，读者可以忽略。补充：这一段只是为了让回答足够长，不涉及任何具体的代码位置，也不提到任何文件或者符号的名字，读者可以忽略。";

fn cited_repo() -> tempfile::TempDir {
    let root = tempfile::tempdir().unwrap();
    std::fs::create_dir_all(root.path().join("shop")).unwrap();
    std::fs::write(
        root.path().join("shop/pricing.py"),
        "MEMBER_RATE = 0.9\n\n\ndef apply_discount(order):\n    if order.member:\n        return order.total * MEMBER_RATE\n    return order.total\n",
    )
    .unwrap();
    let mut filler = String::new();
    for n in 1..=60 {
        filler.push_str(&if n == 50 {
            "def checkout(order):\n".to_owned()
        } else {
            format!("# {n}\n")
        });
    }
    std::fs::write(root.path().join("shop/checkout.py"), filler).unwrap();
    root
}

#[tokio::test]
async fn a_wrong_code_reference_in_an_answer_is_handed_back_once() {
    let root = cited_repo();
    let data = tempfile::tempdir().unwrap();
    let wrong = format!("只有会员 9 折，见 `shop/pricing.py:40` 的 `apply_discount`。{PAD}");
    let right = format!("只有会员 9 折，见 `shop/pricing.py:4` 的 `apply_discount`。{PAD}");
    let (url, server) = mock_server(vec![
        tool("r", "read", json!({"path":"shop/pricing.py"})),
        text(&wrong),
        text(&right),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "default")
        .args(["-p", "折扣怎么算？只回答"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert!(output.status.success(), "{stderr}");
    assert_eq!(requests.len(), 3, "{stderr}");
    let nudge = tool_result(&requests[2]);
    assert!(
        nudge.contains("could not be verified") && nudge.contains("the file has 7 lines"),
        "{nudge}"
    );
    assert!(String::from_utf8_lossy(&output.stdout).contains("shop/pricing.py:4"));
    let report = std::fs::read_dir(data.path().join("acceptance"))
        .unwrap()
        .next()
        .unwrap()
        .unwrap()
        .path();
    let report: Value = serde_json::from_str(&std::fs::read_to_string(report).unwrap()).unwrap();
    assert_eq!(report["citations"]["nudged"], json!(true), "{report}");
    assert_eq!(report["citations"]["passed"], json!(1), "{report}");
}

#[tokio::test]
async fn a_line_offset_is_only_reported_and_costs_no_model_call() {
    let root = cited_repo();
    let data = tempfile::tempdir().unwrap();
    let answer = format!("结算入口是 `checkout`（`shop/checkout.py:2`）。{PAD}");
    let (url, server) = mock_server(vec![
        tool("r", "read", json!({"path":"shop/checkout.py"})),
        text(&answer),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "default")
        .args(["-p", "结算入口在哪？只回答"])
        .launch()
        .await
        .unwrap();
    let stderr = String::from_utf8_lossy(&output.stderr);
    let requests = server.await.unwrap();
    assert!(output.status.success(), "{stderr}");
    assert_eq!(requests.len(), 2, "{stderr}");
    assert!(
        stderr.contains("[引用核实] shop/checkout.py:2 → 实际在第 50 行"),
        "{stderr}"
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

fn failing_shell(id: &str) -> String {
    tool(id, "shell", json!({"command":"exit 1"}))
}

#[tokio::test]
async fn a_task_without_progress_stops_and_summarizes_in_print_mode() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    // Round 1 is new information; rounds 2-13 bring nothing new, so the 14th request
    // is the tool-free closing summary.
    let mut responses: Vec<String> = (0..13).map(|i| failing_shell(&format!("s{i}"))).collect();
    responses.push(text(
        "exit 1 keeps failing the same way; I need you to tell me what the command should do.",
    ));
    let (url, server) = mock_server(responses).await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "run it"])
        .launch()
        .await
        .unwrap();
    let all = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 14, "{all}");
    assert!(!output.status.success(), "{all}");
    let last = &requests[13];
    assert!(
        last.get("tools")
            .is_none_or(|tools| tools.as_array().is_some_and(Vec::is_empty)),
        "{last}"
    );
    assert!(last.to_string().contains("cove stopped the task"), "{last}");
    // The warning came at round 7 (6 idle rounds).
    assert!(requests[7].to_string().contains("[progress] 最近 6 轮"));
    assert!(!requests[6].to_string().contains("[progress]"));
    assert!(all.contains("[summary]"), "{all}");
    assert!(all.contains("因连续 12 轮没有进展提前停止"), "{all}");
    assert!(all.contains("没有进展，已停下"), "{all}");
    let trace = std::fs::read_to_string(data.path().join("trace.jsonl")).unwrap();
    let task = trace
        .lines()
        .rfind(|line| line.contains("\"kind\":\"task\""))
        .unwrap();
    assert!(
        task.contains("\"stalled\":true") && task.contains("\"task_calls\":14"),
        "{task}"
    );
    assert!(
        task.contains("\"idle_max\":12") && task.contains("E5008"),
        "{task}"
    );
}

/// Same command, same (empty) output every round; `writes` decides whether it changes
/// a file. With `stall_rounds` 4 the work tree tells the two apart.
async fn shell_rounds(writes: bool) -> (Vec<Value>, String, bool) {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    std::fs::write(
        data.path().join("config.json"),
        json!({"stall_rounds":4}).to_string(),
    )
    .unwrap();
    let script = if writes {
        "import time; open('out.txt', 'w').write(str(time.time_ns()))"
    } else {
        "import time; time.time_ns()"
    };
    let command = format!("{} -c \"{script}\"", python());
    let rounds = if writes { 9 } else { 5 };
    let mut responses: Vec<String> = (0..rounds)
        .map(|i| tool(&format!("s{i}"), "shell", json!({ "command": command })))
        .collect();
    responses.push(text("Ran the script repeatedly."));
    let (url, server) = mock_server(responses).await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["-p", "run the script"])
        .launch()
        .await
        .unwrap();
    let all = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    (server.await.unwrap(), all, output.status.success())
}

#[tokio::test]
async fn files_changed_by_shell_are_progress_and_unchanged_ones_are_not() {
    let (requests, all, ok) = shell_rounds(true).await;
    assert_eq!(requests.len(), 10, "{all}");
    assert!(ok, "{all}");
    assert!(!all.contains("没有进展"), "{all}");
    let (requests, all, ok) = shell_rounds(false).await;
    assert_eq!(requests.len(), 6, "{all}");
    assert!(!ok, "{all}");
    assert!(all.contains("因连续 4 轮没有进展提前停止"), "{all}");
}

#[tokio::test]
async fn the_model_call_limit_also_prints_the_summary() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let (url, server) = mock_server(vec![
        tool("a", "glob", json!({"pattern":"*.a"})),
        tool("b", "glob", json!({"pattern":"*.b"})),
        tool("c", "glob", json!({"pattern":"*.c"})),
        text("Looked for three kinds of files; none found yet."),
    ])
    .await;
    let output = cli(root.path(), data.path(), &url, "bypass")
        .args(["--max-turns", "3", "-p", "find files"])
        .launch()
        .await
        .unwrap();
    let all = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    let requests = server.await.unwrap();
    assert_eq!(requests.len(), 4, "{all}");
    assert!(requests[3].to_string().contains("model call limit reached"));
    assert!(!output.status.success(), "{all}");
    assert!(
        all.contains("[summary]") && all.contains("因用完 3 次模型调用停止"),
        "{all}"
    );
    assert!(all.contains("已用完 3 次模型调用"), "{all}");
}
