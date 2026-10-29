mod common;
use cove::{
    domain::{Block, Event, Message, ModelRequest, ModelResponse, Provider, Result, Role, Usage},
    engine::agent::{Agent, Limits},
    extensions::hooks::{HookState, Hooks, Lifecycle},
    extensions::mcp::{Mcp, McpArgs},
    extensions::skills::{SkillArgs, Skills},
    extensions::{
        Catalog, ExtensionConfig, HookConfig, HookEvent, Registry, ServerConfig, Sourced,
    },
    memory::store::{Memory, MemoryArgs, Scope},
    safety::permission::{LocalAuthorizer, Mode},
    session::workspace::Workspace,
    session::{Session, SessionPersistence, State, Store},
    tools::LocalTools,
};
use futures_util::future::BoxFuture;
use serde_json::json;
use std::{
    collections::{BTreeMap, VecDeque},
    path::{Path, PathBuf},
    sync::{Arc, Mutex},
};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

fn authorizer(mode: Mode) -> Arc<LocalAuthorizer> {
    Arc::new(LocalAuthorizer { mode, input: None })
}
fn skill(root: &Path, name: &str) {
    std::fs::create_dir_all(root).unwrap();
    std::fs::write(
        root.join("SKILL.md"),
        format!("---\nname: {name}\ndescription: Coding guidance\n---\nPreserve existing tests.\n"),
    )
    .unwrap();
}

#[test]
fn skill_discovery_is_lazy_sourced_and_disableable() {
    let source = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    skill(source.path(), "coding");
    std::fs::write(source.path().join("reference.txt"), "specific reference").unwrap();
    let registry = Arc::new(Registry::open(data.path()).unwrap());
    let skills = Skills::discover(
        &[Sourced {
            id: String::new(),
            plugin: Some("plugin".into()),
            value: source.path().canonicalize().unwrap(),
        }],
        registry.clone(),
    )
    .unwrap();
    let list = skills.execute(SkillArgs::List).unwrap().to_string();
    assert!(list.contains("plugin/coding") && !list.contains("Preserve existing tests"));
    assert!(
        skills
            .execute(SkillArgs::Load {
                name: "plugin/coding".into()
            })
            .unwrap()
            .to_string()
            .contains("Preserve existing tests")
    );
    assert!(
        skills
            .execute(SkillArgs::Read {
                name: "plugin/coding".into(),
                path: "reference.txt".into()
            })
            .unwrap()
            .to_string()
            .contains("specific reference")
    );
    assert!(
        skills
            .execute(SkillArgs::Read {
                name: "plugin/coding".into(),
                path: "../escape".into()
            })
            .is_err()
    );
    registry
        .change(|state| {
            state.disabled.insert("plugin:plugin".into());
            Ok(())
        })
        .unwrap();
    assert_eq!(skills.execute(SkillArgs::List).unwrap(), json!([]));
    assert!(
        skills
            .execute(SkillArgs::Load {
                name: "plugin/coding".into()
            })
            .is_err()
    );
    assert!(
        !Registry::open(data.path())
            .unwrap()
            .active("skill", "plugin/coding", Some("plugin"))
            .unwrap()
    );
}

#[test]
fn malformed_skills_and_duplicate_names_are_explicit_errors() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    skill(&root.path().join("a"), "same");
    skill(&root.path().join("b"), "same");
    let source = Sourced {
        id: String::new(),
        plugin: None,
        value: root.path().canonicalize().unwrap(),
    };
    let registry = Arc::new(Registry::open(data.path()).unwrap());
    assert!(Skills::discover(std::slice::from_ref(&source), registry.clone()).is_err());
    std::fs::write(root.path().join("b").join("SKILL.md"), "not valid metadata").unwrap();
    assert!(Skills::discover(&[source], registry).is_err());
}

