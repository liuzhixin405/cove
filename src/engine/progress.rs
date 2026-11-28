//! Whether a task is getting anywhere.
//!
//! Counted per round: one model reply with all its tool calls and results, a text-only
//! reply sent back by a finish nudge, a failed automatic verification, or review
//! findings handed back. A round makes progress when it brings a file change, a
//! result or an error not seen before in this run, a different verification result,
//! different review findings, or the user's words. Refused calls (guardrail,
//! loop detection, plan mode, deny rules, the user) and bookkeeping tools never do.
//! Half the threshold of idle rounds warns the model; the full threshold stalls the
//! task, which the agent loop confirms against the work tree and then asks or stops.

use std::{
    collections::{HashMap, HashSet},
    hash::{Hash, Hasher},
};

use futures_util::future::BoxFuture;
use serde_json::Value;

/// Default of `stall_rounds`: twice the longest idle stretch of real local tasks.
pub const STALL_ROUNDS: u32 = 12;
/// Tools that keep notes; they neither count as progress nor reset the count.
const BOOKKEEPING: &[&str] = &["todo", "memory", "skill"];
/// Bytes of a result's start and end that identify it.
const HEAD: usize = 4096;
const TAIL: usize = 1024;

/// The work tree now, as a comparable id (the snapshot store's tree hash).
pub trait WorkTree: Send + Sync {
    /// `None` when it is unavailable or too slow to tell.
    fn tree(&self) -> BoxFuture<'_, Option<String>>;
}

