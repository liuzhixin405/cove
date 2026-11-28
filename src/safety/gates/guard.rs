//! Guardrail (repeated failures) and loop detection, as one `TurnHook`.
//!
//! Guardrail: a tool failing 3 times within 30 s warns, 5 blocks; the same call failing
//! warns at 2 and blocks at 5; the same tool failing warns at 3 and blocks at 8; an
//! idempotent read repeating an identical result warns at 2 and blocks at 5.
//! Loop detector: the same mutating call 10 times in the last 14, the same tool with
//! changing arguments 10 times in the last 12 (unless its outputs or directories vary),
//! the same output 8 times in the last 40. Each detection refuses the call (or flags
//! the output) with guidance. Whether the task should stop is the engine's progress
//! check (`engine::progress`), which counts these refusals as rounds without progress.

use std::{
    collections::{HashMap, HashSet, VecDeque},
    hash::{Hash, Hasher},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use futures_util::future::BoxFuture;
use serde_json::Value;

use crate::{
    engine::turn::TurnHook, safety::permission::safety, session::Session, tools::ToolOutput,
};

const RAPID_WINDOW: Duration = Duration::from_secs(30);
const IDEMPOTENT: &[&str] = &["read", "grep", "glob", "repo_map", "web_fetch"];
/// Exempt from loop detection: reading the same file again is not a loop.
const LOOP_EXEMPT: &[&str] = &[
    "read",
    "glob",
    "grep",
    "repo_map",
    "web_fetch",
    "web_search",
    "skill",
    "todo",
    "ask_user",
    "memory",
];

const CALL_WINDOW: usize = 14;
const CALL_THRESHOLD: usize = 10;
const PATTERN_WINDOW: usize = 12;
const PATTERN_THRESHOLD: usize = 10;
const OUTPUT_WINDOW: usize = 40;
const OUTPUT_THRESHOLD: u32 = 8;

fn hash(value: impl Hash) -> u64 {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    value.hash(&mut hasher);
    hasher.finish()
}

fn signature(name: &str, input: &Value) -> (String, u64) {
    (name.to_owned(), hash((name, input.to_string())))
}

fn failure_group<'a>(name: &'a str, input: &Value) -> &'a str {
    if name == "memory"
        && !matches!(
            input.get("scope").and_then(Value::as_str),
            Some("project" | "global")
        )
    {
        "memory (invalid scope)"
    } else {
        name
    }
}

/// Tool plus the start of its main argument: `shell:cargo test --`, `edit:src/main.rs`.
fn pattern(name: &str, input: &Value) -> String {
    let main = ["command", "path", "pattern", "query", "url", "prompt"]
        .iter()
        .find_map(|key| input.get(key).and_then(Value::as_str))
        .unwrap_or_default();
    format!("{name}:{}", main.chars().take(20).collect::<String>())
}

fn directory(input: &Value) -> Option<String> {
    let value = input
        .get("path")
        .or_else(|| input.get("command"))
        .and_then(Value::as_str)?;
    let value = value.trim();
    if let Some(rest) = value
        .strip_prefix("cd ")
        .or_else(|| value.strip_prefix("Set-Location "))
    {
        return Some(
            rest.split([';', '&', '|'])
                .next()
                .unwrap_or(rest)
                .trim()
                .to_owned(),
        );
    }
    let path = std::path::Path::new(value.split_whitespace().next().unwrap_or(value));
    path.parent()
        .map(|p| p.to_string_lossy().into_owned())
        .filter(|p| !p.is_empty())
}

#[derive(Default)]
struct State {
    recent_failures: VecDeque<(Instant, String)>,
    exact_failures: HashMap<(String, u64), u32>,
    tool_failures: HashMap<String, u32>,
    idempotent_result: HashMap<(String, u64), u64>,
    idempotent_repeats: HashMap<(String, u64), u32>,
    /// Guardrail warnings for calls about to run, appended to their results.
    warnings: HashMap<(String, u64), String>,
    calls: VecDeque<u64>,
    patterns: VecDeque<String>,
    pattern_outputs: HashMap<String, VecDeque<u64>>,
    pattern_dirs: HashMap<String, HashSet<String>>,
    last_pattern: Option<String>,
    outputs: VecDeque<u64>,
    output_counts: HashMap<u64, u32>,
    /// File-tool hints already given this task.
    hinted: HashSet<&'static str>,
}