#[tokio::test]
async fn memory_keeps_sources_isolates_projects_and_shares_only_global_notes() {
    let root = tempfile::tempdir().unwrap();
    let other = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let memory = Memory::new(
        data.path(),
        &root.path().canonicalize().unwrap(),
        authorizer(Mode::Auto),
    )
    .unwrap();
    let second = Memory::new(
        data.path(),
        &other.path().canonicalize().unwrap(),
        authorizer(Mode::Readonly),
    )
    .unwrap();
    let cancel = CancellationToken::new();
    let add = || MemoryArgs::Add {
        scope: Scope::Project,
        title: "build".into(),
        content: "cargo test".into(),
        source: "user request".into(),
    };
    memory.authorize(&add(), cancel.clone()).await.unwrap();
    let id = memory.execute(add(), &cancel).unwrap()["saved"]
        .as_str()
        .unwrap()
        .to_owned();
    let alias = Memory::new(
        data.path(),
        &root.path().join("."),
        authorizer(Mode::Readonly),
    )
    .unwrap();
    assert_eq!(
        alias
            .execute(
                MemoryArgs::List {
                    scope: Scope::Project
                },
                &cancel
            )
            .unwrap()
            .as_array()
            .unwrap()
            .len(),
        1
    );
    assert_eq!(
        second
            .execute(
                MemoryArgs::List {
                    scope: Scope::Project
                },
                &cancel
            )
            .unwrap(),
        json!([])
    );
    assert!(second.authorize(&add(), cancel.clone()).await.is_err());
    assert!(
        memory
            .execute(
                MemoryArgs::Read {
                    scope: Scope::Project,
                    id: id.clone()
                },
                &cancel
            )
            .unwrap()["source"]
            == "user request"
    );
    memory
        .execute(
            MemoryArgs::Add {
                scope: Scope::Global,
                title: "shared".into(),
                content: "read the README".into(),
                source: "operator".into(),
            },
            &cancel,
        )
        .unwrap();
    assert_eq!(
        second
            .execute(
                MemoryArgs::List {
                    scope: Scope::Global
                },
                &cancel
            )
            .unwrap()
            .as_array()
            .unwrap()
            .len(),
        1
    );
    assert_eq!(
        memory
            .execute(
                MemoryArgs::Search {
                    scope: Scope::Project,
                    query: "cargo".into()
                },
                &cancel
            )
            .unwrap()
            .as_array()
            .unwrap()
            .len(),
        1
    );
    memory
        .execute(
            MemoryArgs::Update {
                scope: Scope::Project,
                id: id.clone(),
                title: "updated".into(),
                content: "cargo check".into(),
                source: "new evidence".into(),
            },
            &cancel,
        )
        .unwrap();
    assert_eq!(
        memory
            .execute(
                MemoryArgs::Read {
                    scope: Scope::Project,
                    id: id.clone()
                },
                &cancel
            )
            .unwrap()["source"],
        "new evidence"
    );
    memory
        .execute(
            MemoryArgs::Delete {
                scope: Scope::Project,
                id: id.clone(),
            },
            &cancel,
        )
        .unwrap();
    assert!(
        memory
            .execute(
                MemoryArgs::Read {
                    scope: Scope::Project,
                    id
                },
                &cancel
            )
            .is_err()
    );
    assert!(
        memory
            .execute(
                MemoryArgs::Read {
                    scope: Scope::Project,
                    id: "../escape".into()
                },
                &cancel
            )
            .is_err()
    );
}

#[test]
fn plugins_compose_namespaces_without_executing_commands() {
    let source = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    skill(&source.path().join("skills"), "coding");
    let manifest = source.path().join("plugin.json");
    std::fs::write(
        &manifest,
        json!({"version":1,"name":"example","skills":["skills"],
        "mcp":{"tools":{"command":"must-not-run","tools":["echo"]}},
        "hooks":[{"name":"before","event":"before_tool","command":"must-not-run"}]})
        .to_string(),
    )
    .unwrap();
    Catalog::validate_plugin(&manifest.canonicalize().unwrap()).unwrap();
    let registry = Registry::open(data.path()).unwrap();
    let config = ExtensionConfig {
        plugins: vec![manifest],
        ..Default::default()
    };
    let catalog = Catalog::load(config, source.path(), &registry).unwrap();
    assert_eq!(catalog.servers[0].id, "example/tools");
    assert_eq!(catalog.hooks[0].id, "example/before");
    assert!(catalog.plugins.contains_key("example"));
}

#[test]
fn memory_roundtrips_the_full_byte_limit_even_with_json_escaping() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let memory = Memory::new(data.path(), root.path(), authorizer(Mode::Auto)).unwrap();
    let content = "\"".repeat(25 * 1024);
    let cancel = CancellationToken::new();
    let saved = memory
        .execute(
            MemoryArgs::Add {
                scope: Scope::Project,
                title: "escaped".into(),
                content: content.clone(),
                source: "fixture".into(),
            },
            &cancel,
        )
        .unwrap();
    let id = saved["saved"].as_str().unwrap().to_owned();
    assert_eq!(
        memory
            .execute(
                MemoryArgs::Read {
                    scope: Scope::Project,
                    id
                },
                &cancel
            )
            .unwrap()["content"],
        content
    );
    assert!(
        memory
            .execute(
                MemoryArgs::Add {
                    scope: Scope::Project,
                    title: "too big".into(),
                    content: "a".repeat(25 * 1024 + 1),
                    source: "fixture".into()
                },
                &cancel
            )
            .is_err()
    );
}

