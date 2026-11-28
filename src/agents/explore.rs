//! What the parent gets back from a delegated investigation: the model tier of a
//! sub-agent type, a hard cap on the answer that lands in the parent's context, a
//! mechanical check of the `path:line` evidence in it, and a one-time hint when the main
//! task reads many files itself.

use std::{
    collections::HashSet,
    path::Path,
    sync::{Arc, LazyLock, Mutex},
};

use futures_util::future::BoxFuture;
use regex::Regex;
use serde_json::Value;
use sha2::{Digest, Sha256};

use crate::{
    agents::subagent::{ModelCatalog, claim_key},
    engine::turn::TurnHook,
    safety::gates::cite,
    session::Session,
    tools::ToolOutput,
};

/// Bytes of a sub-agent's answer rendered into the parent's context (about 2000 CJK
/// characters); the rest goes to `tool-outputs/`.
pub const CONCLUSION_LIMIT: usize = 6 * 1024;
/// Model calls of an explore run: 9 was the most seen; a low cap stops a drifting run early.
pub const EXPLORE_MAX_TURNS: u32 = 30;
/// Different files the main task reads in one stretch of read-only calls before the hint.
pub const HINT_FILES: usize = 6;

/// Which model a sub-agent type runs on.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ModelTier {
    /// The main model, even when the task was routed to the fast one.
    Main,
    /// The fast model when one different from the main model is configured.
    Fast,
    /// Whatever the current task runs on (no model in the request).
    Inherit,
    /// A model by name.
    Named(String),
}

impl ModelTier {
    /// `main`, `fast`, `inherit` or a model name (`agent_types.<name>.model`).
    pub fn parse(text: &str) -> std::result::Result<Self, String> {
        let text = text.trim();
        Ok(match text.to_ascii_lowercase().as_str() {
            "main" => Self::Main,
            "fast" => Self::Fast,
            "inherit" => Self::Inherit,
            _ if text.is_empty() || text.len() > 128 || text.chars().any(char::is_whitespace) => {
                return Err(format!(
                    "model must be main, fast, inherit or a model name, not {text:?}"
                ));
            }
            _ => Self::Named(text.to_owned()),
        })
    }

    /// The model of the request; `None` keeps the task's model.
    pub fn resolve(&self, catalog: &dyn ModelCatalog) -> Option<String> {
        match self {
            Self::Main => Some(catalog.main_model()),
            Self::Fast => Some(catalog.fast_model().unwrap_or_else(|| catalog.main_model())),
            Self::Inherit => None,
            Self::Named(model) => Some(model.clone()),
        }
    }
}

// ---------------------------------------------------------------- the answer's size

/// `output` within [`CONCLUSION_LIMIT`]; a longer answer is saved whole under `dir` and
/// the cut text ends with where it is. The budget is cove's, whatever the prompt asked.
pub fn cap_conclusion(output: &str, dir: Option<&Path>) -> String {
    if output.len() <= CONCLUSION_LIMIT {
        return output.to_owned();
    }
    let kept = crate::text::head(output, CONCLUSION_LIMIT);
    let kept = &kept[..kept
        .rfind('\n')
        .filter(|end| *end > 0)
        .unwrap_or(kept.len())];
    let size = format!("{:.1}", output.len() as f64 / 1024.0);
    let saved = dir.and_then(|dir| {
        let sum = Sha256::digest(output.as_bytes());
        let name = format!(
            "output_{}_agent.txt",
            sum[..8]
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect::<String>()
        );
        let path = dir.join(name);
        crate::persist::write_atomic(&path, output.as_bytes(), false)
            .ok()
            .map(|()| path)
    });
    match saved {
        Some(path) => format!(
            "{kept}\n[answer cut at {} KB: the full answer ({size} KB) is saved at {}; read it with the read tool if needed]",
            CONCLUSION_LIMIT / 1024,
            crate::paths::display(&path)
        ),
        None => format!(
            "{kept}\n[answer cut at {} KB of {size} KB]",
            CONCLUSION_LIMIT / 1024
        ),
    }
}

// ---------------------------------------------------------------- evidence

static UNRESOLVED: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?im)^[ \t>*#\-]*(?:not determined|unresolved|open questions?|没查清|未查清|未确定)[ \t*]*[:：]")
        .expect("unresolved heading")
});