#[derive(Default)]
pub struct ToolGuard {
    state: Mutex<State>,
}

/// The guidance of a detected loop.
fn detected(reason: &str) -> String {
    format!(
        "[loop detection] {reason}. This call was not run. Change approach: re-read the error, inspect the relevant code, or explain what blocks you instead of repeating the same step."
    )
}

impl ToolGuard {
    fn guardrail(
        state: &mut State,
        name: &str,
        sig: &(String, u64),
    ) -> Result<Option<String>, String> {
        let now = Instant::now();
        state
            .recent_failures
            .retain(|(at, _)| now.duration_since(*at) < RAPID_WINDOW);
        let rapid = state
            .recent_failures
            .iter()
            .filter(|(_, tool)| tool == name)
            .count();
        if rapid >= 5 {
            return Err(format!(
                "[guardrail] {name} failed {rapid} times within 30 s; the call is blocked. Stop and find the root cause."
            ));
        }
        let exact = state.exact_failures.get(sig).copied().unwrap_or(0);
        if exact >= 5 {
            return Err("[guardrail] this exact call already failed 5 times; it is blocked. Use a different approach.".into());
        }
        let tool = state.tool_failures.get(name).copied().unwrap_or(0);
        if tool >= 8 {
            return Err(format!(
                "[guardrail] {name} failed 8 times in a row; it is blocked. Try another tool or method."
            ));
        }
        let repeats = state.idempotent_repeats.get(sig).copied().unwrap_or(0);
        if IDEMPOTENT.contains(&name) && repeats >= 5 {
            return Err("[guardrail] this read returned the same result 5 times; it is blocked. Use what you already have.".into());
        }
        Ok(if rapid >= 3 {
            Some(format!(
                "[guardrail] {name} failed {rapid} times within 30 s (blocks at 5); change approach."
            ))
        } else if exact >= 2 {
            Some("[guardrail] this exact call has failed repeatedly; consider another way.".into())
        } else if tool >= 3 {
            Some(format!(
                "[guardrail] {name} keeps failing; check whether you need to adjust."
            ))
        } else if IDEMPOTENT.contains(&name) && repeats >= 2 {
            Some(
                "[guardrail] this call keeps returning the same result; repeating it is pointless."
                    .into(),
            )
        } else {
            None
        })
    }
}