fn hook_command(allow: bool) -> String {
    if cfg!(windows) {
        format!(
            "if ($env:COVE_API_KEY) {{ exit 8 }}; $data=ConvertFrom-Json $env:COVE_HOOK_INPUT; if ($data.version -ne 1) {{ exit 9 }}; [Console]::Out.Write('{{\"decision\":\"{}\",\"message\":\"fixture\"}}')",
            if allow { "allow" } else { "deny" }
        )
    } else {
        format!(
            "test -z \"$COVE_API_KEY\" || exit 8; test -n \"$COVE_HOOK_INPUT\" || exit 9; printf '%s' '{{\"decision\":\"{}\",\"message\":\"fixture\"}}'",
            if allow { "allow" } else { "deny" }
        )
    }
}

fn hooks(
    root: &Path,
    store: &Store,
    workspace: Arc<Workspace>,
    event: HookEvent,
    allow: bool,
    trusted: bool,
) -> Arc<Hooks> {
    Arc::new(Hooks::new(
        vec![Sourced {
            id: "fixture".into(),
            plugin: None,
            value: HookConfig {
                name: "fixture".into(),
                event,
                command: hook_command(allow),
                timeout_seconds: 5,
                matcher: None,
                run_async: false,
                claude: false,
            },
        }],
        Arc::new(Registry::open(store.root()).unwrap()),
        authorizer(Mode::Auto),
        workspace,
        root.into(),
        trusted,
    ))
}

#[tokio::test]
async fn hook_contract_and_session_end_are_bounded_and_exactly_once_per_visit() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Arc::new(Workspace::new(root.path().into(), store.clone()).unwrap());
    let mut session = Session::new(root.path().into(), "Openai".into(), "mock".into(), None);
    let hooks = hooks(
        root.path(),
        &store,
        workspace,
        HookEvent::SessionEnd,
        true,
        true,
    );
    for event in [
        HookEvent::SessionStart,
        HookEvent::SessionEnd,
        HookEvent::SessionEnd,
    ] {
        hooks
            .run(
                event,
                &mut session,
                &store,
                json!({}),
                CancellationToken::new(),
            )
            .await
            .unwrap();
    }
    assert_eq!(session.hook_runs.len(), 1);
    assert_eq!(session.hook_runs[0].state, HookState::Completed);
    assert_eq!(session.hook_runs[0].outcome, "fixture");
}

struct Fake(Mutex<VecDeque<ModelResponse>>);
impl Provider for Fake {
    fn stream(
        &self,
        _: ModelRequest,
        _: mpsc::Sender<Event>,
        _: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            Ok(self
                .0
                .lock()
                .unwrap()
                .pop_front()
                .expect("unexpected request"))
        })
    }
}
fn response(blocks: Vec<Block>) -> ModelResponse {
    ModelResponse {
        message: Message {
            role: Role::Assistant,
            blocks,
        },
        usage: Usage::default(),
        complete: true,
        usage_reported: true,
        model: None,
    }
}

