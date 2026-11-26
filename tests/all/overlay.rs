//! Overlay sub-tasks (docs/optimizations/0053): file tools through an overlay stack, and
//! the shell limits that come with it.

use std::{
    path::{Path, PathBuf},
    sync::Arc,
};

use cove::{
    safety::permission::{LocalAuthorizer, Mode},
    session::overlay::{Content, Intent, Overlay, OverlayStack, content_hash},
    tools::{LocalTools, OVERLAY_SHELL_REFUSED, ToolExecutor, ToolOutput, replay_edit},
};
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

fn tools(root: &Path) -> LocalTools {
    LocalTools::new(
        root.to_path_buf(),
        Arc::new(LocalAuthorizer {
            mode: Mode::Bypass,
            input: None,
        }),
    )
    .unwrap()
}

fn layer(dir: &Path) -> Arc<Overlay> {
    Arc::new(Overlay::open(dir.to_path_buf()).unwrap())
}

fn overlay_tools(root: &Path, layers: Vec<Arc<Overlay>>) -> (LocalTools, Arc<OverlayStack>) {
    let stack = Arc::new(OverlayStack::new(root, layers).unwrap());
    let tools = tools(root).with_overlay(stack.clone()).unwrap();
    (tools, stack)
}

async fn run(tools: &LocalTools, name: &str, input: Value) -> ToolOutput {
    tools
        .execute(name.into(), input, CancellationToken::new())
        .await
        .unwrap()
}

/// A workspace with an ignore file, a source tree and a protected-looking `.git`.
fn workspace() -> tempfile::TempDir {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    std::fs::create_dir_all(root.join("src")).unwrap();
    std::fs::create_dir_all(root.join(".git")).unwrap();
    std::fs::write(root.join(".gitignore"), "*.log\n").unwrap();
    std::fs::write(
        root.join("src/lib.rs"),
        "pub fn alpha() {}\npub fn beta() {}\n",
    )
    .unwrap();
    std::fs::write(root.join("src/old.rs"), "pub fn old_one() {}\n").unwrap();
    std::fs::write(root.join("crlf.txt"), "one\r\ntwo\r\n").unwrap();
    dir
}