/// A hint when a shell command does what a file tool does better: writing or
/// editing a workspace file, or reading a line range. Temporary files are fine.
fn file_tool_hint(command: &str) -> Option<(&'static str, &'static str)> {
    static PATTERNS: std::sync::OnceLock<[regex::Regex; 4]> = std::sync::OnceLock::new();
    let [in_place, redirect, tee, range] = PATTERNS.get_or_init(|| {
        [
            regex::Regex::new(r"\bsed\s+(?:-[a-zA-Z]*i|--in-place)").unwrap(),
            regex::Regex::new(r"\bcat\b[^|;&\n]*?>{1,2}\s*([^\s;&|<>]+)").unwrap(),
            regex::Regex::new(r"\btee\s+(?:-a\s+)?([^\s;&|<>-][^\s;&|<>]*)").unwrap(),
            regex::Regex::new(r#"\bsed\s+-n\s+['"]?\d+(?:,\d+)?p"#).unwrap(),
        ]
    });
    let temporary = |target: &str| {
        let target = target
            .trim_matches(['"', '\''])
            .replace('\\', "/")
            .to_lowercase();
        [
            "/tmp",
            "$tmp",
            "${tmp",
            "$temp",
            "${temp",
            "$env:temp",
            "/dev/",
        ]
        .iter()
        .any(|prefix| target.starts_with(prefix))
            || target.contains("appdata/local/temp/")
    };
    if in_place.is_match(command) {
        return Some((
            "edit",
            "[hint] Change workspace files with the edit tool rather than sed -i: it checks the file's current content, keeps encoding and line endings, and needs no shell approval.",
        ));
    }
    let writes = redirect
        .captures_iter(command)
        .chain(tee.captures_iter(command))
        .any(|captures| !temporary(&captures[1]));
    if writes {
        return Some((
            "write",
            "[hint] Create or rewrite workspace files with the write tool rather than cat >, heredocs or tee: it checks the file's current content, keeps encoding and line endings, and needs no shell approval.",
        ));
    }
    if range.is_match(command) {
        return Some((
            "read",
            "[hint] Read a line range with the read tool (offset and limit) rather than sed -n: it needs no shell approval and runs in parallel with other reads.",
        ));
    }
    None
}

/// Rules for rare kinds of shell work, given after the first such command instead
/// of in every request's system prompt.
fn scenario_hint(command: &str) -> Option<(&'static str, &'static str)> {
    static PATTERNS: std::sync::OnceLock<[regex::Regex; 2]> = std::sync::OnceLock::new();
    let [download, piped_build] = PATTERNS.get_or_init(|| {
        [
            regex::Regex::new(
                r"(?i)\b(?:huggingface-cli\s+download|hf\s+download|modelscope\s+download|ollama\s+pull|git\s+lfs\s+(?:pull|fetch))\b|\b(?:curl|wget|aria2c)\b[^|;&\n]*\.(?:gguf|safetensors|onnx|ckpt|pth|pt)\b",
            )
            .unwrap(),
            regex::Regex::new(
                r"\b(?:cargo\s+(?:build|test|check|clippy|nextest)|go\s+(?:build|test)|dotnet\s+(?:build|test)|(?:npm|pnpm|yarn)\s+(?:run\s+)?(?:build|test)|pytest|mvn|gradlew?|make|ctest)\b[^\n]*\|\s*(?:tail|head)\b",
            )
            .unwrap(),
        ]
    });
    if download.is_match(command) {
        return Some((
            "download",
            "[hint] For a large model download: identify the exact repository, file, revision and advertised size once, and check disk space and any partial file before starting. \
             Use one resumable transfer to a temporary file, avoid parallel or repeated retries while it is running, and verify size/hash before renaming it into place. \
             After repeated TLS/network failures, stop and report the evidence instead of cycling download commands.",
        ));
    }
    if piped_build.is_match(command) {
        return Some((
            "piped_build",
            "[hint] For long-running build/test commands, do not pipe output through tail/head; show live output so progress remains visible.",
        ));
    }
    None
}

impl TurnHook for ToolGuard {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = State::default();
        }
        Box::pin(async { None })
    }

    fn before_tool(&self, name: &str, input: &Value) -> Option<String> {
        let mut state = self.state.lock().ok()?;
        let sig = signature(name, input);
        match Self::guardrail(&mut state, failure_group(name, input), &sig) {
            Err(block) => return Some(block),
            Ok(Some(warning)) => {
                state.warnings.insert(sig.clone(), warning);
            }
            Ok(None) => {}
        }
        if LOOP_EXEMPT.contains(&name) {
            return None;
        }
        state.calls.push_back(sig.1);
        if state.calls.len() > CALL_WINDOW {
            state.calls.pop_front();
        }
        let same = state.calls.iter().filter(|call| **call == sig.1).count();
        if same >= CALL_THRESHOLD {
            return Some(detected(&format!(
                "the same {name} call ran {same} of the last {CALL_WINDOW} times"
            )));
        }
        let pattern = pattern(name, input);
        if let Some(dir) = directory(input) {
            let dirs = state.pattern_dirs.entry(pattern.clone()).or_default();
            dirs.insert(dir);
            if dirs.len() >= 3 {
                // Working through different directories is progress.
                state.patterns.retain(|p| *p != pattern);
            }
        }
        state.patterns.push_back(pattern.clone());
        if state.patterns.len() > PATTERN_WINDOW {
            state.patterns.pop_front();
        }
        state.last_pattern = Some(pattern.clone());
        let similar = state.patterns.iter().filter(|p| **p == pattern).count();
        if similar >= PATTERN_THRESHOLD {
            return Some(detected(&format!(
                "{pattern}… ran {similar} of the last {PATTERN_WINDOW} calls with changing arguments but no progress"
            )));
        }
        None
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        let mut state = self.state.lock().ok()?;
        let sig = signature(name, input);
        let group = failure_group(name, input);
        let mut notes: Vec<String> = state.warnings.remove(&sig).into_iter().collect();
        if output.failed {
            state
                .recent_failures
                .push_back((Instant::now(), group.to_owned()));
            if state.recent_failures.len() > 100 {
                state.recent_failures.pop_front();
            }
            *state.exact_failures.entry(sig.clone()).or_default() += 1;
            *state.tool_failures.entry(group.to_owned()).or_default() += 1;
        } else {
            state.recent_failures.retain(|(_, tool)| tool != group);
            state.exact_failures.remove(&sig);
            state.tool_failures.insert(group.to_owned(), 0);
            if IDEMPOTENT.contains(&name) {
                let result = hash(&output.content);
                if state.idempotent_result.get(&sig) == Some(&result) {
                    *state.idempotent_repeats.entry(sig.clone()).or_default() += 1;
                } else {
                    state.idempotent_result.insert(sig.clone(), result);
                    state.idempotent_repeats.insert(sig.clone(), 0);
                }
            }
        }
        if name == "shell"
            && let Some(command) = input.get("command").and_then(Value::as_str)
        {
            for warning in safety::risky(command) {
                notes.push(format!("[safety] warning: {warning}"));
            }
            for (kind, hint) in file_tool_hint(command)
                .into_iter()
                .chain(scenario_hint(command))
            {
                if state.hinted.insert(kind) {
                    notes.push(hint.to_owned());
                }
            }
        }
        if !LOOP_EXEMPT.contains(&name) && !output.content.trim().is_empty() {
            let digest = hash(
                output
                    .content
                    .get(..512.min(output.content.len()))
                    .unwrap_or(&output.content),
            );
            if let Some(pattern) = state.last_pattern.clone() {
                let outputs = state.pattern_outputs.entry(pattern.clone()).or_default();
                outputs.push_back(digest);
                if outputs.len() > PATTERN_WINDOW {
                    outputs.pop_front();
                }
                if outputs.iter().collect::<HashSet<_>>().len() >= 4 {
                    // Outputs keep changing: the attempts are exploring, not looping.
                    state.patterns.retain(|p| *p != pattern);
                }
            }
            state.outputs.push_back(digest);
            *state.output_counts.entry(digest).or_default() += 1;
            if state.outputs.len() > OUTPUT_WINDOW
                && let Some(old) = state.outputs.pop_front()
                && let Some(count) = state.output_counts.get_mut(&old)
            {
                *count -= 1;
            }
            let count = state.output_counts.get(&digest).copied().unwrap_or(0);
            if count >= OUTPUT_THRESHOLD {
                state.output_counts.insert(digest, 0);
                notes.push(detected(&format!(
                    "the same tool output appeared {count} times in the last {OUTPUT_WINDOW} results"
                )).replace("This call was not run. ", ""));
            }
        }
        (!notes.is_empty()).then(|| notes.join("\n"))
    }
}