#[tokio::test]
async fn denied_before_tool_hook_prevents_the_actual_file_write() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Arc::new(Workspace::new(root.path().into(), store.clone()).unwrap());
    workspace.begin().unwrap();
    let provider = Arc::new(Fake(Mutex::new(
        vec![response(vec![Block::ToolUse {
            id: "write".into(),
            name: "write".into(),
            input: json!({"path":"file","content":"bad"}),
            extra: None,
        }])]
        .into(),
    )));
    let tools = Arc::new(
        LocalTools::new(root.path().into(), authorizer(Mode::Auto))
            .unwrap()
            .with_workspace(workspace.clone()),
    );
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: None,
            prices: None,
        },
    )
    .unwrap()
    .with_hooks(hooks(
        root.path(),
        &store,
        workspace,
        HookEvent::BeforeTool,
        false,
        true,
    ));
    let mut session = Session::new(root.path().into(), "Openai".into(), "mock".into(), None);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    assert!(
        agent
            .run_in(
                &mut session,
                Some("task".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    drain.await.unwrap();
    assert!(!root.path().join("file").exists());
    assert_eq!(
        session.pending[0].state,
        cove::session::CallState::Uncertain
    );
    assert_eq!(session.hook_runs[0].state, HookState::Uncertain);
}

#[tokio::test]
async fn hooks_cannot_execute_without_explicit_extension_trust() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Arc::new(Workspace::new(root.path().into(), store.clone()).unwrap());
    let hooks = hooks(
        root.path(),
        &store,
        workspace,
        HookEvent::TaskStart,
        true,
        false,
    );
    let mut session = Session::new(root.path().into(), "Openai".into(), "mock".into(), None);
    assert!(
        hooks
            .run(
                HookEvent::TaskStart,
                &mut session,
                &store,
                json!({}),
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    assert_eq!(session.hook_runs[0].state, HookState::Uncertain);
}

#[tokio::test]
async fn continuing_after_tool_approval_denial_does_not_repeat_completed_before_hooks() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Arc::new(Workspace::new(root.path().into(), store.clone()).unwrap());
    workspace.begin().unwrap();
    let provider = Arc::new(Fake(Mutex::new(
        vec![response(vec![Block::ToolUse {
            id: "write".into(),
            name: "write".into(),
            input: json!({"path":"file","content":"blocked"}),
            extra: None,
        }])]
        .into(),
    )));
    let tools = Arc::new(
        LocalTools::new(root.path().into(), authorizer(Mode::Readonly))
            .unwrap()
            .with_workspace(workspace.clone()),
    );
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: None,
            prices: None,
        },
    )
    .unwrap()
    .with_hooks(hooks(
        root.path(),
        &store,
        workspace,
        HookEvent::BeforeTool,
        true,
        true,
    ));
    let mut session = Session::new(root.path().into(), "Openai".into(), "mock".into(), None);
    for prompt in [Some("task".into()), None] {
        let (tx, mut rx) = mpsc::channel(64);
        let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
        assert!(
            agent
                .run_in(&mut session, prompt, &store, tx, CancellationToken::new())
                .await
                .is_err()
        );
        drain.await.unwrap();
    }
    assert_eq!(session.hook_runs.len(), 1);
    assert_eq!(session.hook_runs[0].state, HookState::Completed);
    assert!(!root.path().join("file").exists());
}

fn mcp(
    root: &Path,
    store: &Store,
    command: String,
    args: Vec<String>,
    mode: Mode,
    timeout_seconds: u64,
) -> Mcp {
    let workspace = Arc::new(Workspace::new(root.into(), store.clone()).unwrap());
    workspace.begin().unwrap();
    Mcp::new(
        vec![Sourced {
            id: "fixture".into(),
            plugin: None,
            value: ServerConfig {
                command,
                args,
                env: Vec::new(),
                tools: vec!["echo".into(), "fail".into(), "slow".into()],
                timeout_seconds,
                ..ServerConfig::default()
            },
        }],
        root,
        common::exe().to_path_buf(),
        true,
        Arc::new(Registry::open(store.root()).unwrap()),
        authorizer(mode),
        workspace,
        store.root().join("mcp-oauth"),
    )
}