#[tokio::test]
async fn overlay_writes_and_edits_stay_out_of_the_workspace_and_record_intents() {
    let dir = workspace();
    let root = dir.path();
    let data = tempfile::tempdir().unwrap();
    let (tools, stack) = overlay_tools(root, vec![layer(&data.path().join("t"))]);

    let out = run(
        &tools,
        "write",
        json!({"path":"src/new/mod.rs","content":"pub fn gamma() {}\n"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert!(
        out.content.starts_with("Created src/new/mod.rs"),
        "{}",
        out.content
    );
    assert!(!root.join("src/new").exists());

    // Replacing an unread file is refused as on disk.
    let out = run(&tools, "write", json!({"path":"src/lib.rs","content":"x"})).await;
    assert!(out.failed, "{}", out.content);

    let out = run(
        &tools,
        "edit",
        json!({"path":"crlf.txt","old":"two\n","new":"TWO\n"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    assert!(
        out.content.starts_with("Edited crlf.txt: 1 replacement(s)"),
        "{}",
        out.content
    );
    assert_eq!(
        std::fs::read(root.join("crlf.txt")).unwrap(),
        b"one\r\ntwo\r\n"
    );
    assert_eq!(
        stack.read(Path::new("crlf.txt")).unwrap(),
        b"one\r\nTWO\r\n"
    );

    // A second edit matches the layered content, not the disk.
    let out = run(
        &tools,
        "edit",
        json!({"path":"crlf.txt","old":"TWO","new":"three"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    let read = run(&tools, "read", json!({"path":"crlf.txt"})).await;
    assert!(read.content.contains("2: three"), "{}", read.content);
    assert!(read.content.contains("CRLF"), "{}", read.content);

    let read = run(&tools, "read", json!({"path":"src/new/mod.rs"})).await;
    assert!(
        read.content.contains("1: pub fn gamma() {}"),
        "{}",
        read.content
    );
    let listing = run(&tools, "read", json!({"path":"src"})).await;
    assert!(listing.content.contains("new/"), "{}", listing.content);
    let out = run(
        &tools,
        "write",
        json!({"path":"src/new/mod.rs","content":"pub fn delta() {}\n"}),
    )
    .await;
    assert!(out.content.starts_with("Overwrote"), "{}", out.content);

    let intents = stack.top().intents();
    assert_eq!(intents.len(), 4, "{intents:?}");
    assert!(matches!(
        &intents[0],
        Intent::Write {
            base_hash: None,
            ..
        }
    ));
    assert!(matches!(
        &intents[1],
        Intent::Edit { base_hash, old, .. } if *base_hash == content_hash(b"one\r\ntwo\r\n") && old == "two\n"
    ));
    assert_eq!(
        stack.changed_paths(),
        [
            PathBuf::from("crlf.txt"),
            PathBuf::from("src").join("new").join("mod.rs")
        ]
    );
    // Replaying the recorded edit on the base gives what the tool wrote.
    let Intent::Edit {
        old,
        new,
        replace_all,
        ..
    } = &intents[1]
    else {
        unreachable!()
    };
    assert_eq!(
        replay_edit(b"one\r\ntwo\r\n", "crlf.txt", old, new, *replace_all).unwrap(),
        b"one\r\nTWO\r\n"
    );
    assert!(replay_edit(b"none", "x", "absent", "y", false).is_err());
}

#[tokio::test]
async fn a_dependent_sub_task_reads_the_layer_below() {
    let dir = workspace();
    let root = dir.path();
    let data = tempfile::tempdir().unwrap();
    let (a, _) = overlay_tools(root, vec![layer(&data.path().join("a"))]);
    run(&a, "read", json!({"path":"src/lib.rs"})).await;
    let out = run(
        &a,
        "edit",
        json!({"path":"src/lib.rs","old":"beta","new":"beta_a"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    let (b, stack) = overlay_tools(
        root,
        vec![layer(&data.path().join("a")), layer(&data.path().join("b"))],
    );
    let read = run(&b, "read", json!({"path":"src/lib.rs"})).await;
    assert!(read.content.contains("beta_a"), "{}", read.content);
    run(
        &b,
        "edit",
        json!({"path":"src/lib.rs","old":"alpha","new":"alpha_b"}),
    )
    .await;
    assert_eq!(
        stack.read(Path::new("src/lib.rs")).unwrap(),
        b"pub fn alpha_b() {}\npub fn beta_a() {}\n"
    );
    assert!(stack.layers()[0].get(Path::new("src/lib.rs")).is_some());
    assert_eq!(
        stack.top().changed_paths(),
        [PathBuf::from("src").join("lib.rs")]
    );
}

#[tokio::test]
async fn glob_grep_and_repo_map_see_the_layers() {
    let dir = workspace();
    let root = dir.path();
    std::fs::write(root.join("debug.log"), "alpha\n").unwrap();
    let data = tempfile::tempdir().unwrap();
    let (tools, stack) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    run(
        &tools,
        "write",
        json!({"path":"src/fresh.rs","content":"pub fn zeta_fresh() { alpha() }\n"}),
    )
    .await;
    run(
        &tools,
        "write",
        json!({"path":"fresh.log","content":"alpha\n"}),
    )
    .await;
    run(&tools, "read", json!({"path":"src/lib.rs"})).await;
    run(
        &tools,
        "edit",
        json!({"path":"src/lib.rs","old":"pub fn alpha() {}","new":"pub fn alpha_renamed() {}"}),
    )
    .await;
    stack
        .delete(Path::new("src/old.rs"), b"pub fn old_one() {}\n")
        .unwrap();

    let glob = run(&tools, "glob", json!({"pattern":"*.rs"})).await;
    let mut listed: Vec<&str> = glob.content.lines().collect();
    listed.sort();
    assert_eq!(listed, ["src/fresh.rs", "src/lib.rs"], "{}", glob.content);
    // Layered files are subject to ignore rules too.
    let logs = run(
        &tools,
        "glob",
        json!({"pattern":"*.log","include_ignored":true}),
    )
    .await;
    let mut logs: Vec<&str> = logs.content.lines().collect();
    logs.sort();
    assert_eq!(logs, ["debug.log", "fresh.log"]);
    let logs = run(&tools, "glob", json!({"pattern":"*.log"})).await;
    assert!(
        logs.content.starts_with("No files found"),
        "{}",
        logs.content
    );

    for ripgrep in [false, true] {
        let tools = tools.clone().with_ripgrep(ripgrep);
        let grep = run(&tools, "grep", json!({"pattern":"alpha"})).await;
        let mut lines: Vec<&str> = grep.content.lines().collect();
        lines.sort();
        assert_eq!(
            lines,
            [
                "src/fresh.rs:1:pub fn zeta_fresh() { alpha() }",
                "src/lib.rs:1:pub fn alpha_renamed() {}"
            ],
            "ripgrep={ripgrep}: {}",
            grep.content
        );
        let grep = run(&tools, "grep", json!({"pattern":"old_one"})).await;
        assert!(
            grep.content.starts_with("No matches found"),
            "{}",
            grep.content
        );
        let grep = run(
            &tools,
            "grep",
            json!({"pattern":"fn","path":"src/fresh.rs"}),
        )
        .await;
        assert_eq!(
            grep.content,
            "src/fresh.rs:1:pub fn zeta_fresh() { alpha() }"
        );
        let grep = run(
            &tools,
            "grep",
            json!({"pattern":"beta","path":"src","files_only":true}),
        )
        .await;
        assert_eq!(grep.content, "src/lib.rs");
    }

    let map = run(
        &tools,
        "repo_map",
        json!({"query":"zeta_fresh alpha_renamed old_one"}),
    )
    .await;
    assert!(map.content.contains("zeta_fresh"), "{}", map.content);
    assert!(map.content.contains("alpha_renamed"), "{}", map.content);
    assert!(!map.content.contains("old_one"), "{}", map.content);

    // Normal tools over the same workspace still see only the disk.
    let disk = self::tools(root);
    let grep = run(&disk, "grep", json!({"pattern":"alpha"})).await;
    assert_eq!(grep.content, "src/lib.rs:1:pub fn alpha() {}");
    let glob = run(
        &disk,
        "glob",
        json!({"pattern":"fresh.*","include_ignored":true}),
    )
    .await;
    assert!(
        glob.content.starts_with("No files found"),
        "{}",
        glob.content
    );
}

#[tokio::test]
async fn overlay_tools_keep_the_workspace_boundary_and_protected_paths() {
    let dir = workspace();
    let root = dir.path();
    let data = tempfile::tempdir().unwrap();
    let (tools, stack) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    for (name, input) in [
        ("write", json!({"path":"../outside.txt","content":"x"})),
        ("write", json!({"path":".git/config","content":"x"})),
        ("read", json!({"path":"../x"})),
        ("edit", json!({"path":"missing.rs","old":"a","new":"b"})),
    ] {
        let out = run(&tools, name, input.clone()).await;
        assert!(out.failed, "{name} {input}: {}", out.content);
    }
    cove::safety::protected::protect(&root.join(".cove"));
    let out = run(
        &tools,
        "write",
        json!({"path":".cove/config.json","content":"x"}),
    )
    .await;
    assert!(
        out.failed && out.content.contains("data directory"),
        "{}",
        out.content
    );
    assert!(stack.top().is_empty());
    // Another workspace's stack is refused.
    let other = tempfile::tempdir().unwrap();
    let foreign =
        Arc::new(OverlayStack::new(other.path(), vec![layer(&data.path().join("o"))]).unwrap());
    assert!(self::tools(root).with_overlay(foreign).is_err());
    assert_eq!(stack.lookup(Path::new("src/lib.rs")), None::<Content>);
}

#[tokio::test]
async fn overlay_shell_runs_only_read_only_commands_without_background_jobs() {
    let dir = workspace();
    let root = dir.path();
    let data = tempfile::tempdir().unwrap();
    let (tools, _) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    for command in [
        "echo hi > made.txt",
        "cargo fmt",
        "cargo build",
        "rm src/lib.rs",
        "touch x.txt",
        "npm install",
    ] {
        let out = run(&tools, "shell", json!({"command":command})).await;
        assert!(out.failed, "{command}: {}", out.content);
        assert!(
            out.content.starts_with(OVERLAY_SHELL_REFUSED),
            "{command}: {}",
            out.content
        );
    }
    assert!(!root.join("made.txt").exists());
    let out = run(
        &tools,
        "shell",
        json!({"command":"git --version","run_in_background":true}),
    )
    .await;
    assert!(
        out.failed && out.content.starts_with(OVERLAY_SHELL_REFUSED),
        "{}",
        out.content
    );
    let out = run(&tools, "shell_task", json!({"action":"list"})).await;
    assert!(out.failed, "{}", out.content);

    let out = run(&tools, "shell", json!({"command":"git --version"})).await;
    assert!(!out.failed, "{}", out.content);
    assert!(out.content.contains("git version"), "{}", out.content);
    assert!(
        out.content
            .ends_with("use read, grep or glob to see your own changes]"),
        "{}",
        out.content
    );

    let names: Vec<String> = tools.definitions().into_iter().map(|d| d.name).collect();
    assert!(!names.contains(&"shell_task".to_owned()));
    let shell = tools
        .definitions()
        .into_iter()
        .find(|d| d.name == "shell")
        .unwrap();
    assert!(
        shell.schema["properties"]
            .get("run_in_background")
            .is_none()
    );
    // Normal tools keep both.
    let normal = self::tools(root);
    assert!(normal.definitions().iter().any(|d| d.name == "shell_task"));
}

#[tokio::test]
async fn ripgrep_excludes_exactly_the_changed_paths() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    std::fs::create_dir(root.join("sub")).unwrap();
    for name in ["a.rs", "sub/a.rs", "b[1].rs", "b1.rs"] {
        std::fs::write(root.join(name), "needle disk\n").unwrap();
    }
    let data = tempfile::tempdir().unwrap();
    let (tools, _) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    for name in ["a.rs", "b[1].rs"] {
        run(&tools, "read", json!({"path":name})).await;
        let out = run(
            &tools,
            "edit",
            json!({"path":name,"old":"disk","new":"layer"}),
        )
        .await;
        assert!(!out.failed, "{}", out.content);
    }
    for ripgrep in [false, true] {
        let tools = tools.clone().with_ripgrep(ripgrep);
        let grep = run(&tools, "grep", json!({"pattern":"needle"})).await;
        let mut lines: Vec<&str> = grep.content.lines().collect();
        lines.sort();
        assert_eq!(
            lines,
            [
                "a.rs:1:needle layer",
                "b1.rs:1:needle disk",
                "b[1].rs:1:needle layer",
                "sub/a.rs:1:needle disk"
            ],
            "ripgrep={ripgrep}"
        );
    }
}

#[tokio::test]
async fn grep_include_does_not_bring_back_changed_or_deleted_disk_files() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    std::fs::create_dir(root.join("sub")).unwrap();
    for (name, content) in [
        ("a.rs", "needle disk\n"),
        ("gone.rs", "needle gone\n"),
        ("sub/b.rs", "needle sub\n"),
        ("sub/gone.rs", "needle sub gone\n"),
        ("c.toml", "needle toml\n"),
    ] {
        std::fs::write(root.join(name), content).unwrap();
    }
    let data = tempfile::tempdir().unwrap();
    let (tools, stack) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    run(&tools, "read", json!({"path":"a.rs"})).await;
    let out = run(
        &tools,
        "edit",
        json!({"path":"a.rs","old":"disk","new":"layer"}),
    )
    .await;
    assert!(!out.failed, "{}", out.content);
    stack
        .delete(Path::new("gone.rs"), b"needle gone\n")
        .unwrap();
    stack
        .delete(Path::new("sub/gone.rs"), b"needle sub gone\n")
        .unwrap();
    let cases: [(Value, &[&str]); 5] = [
        (
            json!({"pattern":"needle","include":"*.rs"}),
            &["a.rs:1:needle layer", "sub/b.rs:1:needle sub"],
        ),
        (
            json!({"pattern":"needle","include":"{*.rs,*.toml}"}),
            &[
                "a.rs:1:needle layer",
                "c.toml:1:needle toml",
                "sub/b.rs:1:needle sub",
            ],
        ),
        (
            json!({"pattern":"needle","include":"*.rs","path":"sub"}),
            &["sub/b.rs:1:needle sub"],
        ),
        (
            json!({"pattern":"needle","include":"a.rs"}),
            &["a.rs:1:needle layer"],
        ),
        (
            json!({"pattern":"needle","include":"gone.rs","files_only":true}),
            &[],
        ),
    ];
    for ripgrep in [false, true] {
        let tools = tools.clone().with_ripgrep(ripgrep);
        for (input, expected) in &cases {
            let grep = run(&tools, "grep", input.clone()).await;
            let mut lines: Vec<&str> = grep.content.lines().collect();
            lines.sort();
            if expected.is_empty() {
                assert!(
                    grep.content.starts_with("No matches found"),
                    "ripgrep={ripgrep} {input}: {}",
                    grep.content
                );
            } else {
                assert_eq!(lines, *expected, "ripgrep={ripgrep} {input}");
            }
        }
    }
}

#[tokio::test]
async fn grep_finds_files_in_directories_only_the_layers_hold() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    std::fs::write(root.join("a.rs"), "needle disk\n").unwrap();
    let data = tempfile::tempdir().unwrap();
    let (tools, stack) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    stack
        .write(Path::new("newdir/x.rs"), None, b"needle new\n", None)
        .unwrap();
    let mut queries = vec![
        json!({"pattern":"needle","path":"newdir"}),
        json!({"pattern":"needle","path":"newdir","include":"*.rs"}),
        json!({"pattern":"needle","path":"newdir/x.rs"}),
    ];
    // Windows paths ignore case: the layer's file is found under another spelling too.
    if cfg!(windows) {
        queries.push(json!({"pattern":"needle","path":"NEWDIR"}));
        queries.push(json!({"pattern":"needle","path":"NewDir/X.RS"}));
        queries.push(json!({"pattern":"needle","path":"NEWDIR","include":"*.rs"}));
    }
    for ripgrep in [false, true] {
        let tools = tools.clone().with_ripgrep(ripgrep);
        for input in &queries {
            let grep = run(&tools, "grep", input.clone()).await;
            assert!(!grep.failed, "ripgrep={ripgrep} {input}: {}", grep.content);
            assert_eq!(
                grep.content, "newdir/x.rs:1:needle new",
                "ripgrep={ripgrep} {input}"
            );
        }
    }
}

#[tokio::test]
async fn repo_map_keeps_the_layered_files_when_the_scan_is_truncated() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    std::fs::create_dir(root.join("many")).unwrap();
    for index in 0..5001 {
        std::fs::write(
            root.join("many").join(format!("f{index:04}.rs")),
            format!("pub fn disk_{index}() {{}}\n"),
        )
        .unwrap();
    }
    let data = tempfile::tempdir().unwrap();
    let (tools, stack) = overlay_tools(root, vec![layer(&data.path().join("t"))]);
    stack
        .write(
            Path::new("zz/layered.rs"),
            None,
            b"pub fn layered_only_symbol() {}\n",
            None,
        )
        .unwrap();
    let map = run(&tools, "repo_map", json!({"query":"layered_only_symbol"})).await;
    assert!(
        map.content.contains("layered_only_symbol"),
        "{}",
        map.content
    );
    assert!(map.content.contains("scan stopped"), "{}", map.content);
}