/// One finished tool call of a round.
#[derive(Clone, Debug)]
pub struct Observed {
    pub name: String,
    pub input: Value,
    /// The result before turn hooks appended their notes.
    pub content: String,
    pub failed: bool,
    /// Not run: a turn hook, the user or nobody-to-ask refused it.
    pub refused: bool,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Verdict {
    Progress,
    /// Rounds in a row without progress, below the warning.
    Idle(u32),
    /// Half the threshold: the note to append for the model.
    Warn(String),
    /// The threshold is reached.
    Stall,
}

#[derive(Debug)]
pub struct Progress {
    base: u32,
    limit: u32,
    idle: u32,
    idle_max: u32,
    /// Rounds settled so far.
    rounds: u32,
    seen: HashSet<u64>,
    trees: HashSet<String>,
    verification: Option<u64>,
    review: Option<u64>,
    /// Errors and refusals of this run: signature -> (first-seen order, label, the
    /// round of each occurrence).
    errors: HashMap<u64, (usize, String, Vec<u32>)>,
    changed: Vec<String>,
}

fn hash(value: impl Hash) -> u64 {
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    value.hash(&mut hasher);
    hasher.finish()
}

/// Text with the parts that differ between identical outcomes masked: the `\\?\`
/// prefix goes, hex strings of 7 or more characters with a digit become `H`, digit
/// runs become `#` (durations, ports, times, line numbers).
pub fn normalize(text: &str) -> String {
    let text = text.replace(r"\\?\", "");
    let mut out = String::with_capacity(text.len());
    let mut run = String::new();
    let flush = |run: &mut String, out: &mut String| {
        if run.len() >= 7 && run.chars().any(|c| c.is_ascii_digit()) {
            out.push('H');
        } else {
            let mut digits = false;
            for c in run.chars() {
                if c.is_ascii_digit() {
                    if !digits {
                        out.push('#');
                    }
                    digits = true;
                } else {
                    out.push(c);
                    digits = false;
                }
            }
        }
        run.clear();
    };
    for c in text.chars() {
        if c.is_ascii_hexdigit() {
            run.push(c);
        } else {
            flush(&mut run, &mut out);
            out.push(c);
        }
    }
    flush(&mut run, &mut out);
    out
}

/// A result's identity: the normalized start and end of it.
fn digest(content: &str) -> u64 {
    if content.len() <= HEAD + TAIL {
        hash(normalize(content))
    } else {
        hash((
            normalize(crate::text::head(content, HEAD)),
            normalize(crate::text::tail(content, TAIL)),
        ))
    }
}

/// The argument that says what a call is about, as written (paths keep their digits:
/// `shot1.png` and `shot2.png` are different reads); the whole input otherwise.
fn main_argument(input: &Value) -> String {
    [
        "command", "path", "pattern", "query", "url", "prompt", "task",
    ]
    .iter()
    .find_map(|key| input.get(*key).and_then(Value::as_str))
    .map(|text| text.trim().replace(r"\\?\", ""))
    .unwrap_or_else(|| input.to_string())
}

/// Lines of an error that say what went wrong: shell framing (`exit_code=…`,
/// `stdout:`, `stderr:`) and notes left out.
fn error_lines(content: &str) -> Vec<&str> {
    content
        .lines()
        .map(str::trim)
        .filter(|line| {
            !line.is_empty()
                && !matches!(*line, "stdout:" | "stderr:")
                && !line.starts_with("exit_code=")
                && !line.starts_with("[note:")
                && !line.starts_with("[hint:")
        })
        .collect()
}

/// The repeat detector (`safety::gates::guard`) that refused a call, from the first
/// line of the refusal.
fn detector(first: &str) -> Option<&'static str> {
    ["guardrail", "loop detection"]
        .into_iter()
        .find(|name| first.starts_with(&format!("[{name}]")))
}

fn exit_code(content: &str) -> Option<&str> {
    let rest = content.lines().next()?.strip_prefix("exit_code=")?;
    rest.split_whitespace().next()
}

impl Progress {
    /// `rounds` idle rounds stall the task; 0 turns the check off.
    pub fn new(rounds: u32) -> Self {
        Self {
            base: rounds,
            limit: rounds,
            idle: 0,
            idle_max: 0,
            rounds: 0,
            seen: HashSet::new(),
            trees: HashSet::new(),
            verification: None,
            review: None,
            errors: HashMap::new(),
            changed: Vec::new(),
        }
    }

    pub fn idle(&self) -> u32 {
        self.idle
    }

    /// The longest run of idle rounds so far.
    pub fn idle_max(&self) -> u32 {
        self.idle_max
    }

    /// The current threshold (doubled each time the user lets the task go on as is).
    pub fn limit(&self) -> u32 {
        self.limit
    }

    /// The configured threshold.
    pub fn base(&self) -> u32 {
        self.base
    }

    /// A round of tool calls.
    pub fn round(&mut self, calls: &[Observed]) -> Verdict {
        let mut progressed = false;
        for call in calls {
            let name = call.name.as_str();
            if BOOKKEEPING.contains(&name) {
                continue;
            }
            if call.refused {
                let first = error_lines(&call.content).first().copied().unwrap_or("");
                let argument = crate::text::short(&main_argument(&call.input), 60);
                // cove's repeat detectors word their refusals for the model, with counts
                // that change from call to call: one kind each, named by the detector.
                let (signature, label) = match detector(first) {
                    Some(detector) => (
                        hash(("refused", name, detector)),
                        format!("{name} `{argument}` → 被拦下（{detector}）"),
                    ),
                    None => (
                        hash(("refused", name, normalize(first))),
                        format!(
                            "{name} `{argument}` → 被拒绝：{}",
                            crate::text::short(first, 80)
                        ),
                    ),
                };
                self.count_error(signature, label);
                continue;
            }
            if call.failed {
                let lines = error_lines(&call.content);
                let first = lines.first().copied().unwrap_or("");
                let last = lines.last().copied().unwrap_or("");
                let signature = hash((
                    "error",
                    name,
                    normalize(first),
                    normalize(last),
                    exit_code(&call.content),
                ));
                progressed |= self.seen.insert(signature);
                self.count_error(
                    signature,
                    format!(
                        "{name} `{}` → {}",
                        crate::text::short(&main_argument(&call.input), 60),
                        crate::text::short(last, 120)
                    ),
                );
                continue;
            }
            if name == "ask_user" {
                // The user answered.
                progressed = true;
                continue;
            }
            if matches!(name, "write" | "edit") {
                let path = call
                    .input
                    .get("path")
                    .and_then(Value::as_str)
                    .unwrap_or_default()
                    .replace(r"\\?\", "")
                    .replace('\\', "/");
                let change = match call.input.get("content") {
                    Some(content) if name == "write" => hash(("write", &path, content.to_string())),
                    _ => hash(("edit", &path, call.input.to_string())),
                };
                if self.seen.insert(change) {
                    progressed = true;
                    if !self.changed.contains(&path) {
                        self.changed.push(path);
                    }
                }
                continue;
            }
            let info = hash((
                "info",
                name,
                main_argument(&call.input),
                digest(&call.content),
            ));
            progressed |= self.seen.insert(info);
        }
        self.settle(progressed)
    }

    /// A text-only reply that a finish nudge sends back.
    pub fn nudged(&mut self) -> Verdict {
        self.settle(false)
    }

    /// A failed automatic verification: progress when its details changed.
    pub fn verification(&mut self, detail: &str) -> Verdict {
        let digest = digest(detail);
        let changed = self.verification != Some(digest);
        self.verification = Some(digest);
        self.settle(changed)
    }

    /// Review findings handed back to the model: progress when they differ from the
    /// last ones handed back (`digest` names them by level and file).
    pub fn review(&mut self, digest: &str) -> Verdict {
        let digest = hash(digest);
        let changed = self.review != Some(digest);
        self.review = Some(digest);
        self.settle(changed)
    }

    /// The user said something (steering, a typed instruction): count afresh.
    pub fn user_input(&mut self) {
        self.idle = 0;
    }

    /// The task changed models (the fast model escalated to the main one): the new
    /// model gets a full window.
    pub fn model_switched(&mut self) {
        self.idle = 0;
    }

    /// The user asked for another approach.
    pub fn retry(&mut self) {
        self.idle = 0;
    }

    /// The user let the task go on as it was: count afresh to twice the threshold.
    pub fn keep_going(&mut self) {
        self.limit = self.limit.saturating_mul(2);
        self.idle = 0;
    }

    /// Records the work tree; `true` (and a fresh count) when it is one not seen in
    /// this run although an earlier one was: shell commands or sub-agents changed files.
    pub fn tree(&mut self, tree: String) -> bool {
        let changed = !self.trees.is_empty() && !self.trees.contains(&tree);
        self.trees.insert(tree);
        if changed {
            self.idle = 0;
        }
        changed
    }

    /// The most frequent errors of the current idle rounds (at least twice), with how
    /// often each came up in them: the rounds the warning and the question speak of.
    pub fn repeated(&self, n: usize) -> Vec<(String, u32)> {
        let since = self.rounds.saturating_sub(self.idle);
        let mut errors: Vec<_> = self
            .errors
            .values()
            .map(|(order, label, rounds)| {
                let count = rounds.iter().filter(|round| **round > since).count() as u32;
                (*order, label, count)
            })
            .filter(|(_, _, count)| *count >= 2)
            .collect();
        errors.sort_by(|a, b| b.2.cmp(&a.2).then(a.0.cmp(&b.0)));
        errors
            .into_iter()
            .take(n)
            .map(|(_, label, count)| (label.clone(), count))
            .collect()
    }

    /// Files written or edited with the file tools in this run.
    pub fn changed(&self) -> &[String] {
        &self.changed
    }

    /// Local facts for the closing summary, in English for the model.
    pub fn facts(&self, model_calls: u32) -> String {
        let mut lines = vec![format!(
            "Facts recorded by cove: {model_calls} model calls in this run; the last {} rounds brought no new change or information.",
            self.idle
        )];
        let repeated = self.repeated(3);
        if !repeated.is_empty() {
            lines.push("Repeated errors in those rounds:".into());
            for (label, count) in repeated {
                lines.push(format!("- {label} (x{count})"));
            }
        }
        if !self.changed.is_empty() {
            lines.push(format!(
                "Files changed with write/edit: {}",
                self.changed.join(", ")
            ));
        }
        lines.join("\n")
    }

    /// An error of the round being counted (settled next).
    fn count_error(&mut self, signature: u64, label: String) {
        let order = self.errors.len();
        let round = self.rounds + 1;
        self.errors
            .entry(signature)
            .or_insert((order, label, Vec::new()))
            .2
            .push(round);
    }

    fn settle(&mut self, progressed: bool) -> Verdict {
        self.rounds += 1;
        if progressed {
            self.idle = 0;
            return Verdict::Progress;
        }
        self.idle += 1;
        self.idle_max = self.idle_max.max(self.idle);
        if self.limit == 0 {
            return Verdict::Idle(self.idle);
        }
        if self.idle >= self.limit {
            return Verdict::Stall;
        }
        let warn_at = self.limit / 2;
        if warn_at > 0 && self.idle == warn_at {
            return Verdict::Warn(self.warning());
        }
        Verdict::Idle(self.idle)
    }

    fn warning(&self) -> String {
        let repeated = self
            .repeated(1)
            .into_iter()
            .next()
            .map(|(label, count)| format!("（反复出现：{label} ×{count}）"))
            .unwrap_or_default();
        format!(
            "[progress] 最近 {} 轮没有新的改动或新信息{repeated}。先说明是什么挡住了你，再换一个不同的做法；如果缺的是外部条件（服务、权限、依赖），直接说明并结束。",
            self.idle
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn call(name: &str, input: Value, content: &str, failed: bool) -> Observed {
        Observed {
            name: name.into(),
            input,
            content: content.into(),
            failed,
            refused: false,
        }
    }

    fn shell(command: &str, output: &str, failed: bool) -> Observed {
        let code = if failed { 1 } else { 0 };
        call(
            "shell",
            json!({ "command": command }),
            &format!(
                "exit_code=Some({code}) timed_out=false truncated=false encoding_warning=false\nstdout:\n{output}\nstderr:\n"
            ),
            failed,
        )
    }

    /// Verdicts of `rounds`, one call per round.
    fn run(progress: &mut Progress, rounds: impl IntoIterator<Item = Observed>) -> Vec<Verdict> {
        rounds
            .into_iter()
            .map(|observed| progress.round(&[observed]))
            .collect()
    }

    fn first_stall(verdicts: &[Verdict]) -> Option<usize> {
        verdicts
            .iter()
            .position(|verdict| *verdict == Verdict::Stall)
            .map(|index| index + 1)
    }

    #[test]
    fn the_same_error_warns_at_six_and_stalls_at_twelve() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..13).map(|_| shell("python sync.py", "boom", true)),
        );
        // The first failure is new information; the next twelve are not.
        assert_eq!(verdicts[0], Verdict::Progress);
        assert!(matches!(verdicts[6], Verdict::Warn(_)), "{verdicts:?}");
        assert_eq!(first_stall(&verdicts), Some(13));
        assert!(verdicts[..12].iter().all(|v| *v != Verdict::Stall));
        let Verdict::Warn(note) = &verdicts[6] else {
            unreachable!()
        };
        // Counted in the six idle rounds the note speaks of, not since the start.
        assert!(
            note.contains("×6") && note.contains("python sync.py"),
            "{note}"
        );
    }

    #[test]
    fn repeats_are_counted_in_the_current_idle_rounds() {
        let mut progress = Progress::new(4);
        let verdicts = run(&mut progress, (0..5).map(|_| shell("ls", "same", true)));
        assert_eq!(first_stall(&verdicts), Some(5));
        assert_eq!(progress.repeated(1)[0].1, 4);
        progress.keep_going();
        assert!(progress.repeated(1).is_empty(), "a fresh count");
        let verdicts = run(&mut progress, (0..8).map(|_| shell("ls", "same", true)));
        assert_eq!(first_stall(&verdicts), Some(8));
        // Eight in the eight rounds asked about, thirteen in the whole run.
        assert_eq!(progress.repeated(1)[0].1, 8);
        assert!(progress.facts(13).contains("(x8)"));
    }

    #[test]
    fn refusals_of_a_repeat_detector_are_one_kind_named_by_it() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (5..17).map(|times| Observed {
                refused: true,
                ..shell(
                    "ls missing",
                    &format!(
                        "[guardrail] shell failed {times} times within 30 s; the call is blocked."
                    ),
                    true,
                )
            }),
        );
        assert_eq!(first_stall(&verdicts), Some(12));
        let repeated = progress.repeated(3);
        assert_eq!(repeated.len(), 1, "{repeated:?}");
        assert_eq!(repeated[0].0, "shell `ls missing` → 被拦下（guardrail）");
        assert_eq!(repeated[0].1, 12);
    }

    #[test]
    fn the_same_error_from_different_commands_is_one_error() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..13).map(|port| {
                shell(
                    &format!("curl http://127.0.0.1:{}/rates", 59000 + port),
                    &format!(
                        "curl: (7) Failed to connect to 127.0.0.1 port {} after {port} ms: Connection refused",
                        59000 + port
                    ),
                    true,
                )
            }),
        );
        assert_eq!(first_stall(&verdicts), Some(13));
        // Twelve in the twelve idle rounds; the first one was new information.
        assert!(progress.facts(13).contains("Connection refused (x12)"));
    }

    #[test]
    fn changing_errors_are_progress() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            [
                "alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota",
                "kappa", "lambda", "mu", "nu",
            ]
            .map(|word| shell("make", &format!("error: {word} missing"), true)),
        );
        assert!(
            verdicts.iter().all(|v| *v == Verdict::Progress),
            "{verdicts:?}"
        );
    }

    #[test]
    fn edits_with_new_content_are_progress_and_going_back_and_forth_is_not() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..13).map(|index| {
                call(
                    "edit",
                    json!({"path":"a.py","old":format!("v{index}"),"new":format!("v{}", index + 1)}),
                    "edited a.py",
                    false,
                )
            }),
        );
        assert!(verdicts.iter().all(|v| *v == Verdict::Progress));
        assert_eq!(progress.changed(), ["a.py"]);

        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..14).map(|index| {
                let (old, new) = if index % 2 == 0 {
                    ("A", "B")
                } else {
                    ("B", "A")
                };
                call(
                    "edit",
                    json!({"path":"a.py","old":old,"new":new}),
                    "edited a.py",
                    false,
                )
            }),
        );
        assert_eq!(verdicts[0], Verdict::Progress);
        assert_eq!(verdicts[1], Verdict::Progress);
        assert!(verdicts[2..].iter().all(|v| *v != Verdict::Progress));
        assert_eq!(first_stall(&verdicts), Some(14));
    }

    #[test]
    fn reading_different_images_is_progress() {
        // Thirteen screenshots whose results differ only in numbers.
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..13).map(|index| {
                call(
                    "read",
                    json!({"path":format!("shots/shot{index}.png")}),
                    &format!("[image: 1280x{} PNG, {} KB]", 700 + index, 90 + index),
                    false,
                )
            }),
        );
        assert!(
            verdicts.iter().all(|v| *v == Verdict::Progress),
            "{verdicts:?}"
        );
    }

    #[test]
    fn outputs_that_differ_only_in_times_are_the_same() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..3).map(|index| {
                shell(
                    "cargo test",
                    &format!(
                        "test result: ok. 3 passed; finished in {index}.{}s at 2026-11-23T10:0{index}:00 build 9f3e2a1{index}c",
                        index * 7
                    ),
                    false,
                )
            }),
        );
        assert_eq!(
            verdicts,
            [Verdict::Progress, Verdict::Idle(1), Verdict::Idle(2)]
        );
    }

    #[test]
    fn refused_calls_never_progress() {
        // Every edit refused by the guardrail.
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..12).map(|index| Observed {
                refused: true,
                ..call(
                    "edit",
                    json!({"path":"a.py","old":format!("x{index}"),"new":"y"}),
                    "[guardrail] edit failed 8 times in a row; it is blocked. Try another tool or method.",
                    true,
                )
            }),
        );
        assert_eq!(first_stall(&verdicts), Some(12));
        assert!(progress.repeated(1)[0].0.ends_with("被拦下（guardrail）"));
    }

    #[test]
    fn bookkeeping_alone_is_an_idle_round() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(
            &mut progress,
            (0..12).map(|index| {
                call(
                    "todo",
                    json!({"items":[{"text":format!("step {index}")}]}),
                    &format!("todo updated {index}"),
                    false,
                )
            }),
        );
        assert_eq!(first_stall(&verdicts), Some(12));
    }

    #[test]
    fn user_input_counts_afresh() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let mut verdicts = run(&mut progress, (0..10).map(|_| shell("ls", "same", true)));
        progress.user_input();
        verdicts.extend(run(
            &mut progress,
            (0..10).map(|_| shell("ls", "same", true)),
        ));
        assert_eq!(first_stall(&verdicts), None);
        assert_eq!(progress.idle(), 10);
    }

    #[test]
    fn unchanged_verification_details_are_idle() {
        let mut progress = Progress::new(STALL_ROUNDS);
        assert_eq!(progress.verification("1 test failed: a"), Verdict::Progress);
        assert_eq!(progress.verification("1 test failed: a"), Verdict::Idle(1));
        assert_eq!(progress.verification("1 test failed: b"), Verdict::Progress);
        assert_eq!(progress.nudged(), Verdict::Idle(1));
    }

    #[test]
    fn new_review_findings_are_progress() {
        // A review round that hands back different findings is not an idle round.
        let mut progress = Progress::new(STALL_ROUNDS);
        assert_eq!(progress.review("project:{(High, a.py)}"), Verdict::Progress);
        assert_eq!(progress.review("project:{(High, b.py)}"), Verdict::Progress);
        assert_eq!(progress.review("project:{(High, b.py)}"), Verdict::Idle(1));
        // Its own slot: a verification result does not reset it.
        assert_eq!(progress.verification("1 test failed"), Verdict::Progress);
        assert_eq!(progress.review("project:{(High, b.py)}"), Verdict::Idle(1));
    }

    #[test]
    fn the_threshold_is_configurable() {
        let mut off = Progress::new(0);
        let verdicts = run(&mut off, (0..30).map(|_| shell("ls", "same", true)));
        assert!(
            verdicts
                .iter()
                .all(|v| !matches!(v, Verdict::Warn(_) | Verdict::Stall))
        );

        let mut four = Progress::new(4);
        let verdicts = run(&mut four, (0..5).map(|_| shell("ls", "same", true)));
        assert!(matches!(verdicts[2], Verdict::Warn(_)), "{verdicts:?}");
        assert_eq!(first_stall(&verdicts), Some(5));
    }

    #[test]
    fn going_on_as_is_doubles_the_threshold() {
        let mut progress = Progress::new(STALL_ROUNDS);
        let verdicts = run(&mut progress, (0..13).map(|_| shell("ls", "same", true)));
        assert_eq!(first_stall(&verdicts), Some(13));
        progress.keep_going();
        let verdicts = run(&mut progress, (0..24).map(|_| shell("ls", "same", true)));
        assert!(matches!(verdicts[11], Verdict::Warn(_)), "{verdicts:?}");
        assert_eq!(first_stall(&verdicts), Some(24));
        assert_eq!(progress.limit(), 24);
    }

    #[test]
    fn a_new_work_tree_counts_as_progress_once_there_is_a_baseline() {
        let mut progress = Progress::new(STALL_ROUNDS);
        run(&mut progress, (0..6).map(|_| shell("ls", "same", true)));
        assert!(!progress.tree("t1".into()), "no baseline yet");
        assert!(!progress.tree("t1".into()));
        assert!(progress.tree("t2".into()));
        assert_eq!(progress.idle(), 0);
        assert!(!progress.tree("t1".into()), "going back is not progress");
    }

    #[test]
    fn normalization_masks_numbers_hashes_and_the_verbatim_prefix() {
        assert_eq!(
            normalize("took 12.5ms on port 8080"),
            "took #.#ms on port #"
        );
        assert_eq!(normalize("commit 9f3e2a1c done"), "commit H done");
        assert_eq!(normalize(r"\\?\D:\work\a.rs"), r"D:\work\a.rs");
        assert_eq!(normalize("deadbeefcafe"), "deadbeefcafe");
        assert_eq!(normalize("汉字 3 个"), "汉字 # 个");
    }
}