fn mcp_fixture(root: &Path) -> PathBuf {
    let path = root.join("mcp_fixture.py");
    std::fs::write(&path,r#"import sys, json, time
for line in sys.stdin:
    req = json.loads(line)
    if "id" not in req:
        continue
    method, params = req["method"], req.get("params", {})
    if method == "initialize":
        result = {"protocolVersion": params["protocolVersion"], "capabilities": {"tools": {}, "resources": {}}, "serverInfo": {"name":"fixture", "version":"1"}}
    elif method == "tools/list":
        result = {"tools":[{"name":"echo", "description":"echo", "inputSchema":{"type":"object","properties":{"value":{"type":"string"}}}}]}
        if not params.get("cursor"):
            result["nextCursor"] = "second"
    elif method == "tools/call":
        name = params["name"]
        with open("mcp-calls.txt","a") as f:
            f.write(name + "\n")
        if name == "fail":
            sys.exit(2)
        if name == "slow":
            import subprocess
            subprocess.Popen([sys.executable, "-c", "import time; time.sleep(3); open('late-child','w').write('bad')"])
            time.sleep(10)
        result = {"content":[{"type":"text","text":json.dumps(params["arguments"])}],"isError":False}
    elif method == "resources/list":
        result = {"resources":[{"uri":"fixture://note","name":"note"}]}
    elif method == "resources/read":
        result = {"contents":[{"uri":"fixture://note","text":"actual resource"}]}
    else:
        result = {}
    print(json.dumps({"jsonrpc":"2.0","id":req["id"],"result":result}), flush=True)
"#).unwrap();
    path
}
fn python() -> String {
    if cfg!(windows) {
        "python".into()
    } else {
        "python3".into()
    }
}
fn call(tool: &str) -> McpArgs {
    McpArgs::Call {
        server: "fixture".into(),
        tool: tool.into(),
        arguments: json!({"value":"actual output"})
            .as_object()
            .unwrap()
            .clone(),
    }
}

#[tokio::test]
async fn official_mcp_client_initializes_discovers_pages_calls_and_reads_resources() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let script = mcp_fixture(root.path());
    let mcp = mcp(
        root.path(),
        &store,
        python(),
        vec!["-u".into(), script.to_string_lossy().into()],
        Mode::Auto,
        10,
    );
    assert!(
        mcp.execute(McpArgs::Servers, CancellationToken::new())
            .await
            .unwrap()
            .content
            .contains("fixture")
    );
    assert!(!root.path().join("mcp-calls.txt").exists());
    let discovered = mcp
        .execute(
            McpArgs::Tools {
                server: "fixture".into(),
            },
            CancellationToken::new(),
        )
        .await
        .unwrap();
    assert!(discovered.content.contains("inputSchema"));
    let result = mcp
        .execute(call("echo"), CancellationToken::new())
        .await
        .unwrap();
    assert!(!result.failed && result.content.contains("actual output"));
    assert!(
        mcp.execute(
            McpArgs::Resources {
                server: "fixture".into()
            },
            CancellationToken::new()
        )
        .await
        .unwrap()
        .content
        .contains("fixture://note")
    );
    assert!(
        mcp.execute(
            McpArgs::Read {
                server: "fixture".into(),
                uri: "fixture://note".into()
            },
            CancellationToken::new()
        )
        .await
        .unwrap()
        .content
        .contains("actual resource")
    );
    mcp.close(None, None).await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("mcp-calls.txt"))
            .unwrap()
            .lines()
            .collect::<Vec<_>>(),
        vec!["echo"]
    );
}

#[tokio::test]
async fn mcp_disconnect_is_not_replayed_and_next_operation_can_reconnect() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let script = mcp_fixture(root.path());
    let mcp = mcp(
        root.path(),
        &store,
        python(),
        vec!["-u".into(), script.to_string_lossy().into()],
        Mode::Auto,
        10,
    );
    assert!(
        mcp.execute(call("fail"), CancellationToken::new())
            .await
            .is_err()
    );
    assert!(
        mcp.execute(call("echo"), CancellationToken::new())
            .await
            .is_ok()
    );
    mcp.close(None, None).await.unwrap();
    assert_eq!(
        std::fs::read_to_string(root.path().join("mcp-calls.txt"))
            .unwrap()
            .lines()
            .collect::<Vec<_>>(),
        vec!["fail", "echo"]
    );
}

#[tokio::test]
async fn mcp_readonly_and_allowlist_rejections_do_not_start_a_process() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let mcp = mcp(
        root.path(),
        &store,
        "must-not-run".into(),
        Vec::new(),
        Mode::Readonly,
        1,
    );
    assert!(
        mcp.execute(call("forbidden"), CancellationToken::new())
            .await
            .is_err()
    );
    assert!(
        mcp.execute(
            McpArgs::Tools {
                server: "fixture".into()
            },
            CancellationToken::new()
        )
        .await
        .is_err()
    );
    mcp.close(None, None).await.unwrap();
}

#[test]
fn invalid_server_configurations_and_plugin_versions_are_rejected() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let registry = Registry::open(data.path()).unwrap();
    let config = ExtensionConfig {
        mcp: BTreeMap::from([(
            "bad".into(),
            ServerConfig {
                command: "test".into(),
                args: Vec::new(),
                env: Vec::new(),
                tools: Vec::new(),
                timeout_seconds: 0,
                ..ServerConfig::default()
            },
        )]),
        ..Default::default()
    };
    assert!(Catalog::load(config, root.path(), &registry).is_err());
    let path = root.path().join("bad.json");
    std::fs::write(&path, r#"{"version":9,"name":"bad"}"#).unwrap();
    assert!(Catalog::validate_plugin(&path).is_err());
}