fn marker(verdict: &cite::Verdict) -> Option<String> {
    Some(match verdict {
        cite::Verdict::Passed | cite::Verdict::Offset { .. } => return None,
        cite::Verdict::Missing => "[未核实：文件不存在]".into(),
        cite::Verdict::OutOfRange { lines } => format!("[未核实：超出文件行数 {lines}]"),
        cite::Verdict::Ambiguous => "[未核实：有多个同名文件，无法确定是哪一个]".into(),
        cite::Verdict::Mismatch { .. } | cite::Verdict::Unverified => {
            "[未核实：不在工作区内或无法读取]".into()
        }
    })
}

fn absolute(path: &str) -> bool {
    let bytes = path.as_bytes();
    path.starts_with('/')
        || path.starts_with('\\')
        || (bytes.len() > 2 && bytes[0].is_ascii_alphabetic() && bytes[1] == b':')
}

/// `text` with every `path:line` that does not exist in `root` marked after its line,
/// and the summary line for the result header (`citations: 7 ok, 1 invalid`). Only
/// whether the place exists is checked, not what it says (the parent re-reads what it
/// relies on). `required`: an answer without evidence and without a "not determined"
/// part is flagged `citations: none`.
pub fn annotate(root: &Path, text: &str, required: bool) -> (String, Option<String>) {
    let prose = cite::prose(text);
    let files = cite::Files::scan(root);
    let citations = cite::parse(&prose, &files.exts);
    if citations.is_empty() {
        let flagged = required && !text.trim().is_empty() && !UNRESOLVED.is_match(text);
        return (
            text.to_owned(),
            flagged.then(|| {
                "citations: none (no path:line evidence: treat the conclusion as a lead, not a fact)"
                    .to_owned()
            }),
        );
    }
    let mut marks: Vec<(usize, String)> = Vec::new();
    let mut invalid = 0;
    for citation in &citations {
        let mut verdict = cite::verify(&files, citation, &[], "");
        // A relative path of a kind the workspace has none of is still just missing.
        if verdict == cite::Verdict::Unverified
            && !citation.bound
            && !absolute(&citation.path)
            && !citation.path.split('/').any(|part| part == "..")
            && files.resolve(&citation.path).is_empty()
        {
            verdict = cite::Verdict::Missing;
        }
        let Some(mark) = marker(&verdict) else {
            continue;
        };
        invalid += 1;
        let end = text[citation.span.end..]
            .find('\n')
            .map_or(text.len(), |at| citation.span.end + at);
        let end = text[..end].trim_end_matches('\r').len();
        match marks.iter_mut().find(|(at, _)| *at == end) {
            Some((_, existing)) if !existing.contains(&mark) => {
                existing.push(' ');
                existing.push_str(&mark);
            }
            Some(_) => {}
            None => marks.push((end, mark)),
        }
    }
    let mut out = text.to_owned();
    marks.sort_by_key(|(at, _)| std::cmp::Reverse(*at));
    for (at, mark) in marks {
        out.insert_str(at, &format!(" {mark}"));
    }
    let summary = format!(
        "citations: {} ok, {invalid} invalid",
        citations.len() - invalid
    );
    (out, Some(summary))
}

// ---------------------------------------------------------------- the hint

const NEUTRAL: &[&str] = &["todo", "ask_user", "question", "memory", "skill"];
const READING: &[&str] = &[
    "read",
    "glob",
    "grep",
    "repo_map",
    "web_fetch",
    "web_search",
];

#[derive(Default)]
struct HintState {
    files: HashSet<String>,
    delegated: bool,
    hinted: bool,
}

/// Once per task, after the main task read [`HINT_FILES`] different files in one stretch
/// of read-only calls without delegating: a pointer at explore. Never when the task
/// already runs on the fast model or no fast model is configured.
pub struct ExploreHint {
    pub catalog: Arc<dyn ModelCatalog>,
    state: Mutex<HintState>,
}

impl ExploreHint {
    pub fn new(catalog: Arc<dyn ModelCatalog>) -> Self {
        Self {
            catalog,
            state: Mutex::default(),
        }
    }

    fn worth_it(&self) -> bool {
        self.catalog
            .fast_model()
            .is_some_and(|fast| fast != self.catalog.task_model())
    }
}

fn read_path(input: &Value) -> Option<&str> {
    ["path", "file_path", "filePath"]
        .iter()
        .find_map(|key| input.get(key).and_then(Value::as_str))
}