/// Hands hard denials (catastrophic commands, deny rules) to the model as refused
/// results it can adapt to, instead of approval errors that stop the task. Anything
/// that needs asking still goes to the authorizer when the tool runs.
pub struct PolicyGuard {
    pub permissions: Arc<crate::safety::permission::Permissions>,
    pub root: std::path::PathBuf,
}

impl TurnHook for PolicyGuard {
    fn before_tool(&self, name: &str, input: &Value) -> Option<String> {
        use crate::safety::permission::{Decision, Effect};
        let effect = match name {
            "shell" => Effect::Shell(input.get("command")?.as_str()?.to_owned()),
            "write" | "edit" => Effect::Write(self.root.join(input.get("path")?.as_str()?)),
            _ => return None,
        };
        match self.permissions.decide(&effect) {
            Decision::Deny(reason)
                if self.permissions.mode() != crate::safety::permission::Mode::Plan
                    || reason.starts_with("blocked")
                    || reason.contains("policy") =>
            {
                Some(format!(
                    "[permission] {} was refused: {reason}. Do not retry it; choose a safer approach or ask the user.",
                    effect.describe()
                ))
            }
            _ => None,
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_file_operations_get_a_file_tool_hint() {
        let kind = |command: &str| file_tool_hint(command).map(|(kind, _)| kind);
        assert_eq!(kind("sed -i 's/a/b/' src/main.rs"), Some("edit"));
        assert_eq!(kind("sed -ri 's/a/b/' x"), Some("edit"));
        assert_eq!(
            kind("cat > src/Program.cs <<'EOF'\nclass A {}\nEOF"),
            Some("write")
        );
        assert_eq!(kind("cat <<'EOF' >> notes.md\nx\nEOF"), Some("write"));
        assert_eq!(kind("echo x | tee README.md"), Some("write"));
        assert_eq!(kind("sed -n '10,40p' src/lib.rs"), Some("read"));
        // Temporary scripts, plain reads and unrelated commands get none.
        assert_eq!(kind("cat > /tmp/probe.py <<'EOF'\nprint(1)\nEOF"), None);
        assert_eq!(kind("cat > \"$TEMP/x.ps1\""), None);
        assert_eq!(kind("cat src/lib.rs 2>&1"), None);
        assert_eq!(kind("cargo test 2>&1 | tee /dev/null"), None);
        assert_eq!(kind("git status"), None);
        // Workspace paths that merely contain "tmp" or "dev" are still workspace files.
        assert_eq!(
            kind("cat > src/tmpl/page.html <<'EOF'\nx\nEOF"),
            Some("write")
        );
        assert_eq!(kind("tee web/dev/app.js"), Some("write"));
    }

    #[test]
    fn rare_shell_work_gets_its_rules_once_per_task() {
        let kind = |command: &str| scenario_hint(command).map(|(kind, _)| kind);
        assert_eq!(
            kind("huggingface-cli download Qwen/Qwen3-8B-GGUF qwen3-8b-q4_k_m.gguf"),
            Some("download")
        );
        assert_eq!(
            kind("hf download org/model --local-dir models"),
            Some("download")
        );
        assert_eq!(kind("ollama pull llama3"), Some("download"));
        assert_eq!(
            kind("curl -L -C - -o /tmp/m.part https://hf.co/x/resolve/main/model.safetensors"),
            Some("download")
        );
        assert_eq!(
            kind("cargo test --release 2>&1 | tail -30"),
            Some("piped_build")
        );
        assert_eq!(kind("npm run build | head -50"), Some("piped_build"));
        assert_eq!(kind("python -m pytest -q | tail -5"), Some("piped_build"));
        // Ordinary downloads, unpiped builds and filtered non-build output get none.
        assert_eq!(
            kind("curl -sSL https://example.com/install.sh -o install.sh"),
            None
        );
        assert_eq!(kind("pip download requests"), None);
        assert_eq!(kind("cargo test --release"), None);
        assert_eq!(kind("git log --oneline | head -5"), None);
        assert_eq!(kind("ls | tail"), None);

        let guard = ToolGuard::default();
        let input = json!({"command":"cargo build 2>&1 | tail -20"});
        let note = guard.tool_note("shell", &input, &output("Finished", false));
        assert!(note.is_some_and(|note| note.contains("live output")));
        let again = json!({"command":"cargo test 2>&1 | tail -40"});
        let note = guard.tool_note("shell", &again, &output("ok", false));
        assert!(
            !note.unwrap_or_default().contains("live output"),
            "once per task"
        );
    }
    use serde_json::json;

    fn output(content: &str, failed: bool) -> ToolOutput {
        ToolOutput {
            content: content.into(),
            failed,
            ..Default::default()
        }
    }

    #[test]
    fn repeated_failures_warn_then_block() {
        let guard = ToolGuard::default();
        let input = json!({"command":"make"});
        for _ in 0..2 {
            assert!(guard.before_tool("shell", &input).is_none());
            guard.tool_note("shell", &input, &output("boom", true));
        }
        assert!(guard.before_tool("shell", &input).is_none());
        let note = guard
            .tool_note("shell", &input, &output("boom", true))
            .unwrap();
        assert!(note.contains("[guardrail]"), "{note}");
        for _ in 0..2 {
            guard.before_tool("shell", &input);
            guard.tool_note("shell", &input, &output("boom", true));
        }
        assert!(
            guard
                .before_tool("shell", &input)
                .unwrap()
                .contains("blocked")
        );
    }

    #[test]
    fn corrected_memory_scope_is_not_blocked_by_missing_scope_failures() {
        let guard = ToolGuard::default();
        for index in 0..5 {
            let input = json!({"action":"read", "id":format!("entry-{index}")});
            assert!(guard.before_tool("memory", &input).is_none());
            guard.tool_note("memory", &input, &output("missing field `scope`", true));
        }
        let invalid = json!({"action":"read", "id":"another"});
        assert!(
            guard
                .before_tool("memory", &invalid)
                .unwrap()
                .contains("blocked")
        );
        let corrected = json!({"action":"read", "id":"entry-0", "scope":"project"});
        assert!(guard.before_tool("memory", &corrected).is_none());
        guard.tool_note("memory", &corrected, &output("saved note", false));
        assert!(
            guard
                .before_tool("memory", &invalid)
                .unwrap()
                .contains("blocked")
        );
        for index in 0..5 {
            let input =
                json!({"action":"read", "id":format!("missing-{index}"), "scope":"project"});
            assert!(guard.before_tool("memory", &input).is_none());
            guard.tool_note("memory", &input, &output("not found", true));
        }
        assert!(
            guard
                .before_tool("memory", &corrected)
                .unwrap()
                .contains("blocked")
        );
    }

    #[test]
    fn identical_reads_are_blocked_after_five_repeats() {
        let guard = ToolGuard::default();
        let input = json!({"path":"a.txt"});
        for _ in 0..6 {
            assert!(guard.before_tool("read", &input).is_none());
            guard.tool_note("read", &input, &output("same", false));
        }
        assert!(
            guard
                .before_tool("read", &input)
                .unwrap()
                .contains("blocked")
        );
    }

    #[test]
    fn loops_are_refused_and_other_calls_still_run() {
        // Stopping the task is the engine's progress check; the guard only refuses.
        let guard = ToolGuard::default();
        let input = json!({"path":"a.txt","old":"x","new":"y"});
        let mut refused = 0;
        for _ in 0..30 {
            match guard.before_tool("edit", &input) {
                Some(reason) => {
                    assert!(reason.contains("[loop detection]"), "{reason}");
                    refused += 1;
                }
                None => {
                    guard.tool_note("edit", &input, &output("edited a.txt", false));
                }
            }
        }
        assert!(refused >= 20, "{refused}");
        assert!(
            guard
                .before_tool("shell", &json!({"command":"ls"}))
                .is_none()
        );
    }

    #[test]
    fn changing_outputs_are_not_a_loop() {
        let guard = ToolGuard::default();
        for index in 0..40 {
            let input = json!({"command":format!("cargo test case_{index}")});
            assert!(guard.before_tool("shell", &input).is_none(), "{index}");
            guard.tool_note("shell", &input, &output(&format!("result {index}"), false));
        }
    }

    #[test]
    fn hard_denials_reach_the_model() {
        let dir = tempfile::tempdir().unwrap();
        let permissions = Arc::new(
            crate::safety::permission::Permissions::new(crate::safety::permission::Mode::Bypass, dir.path().into(), vec![], None).with_rules(
                serde_json::from_value(json!([{"tool_pattern":"write","action":"deny","param_match":{"path":"*.lock"}}])).unwrap(),
            ),
        );
        let guard = PolicyGuard {
            permissions,
            root: dir.path().into(),
        };
        assert!(
            guard
                .before_tool("shell", &json!({"command":"rm -rf /"}))
                .unwrap()
                .contains("blocked")
        );
        assert!(
            guard
                .before_tool("write", &json!({"path":"Cargo.lock","content":""}))
                .unwrap()
                .contains("policy")
        );
        assert!(
            guard
                .before_tool("write", &json!({"path":"a.rs","content":""}))
                .is_none()
        );
        assert!(
            guard
                .before_tool("shell", &json!({"command":"anything"}))
                .is_none()
        );
    }

    #[test]
    fn risky_shell_commands_get_a_note() {
        let guard = ToolGuard::default();
        let input = json!({"command":"git push --force"});
        guard.before_tool("shell", &input);
        assert!(
            guard
                .tool_note("shell", &input, &output("ok", false))
                .unwrap()
                .contains("[safety]")
        );
    }
}