#[tokio::test]
async fn after_tool_hook_failure_does_not_replay_a_durable_successful_write() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let workspace = Arc::new(Workspace::new(root.path().into(), store.clone()).unwrap());
    workspace.begin().unwrap();
    let provider = Arc::new(Fake(Mutex::new(
        vec![
            response(vec![Block::ToolUse {
                id: "write".into(),
                name: "write".into(),
                input: json!({"path":"file","content":"once"}),
                extra: None,
            }]),
            response(vec![Block::Text(
                "File was written once; hook failed.".into(),
            )]),
        ]
        .into(),
    )));
    let tools = Arc::new(
        LocalTools::new(root.path().into(), authorizer(Mode::Auto))
            .unwrap()
            .with_workspace(workspace.clone()),
    );
    let agent = Agent::new(
        provider,
        tools,
        Limits {
            max_turns: 3,
            max_output_tokens: 256,
            budget: None,
            prices: None,
        },
    )
    .unwrap()
    .with_hooks(hooks(
        root.path(),
        &store,
        workspace,
        HookEvent::AfterTool,
        false,
        true,
    ));
    let mut session = Session::new(root.path().into(), "Openai".into(), "mock".into(), None);
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    assert!(
        agent
            .run_in(
                &mut session,
                Some("task".into()),
                &store,
                tx,
                CancellationToken::new()
            )
            .await
            .is_err()
    );
    drain.await.unwrap();
    assert_eq!(
        session.pending[0].state,
        cove::session::CallState::Finished
    );
    let mut loaded = store.load(&session.id).unwrap();
    loaded.hook_runs[0].state = HookState::Acknowledged;
    store.save(&loaded).unwrap();
    let (tx, mut rx) = mpsc::channel(64);
    let drain = tokio::spawn(async move { while rx.recv().await.is_some() {} });
    let report = agent
        .run_in(&mut loaded, None, &store, tx, CancellationToken::new())
        .await
        .unwrap();
    drain.await.unwrap();
    assert_eq!(report.tools_run, 0);
    assert_eq!(
        std::fs::read_to_string(root.path().join("file")).unwrap(),
        "once"
    );
}

#[test]
fn restarting_an_inflight_hook_never_claims_it_completed() {
    let root = tempfile::tempdir().unwrap();
    let data = tempfile::tempdir().unwrap();
    let store = Store::open(data.path()).unwrap();
    let mut session = Session::new(root.path().into(), "Openai".into(), "mock".into(), None);
    session.state = State::Running;
    session.hook_runs.push(cove::extensions::hooks::HookRun {
        id: cove::session::new_id(),
        hook: "uncertain".into(),
        event: HookEvent::TaskStart,
        state: HookState::Running,
        outcome: String::new(),
        tool_call: None,
        blocked: None,
    });
    store.save(&session).unwrap();
    let loaded = store.load(&session.id).unwrap();
    assert_eq!(loaded.hook_runs[0].state, HookState::Uncertain);
    assert!(matches!(loaded.state, State::Interrupted(_)));
}

#[tokio::test]
async fn cancelled_and_timed_out_mcp_operations_kill_owned_descendants() {
    for cancelled in [true, false] {
        let root = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let store = Store::open(data.path()).unwrap();
        let script = mcp_fixture(root.path());
        let mcp = mcp(
            root.path(),
            &store,
            python(),
            vec!["-u".into(), script.to_string_lossy().into()],
            Mode::Auto,
            2,
        );
        mcp.execute(call("echo"), CancellationToken::new())
            .await
            .unwrap();
        let cancel = CancellationToken::new();
        let signal = cancel.clone();
        let trigger = tokio::spawn(async move {
            if cancelled {
                tokio::time::sleep(std::time::Duration::from_millis(300)).await;
                signal.cancel();
            }
        });
        let start = std::time::Instant::now();
        assert!(mcp.execute(call("slow"), cancel).await.is_err());
        trigger.await.unwrap();
        assert!(start.elapsed() < std::time::Duration::from_secs(6));
        mcp.close(None, None).await.unwrap();
        tokio::time::sleep(std::time::Duration::from_secs(3)).await;
        assert!(!root.path().join("late-child").exists());
        assert_eq!(
            std::fs::read_to_string(root.path().join("mcp-calls.txt"))
                .unwrap()
                .lines()
                .collect::<Vec<_>>(),
            vec!["echo", "slow"]
        );
    }
}
