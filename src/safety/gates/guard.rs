//! Guardrail (repeated failures) and loop detection, as one `TurnHook`.
//!
//! Guardrail: a tool failing 3 times within 30 s warns, 5 blocks; the same call failing
//! warns at 2 and blocks at 5; the same tool failing warns at 3 and blocks at 8; an
//! idempotent read repeating an identical result warns at 2 and blocks at 5.
//! Loop detector: the same mutating call 10 times in the last 14, the same tool with
//! changing arguments 10 times in the last 12 (unless its outputs or directories vary),
//! the same output 8 times in the last 40, and 60 calls without file activity. Each
//! detection refuses the call with guidance; the second asks the user (interactive),
//! the fifth stops tool use so the model wraps up.

use std::{
    collections::{HashMap, HashSet, VecDeque},
    hash::{Hash, Hasher},
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use futures_util::future::BoxFuture;
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::UserInput, engine::turn::TurnHook, safety::permission::safety, session::Session,
    tools::ToolOutput,
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
const STALL_THRESHOLD: u32 = 60;
const MAX_HITS: u32 = 5;
const ASK_AT_HIT: u32 = 2;

fn hash(value: impl Hash) -> u64 {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    value.hash(&mut hasher);
    hasher.finish()
}

fn signature(name: &str, input: &Value) -> (String, u64) {
    (name.to_owned(), hash((name, input.to_string())))
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
        .map(|p| p.display().to_string())
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
    since_activity: u32,
    hits: u32,
    stopped: bool,
}

impl State {
    fn clear_loops(&mut self) {
        self.calls.clear();
        self.patterns.clear();
        self.pattern_outputs.clear();
        self.pattern_dirs.clear();
        self.outputs.clear();
        self.output_counts.clear();
        self.since_activity = 0;
    }
}

pub struct ToolGuard {
    state: Mutex<State>,
    input: Option<Arc<dyn UserInput>>,
}

const STOPPED: &str = "[loop detection] Tool use is stopped for this task after repeated loops. Do not call more tools: summarize what is done, what failed, what was verified and what remains.";

impl ToolGuard {
    pub fn new(input: Option<Arc<dyn UserInput>>) -> Self {
        Self {
            state: Mutex::new(State::default()),
            input,
        }
    }

    /// Asks the user whether to continue; only possible on a multi-threaded runtime.
    fn ask(&self, reason: &str) -> Option<bool> {
        let input = self.input.clone()?;
        let handle = tokio::runtime::Handle::try_current().ok()?;
        if handle.runtime_flavor() != tokio::runtime::RuntimeFlavor::MultiThread {
            return None;
        }
        let question = format!("[循环检测] {reason}\n让 agent 继续吗？[y/N] ");
        let answer = tokio::task::block_in_place(|| {
            handle.block_on(input.answer(question, CancellationToken::new()))
        })
        .ok()?;
        Some(answer.trim().eq_ignore_ascii_case("y"))
    }

    /// Counts a detection; `None` lets the call run after the user allowed it.
    fn hit(&self, state: &mut State, reason: String) -> Option<String> {
        state.hits += 1;
        if state.hits >= MAX_HITS {
            state.stopped = true;
            return Some(format!("{reason}\n{STOPPED}"));
        }
        if state.hits == ASK_AT_HIT {
            match self.ask(&reason) {
                Some(true) => {
                    state.clear_loops();
                    return None;
                }
                Some(false) => {
                    state.stopped = true;
                    return Some(format!("{reason}\nThe user stopped the loop.\n{STOPPED}"));
                }
                None => {}
            }
        }
        Some(format!(
            "[loop detection] {reason}. This call was not run. Change approach: re-read the error, inspect the relevant code, or explain what blocks you instead of repeating the same step."
        ))
    }

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

impl TurnHook for ToolGuard {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = State::default();
        }
        Box::pin(async { None })
    }

    fn before_tool(&self, name: &str, input: &Value) -> Option<String> {
        let mut state = self.state.lock().ok()?;
        if state.stopped {
            return Some(STOPPED.into());
        }
        let sig = signature(name, input);
        match Self::guardrail(&mut state, name, &sig) {
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
            let reason = format!("the same {name} call ran {same} of the last {CALL_WINDOW} times");
            return self.hit(&mut state, reason);
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
            let reason = format!(
                "{pattern}… ran {similar} of the last {PATTERN_WINDOW} calls with changing arguments but no progress"
            );
            return self.hit(&mut state, reason);
        }
        None
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        let mut state = self.state.lock().ok()?;
        let sig = signature(name, input);
        let mut notes: Vec<String> = state.warnings.remove(&sig).into_iter().collect();
        if output.failed {
            state
                .recent_failures
                .push_back((Instant::now(), name.to_owned()));
            if state.recent_failures.len() > 100 {
                state.recent_failures.pop_front();
            }
            *state.exact_failures.entry(sig.clone()).or_default() += 1;
            *state.tool_failures.entry(name.to_owned()).or_default() += 1;
        } else {
            state.recent_failures.retain(|(_, tool)| tool != name);
            state.exact_failures.remove(&sig);
            state.tool_failures.insert(name.to_owned(), 0);
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
        }
        if !output.failed && matches!(name, "read" | "write" | "edit") {
            state.since_activity = 0;
        } else {
            state.since_activity += 1;
            if state.since_activity >= STALL_THRESHOLD {
                state.since_activity = 0;
                notes.push(format!("[loop detection] {STALL_THRESHOLD} tool calls without reading or editing a file; check whether the work is progressing."));
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
                let reason = format!(
                    "the same tool output appeared {count} times in the last {OUTPUT_WINDOW} results"
                );
                if let Some(note) = self.hit(&mut state, reason) {
                    notes.push(note.replace("This call was not run. ", ""));
                }
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
        let guard = ToolGuard::new(None);
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
    fn identical_reads_are_blocked_after_five_repeats() {
        let guard = ToolGuard::new(None);
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
    fn loops_are_refused_and_stop_after_five_detections() {
        let guard = ToolGuard::new(None);
        let input = json!({"path":"a.txt","old":"x","new":"y"});
        let mut refused = 0;
        let mut stopped = false;
        for _ in 0..30 {
            match guard.before_tool("edit", &input) {
                Some(reason) => {
                    refused += 1;
                    if reason.contains("stopped") {
                        stopped = true;
                        break;
                    }
                }
                None => {
                    guard.tool_note("edit", &input, &output("edited a.txt", false));
                }
            }
        }
        assert!(stopped && refused >= 3, "{refused}");
        assert!(
            guard
                .before_tool("shell", &json!({"command":"ls"}))
                .unwrap()
                .contains("stopped")
        );
    }

    #[test]
    fn changing_outputs_are_not_a_loop() {
        let guard = ToolGuard::new(None);
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
        let guard = ToolGuard::new(None);
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