impl TurnHook for ExploreHint {
    fn task_note<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = HintState::default();
        }
        Box::pin(async { None })
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        let mut state = self.state.lock().ok()?;
        if matches!(name, "agent" | "execute_plan") {
            state.delegated = true;
            state.files.clear();
            return None;
        }
        if NEUTRAL.contains(&name) {
            return None;
        }
        if !READING.contains(&name) {
            state.files.clear();
            return None;
        }
        if name == "read"
            && !output.failed
            && let Some(path) = read_path(input)
        {
            state.files.insert(claim_key(path));
        }
        if state.files.len() < HINT_FILES || state.hinted || state.delegated || !self.worth_it() {
            return None;
        }
        state.hinted = true;
        Some(format!(
            "[hint] This stretch of investigation has read {} different files. If the rest of it only needs a conclusion, \
             delegate it to an explore agent (fast model, several run in parallel, it returns only the conclusion and path:line evidence). \
             Keep reading yourself if you are about to edit these files or are narrowing down a bug step by step.",
            state.files.len()
        ))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Models {
        fast: Option<&'static str>,
        task: &'static str,
    }

    impl ModelCatalog for Models {
        fn main_model(&self) -> String {
            "main-model".into()
        }
        fn fast_model(&self) -> Option<String> {
            self.fast.map(str::to_owned)
        }
        fn task_model(&self) -> String {
            self.task.into()
        }
        fn prices(&self, _model: &str) -> Option<crate::model::models::Prices> {
            None
        }
        fn budget(&self, _model: &str) -> (usize, u32) {
            (100_000, 8000)
        }
    }

    #[test]
    fn tiers_resolve_against_the_configured_models() {
        let with_fast = Models {
            fast: Some("fast-model"),
            task: "fast-model",
        };
        let without = Models {
            fast: None,
            task: "main-model",
        };
        assert_eq!(
            ModelTier::Fast.resolve(&with_fast).as_deref(),
            Some("fast-model")
        );
        assert_eq!(
            ModelTier::Fast.resolve(&without).as_deref(),
            Some("main-model")
        );
        // The task was routed to the fast model; main still means the main model.
        assert_eq!(
            ModelTier::Main.resolve(&with_fast).as_deref(),
            Some("main-model")
        );
        assert_eq!(ModelTier::Inherit.resolve(&with_fast), None);
        assert_eq!(ModelTier::parse("FAST"), Ok(ModelTier::Fast));
        assert_eq!(
            ModelTier::parse("claude-haiku-4-5"),
            Ok(ModelTier::Named("claude-haiku-4-5".into()))
        );
        assert!(ModelTier::parse("").is_err());
        assert!(ModelTier::parse("two words").is_err());
    }

    #[test]
    fn long_answers_are_cut_on_a_character_boundary_and_saved_whole() {
        let dir = tempfile::tempdir().unwrap();
        let line = "结论：超时在 B 处理器里被除了两次，单位从秒变成毫秒以后又除以一千。\n";
        let answer = line.repeat(20 * 1024 / line.len() + 1);
        assert!(answer.len() > 20 * 1024);
        let capped = cap_conclusion(&answer, Some(dir.path()));
        assert!(capped.len() < CONCLUSION_LIMIT + 400, "{}", capped.len());
        assert!(capped.contains("the full answer"), "{capped}");
        let saved: Vec<_> = std::fs::read_dir(dir.path())
            .unwrap()
            .map(|entry| entry.unwrap().path())
            .collect();
        assert_eq!(saved.len(), 1);
        assert!(
            saved[0]
                .file_name()
                .unwrap()
                .to_string_lossy()
                .starts_with("output_")
        );
        assert_eq!(std::fs::read_to_string(&saved[0]).unwrap(), answer);
        assert!(capped.contains(&crate::paths::display(&saved[0])));
        // Short answers pass unchanged.
        assert_eq!(cap_conclusion("short", Some(dir.path())), "short");
    }

    fn workspace() -> tempfile::TempDir {
        let root = tempfile::tempdir().unwrap();
        std::fs::create_dir_all(root.path().join("src/engine")).unwrap();
        let body: String = (1..=40).map(|n| format!("line {n}\n")).collect();
        std::fs::write(root.path().join("src/engine/agent.rs"), &body).unwrap();
        std::fs::write(root.path().join("src/lib.rs"), "pub mod engine;\n").unwrap();
        root
    }

    #[test]
    fn evidence_is_checked_for_existence_and_range() {
        let root = workspace();
        let answer = "结论：计费按配置价。\n证据：\n- src/engine/agent.rs:12-14 计费\n- src/engine/agent.rs:90 超出\n- src/engine/missing.rs:3 不存在\n- src/lib.rs:1 模块\n没查清：无";
        let (text, summary) = annotate(root.path(), answer, true);
        assert_eq!(summary.as_deref(), Some("citations: 2 ok, 2 invalid"));
        assert!(
            text.contains("src/engine/agent.rs:90 超出 [未核实：超出文件行数 40]"),
            "{text}"
        );
        assert!(
            text.contains("src/engine/missing.rs:3 不存在 [未核实：文件不存在]"),
            "{text}"
        );
        assert!(text.contains("src/engine/agent.rs:12-14 计费\n"), "{text}");
        // A path outside the workspace is not taken as verified.
        let outside = "证据：\n- /etc/elsewhere/agent.rs:3 别处";
        let (text, summary) = annotate(root.path(), outside, true);
        assert_eq!(summary.as_deref(), Some("citations: 0 ok, 1 invalid"));
        assert!(text.contains("[未核实："), "{text}");
    }

    #[test]
    fn absolute_and_verbatim_paths_inside_the_workspace_are_found() {
        let root = workspace();
        let canonical = root.path().canonicalize().unwrap();
        let absolute = format!(
            "证据：\n- {}:5 计费",
            canonical.join("src/engine/agent.rs").to_string_lossy()
        );
        let (_, summary) = annotate(&canonical, &absolute, true);
        assert_eq!(
            summary.as_deref(),
            Some("citations: 1 ok, 0 invalid"),
            "{absolute}"
        );
        let plain = format!(
            "证据：\n- {}:5 计费",
            crate::paths::plain(&canonical.join("src/engine/agent.rs")).to_string_lossy()
        );
        let (_, summary) = annotate(&canonical, &plain, true);
        assert_eq!(
            summary.as_deref(),
            Some("citations: 1 ok, 0 invalid"),
            "{plain}"
        );
    }

    #[test]
    fn an_answer_without_evidence_is_flagged_unless_it_says_what_is_open() {
        let root = workspace();
        let (_, summary) = annotate(root.path(), "结论：没有调用者。", true);
        assert!(summary.unwrap().starts_with("citations: none"));
        let (_, summary) = annotate(
            root.path(),
            "结论：不确定。\n没查清：用 grep 'save(' 搜了 src/，没有结果",
            true,
        );
        assert_eq!(summary, None);
        // Types without the evidence format are not flagged.
        assert_eq!(annotate(root.path(), "Looks fine.", false).1, None);
    }

    fn read(hook: &ExploreHint, path: &str) -> Option<String> {
        let input = serde_json::json!({ "path": path });
        hook.tool_note("read", &input, &ToolOutput::default())
    }

    #[test]
    fn the_hint_comes_once_at_the_sixth_file_of_a_stretch() {
        let hook = ExploreHint::new(Arc::new(Models {
            fast: Some("fast-model"),
            task: "main-model",
        }));
        for n in 1..HINT_FILES {
            assert!(read(&hook, &format!("src/f{n}.rs")).is_none());
        }
        // The same file again is not a new one.
        assert!(read(&hook, "src/f1.rs").is_none());
        assert!(read(&hook, "src/f6.rs").unwrap().contains("explore"));
        assert!(read(&hook, "src/f7.rs").is_none());
        // An edit starts a new stretch, but the hint was already given this task.
        hook.tool_note("edit", &serde_json::json!({}), &ToolOutput::default());
        for n in 10..20 {
            assert!(read(&hook, &format!("src/g{n}.rs")).is_none());
        }
    }

    #[test]
    fn an_edit_in_between_restarts_the_count() {
        let hook = ExploreHint::new(Arc::new(Models {
            fast: Some("fast-model"),
            task: "main-model",
        }));
        for n in 1..HINT_FILES {
            assert!(read(&hook, &format!("src/f{n}.rs")).is_none());
        }
        hook.tool_note("edit", &serde_json::json!({}), &ToolOutput::default());
        assert!(read(&hook, "src/f6.rs").is_none());
        for n in 7..(7 + HINT_FILES - 2) {
            assert!(read(&hook, &format!("src/f{n}.rs")).is_none());
        }
        assert!(read(&hook, "src/last.rs").is_some());
    }

    #[test]
    fn no_hint_after_delegating_or_on_the_fast_model() {
        let delegated = ExploreHint::new(Arc::new(Models {
            fast: Some("fast-model"),
            task: "main-model",
        }));
        delegated.tool_note("agent", &serde_json::json!({}), &ToolOutput::default());
        for n in 0..10 {
            assert!(read(&delegated, &format!("src/f{n}.rs")).is_none());
        }
        for models in [
            Models {
                fast: Some("fast-model"),
                task: "fast-model",
            },
            Models {
                fast: None,
                task: "main-model",
            },
        ] {
            let hook = ExploreHint::new(Arc::new(models));
            for n in 0..10 {
                assert!(read(&hook, &format!("src/f{n}.rs")).is_none());
            }
        }
    }
}
