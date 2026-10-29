//! Tool-output management: per-tool result caps scaled by the context window, and a masker
//! that moves old tool outputs to `<data_dir>/tool-outputs/` once they take a lot of context.

use std::{
    collections::HashMap,
    path::PathBuf,
    time::{Duration, SystemTime},
};

use sha2::{Digest, Sha256};

use crate::{
    domain::{Block, Message},
    engine::context::{text_tokens, tokens},
    session::History,
};

/// The window the fixed limits were tuned for.
const BASE_WINDOW: usize = 128_000;
const MAX_SCALE: f64 = 4.0;
/// Recent context never masked, scaled with the window.
const PROTECTED_TOKENS: usize = 50_000;
/// Old tool output worth a history rewrite.
const MIN_PRUNABLE_TOKENS: usize = 30_000;
/// Latest results dedupe never touches.
const PROTECT_RECENT_RESULTS: usize = 4;
const DEDUPE_MIN_BYTES: usize = 512;
const DEDUPE_MIN_SAVED_TOKENS: usize = 2_000;
const RETENTION: Duration = Duration::from_secs(7 * 24 * 3600);
/// Interactive and plan state the model must keep seeing.
const EXEMPT: &[&str] = &[
    "question",
    "ask_user",
    "todo",
    "plan_mode",
    "exit_plan_mode",
];
const MASKED_PREFIX: &str = "[tool output masked:";
const DEDUPED_PREFIX: &str = "[same content as the later result of call";

pub struct OutputPolicy {
    window: usize,
    dir: Option<PathBuf>,
}

impl Default for OutputPolicy {
    fn default() -> Self {
        Self::new(BASE_WINDOW, None)
    }
}

impl OutputPolicy {
    /// `dir` enables masking; `None` only caps results.
    pub fn new(window: usize, dir: Option<PathBuf>) -> Self {
        Self {
            window: window.max(8192),
            dir,
        }
    }

    fn scale(&self) -> f64 {
        (self.window as f64 / BASE_WINDOW as f64).max(1.0)
    }

    /// Token cap of one result of `name`.
    pub fn limit(&self, name: &str) -> usize {
        let base = match name {
            "read" | "grep" => 6000.0,
            "shell" | "web_fetch" | "web_search" => 3000.0,
            _ => 4000.0,
        };
        ((base * self.scale().min(MAX_SCALE)) as usize).min(self.window / 8)
    }

    /// `content` within the cap of `name`. Reads keep their head and a corrected
    /// `[next: offset=N]`; other output keeps head and tail.
    pub fn cap(&self, name: &str, content: String) -> String {
        let limit = self.limit(name);
        let estimate = text_tokens(&content);
        if estimate <= limit {
            return content;
        }
        let budget = ((content.len() as u128 * limit as u128 / estimate as u128) as usize)
            .saturating_sub(200)
            .max(256);
        let budget = content.floor_char_boundary(budget.min(content.len()));
        let notice = format!("[result cut to about {limit} tokens to fit the context]");
        if matches!(name, "read" | "grep" | "glob" | "repo_map") {
            let head = &content[..budget];
            let head = &head[..head.rfind('\n').map_or(head.len(), |end| end + 1)];
            let mut out = format!("{head}... {notice}");
            if name == "read"
                && let Some(last) = head.lines().rev().find_map(|line| {
                    line.split_once(": ")
                        .and_then(|(n, _)| n.parse::<usize>().ok())
                })
            {
                out.push_str(&format!("\n[next: offset={}]", last + 1));
            }
            return out;
        }
        let head_end = content.floor_char_boundary(budget * 2 / 5);
        let head = &content[..head_end];
        let head = &head[..head.rfind('\n').map_or(head.len(), |end| end + 1)];
        let tail_start = content.ceil_char_boundary(content.len() - (budget - head_end));
        let tail = &content[tail_start..];
        let tail = tail.find('\n').map_or(tail, |start| &tail[start + 1..]);
        let omitted = content.len() - head.len() - tail.len();
        format!("{head}[... {omitted} bytes omitted; {notice} ...]\n{tail}")
    }

    /// Masks old tool outputs and collapses repeated identical results; `Some(notice)`
    /// when `messages` changed.
    /// The history is only rewritten (a new journal epoch) when something is masked.
    pub fn mask(&self, history: &mut History) -> Option<String> {
        let dir = self.dir.as_ref()?;
        let messages: &[Message] = history;
        let names: HashMap<String, String> = messages
            .iter()
            .flat_map(|message| &message.blocks)
            .filter_map(|block| match block {
                Block::ToolUse { id, name, .. } => Some((id.clone(), name.clone())),
                _ => None,
            })
            .collect();
        let name_of = |id: &str| names.get(id).map_or("tool", String::as_str).to_owned();
        let exempt = |name: &str| {
            EXEMPT.contains(&name) || name.split("__").any(|part| EXEMPT.contains(&part))
        };
        let maskable = |content: &str| {
            content.len() > 100
                && !content.starts_with(MASKED_PREFIX)
                && !content.starts_with(DEDUPED_PREFIX)
        };
        // Where the protected recent window starts (message index).
        let threshold = (PROTECTED_TOKENS as f64 * self.scale()) as usize;
        let mut protected = 0;
        let mut cutoff = 0;
        for (index, message) in messages.iter().enumerate().rev() {
            protected += tokens(std::slice::from_ref(message));
            if protected >= threshold {
                cutoff = index;
                break;
            }
        }
        let prunable: usize = messages[..cutoff]
            .iter()
            .flat_map(|message| &message.blocks)
            .filter_map(|block| match block {
                Block::ToolResult { id, content, .. }
                    if maskable(content) && !exempt(&name_of(id)) =>
                {
                    Some(text_tokens(content))
                }
                _ => None,
            })
            .sum();
        let disk = prunable >= MIN_PRUNABLE_TOKENS;

        // Repeated identical results: earlier copies point at the latest one, which
        // compaction keeps longest, so a stub never outlives its content.
        let positions: Vec<(usize, usize)> = messages
            .iter()
            .enumerate()
            .flat_map(|(m, message)| {
                message
                    .blocks
                    .iter()
                    .enumerate()
                    .filter(|(_, block)| matches!(block, Block::ToolResult { .. }))
                    .map(move |(b, _)| (m, b))
            })
            .collect();
        let protect_from = positions.len().saturating_sub(PROTECT_RECENT_RESULTS);
        let mut latest: HashMap<[u8; 32], String> = HashMap::new();
        let mut stubs = Vec::new();
        let mut dedupe_saved = 0;
        for (rank, &(m, b)) in positions.iter().enumerate().rev() {
            let Block::ToolResult { id, content, .. } = &messages[m].blocks[b] else {
                continue;
            };
            if content.len() < DEDUPE_MIN_BYTES || !maskable(content) || exempt(&name_of(id)) {
                continue;
            }
            let sum: [u8; 32] = Sha256::digest(content.as_bytes()).into();
            match latest.get(&sum) {
                None => {
                    latest.insert(sum, id.clone());
                }
                Some(_) if rank >= protect_from => {}
                Some(holder) => {
                    let stub = format!(
                        "{DEDUPED_PREFIX} {holder} ({} bytes); omitted here]",
                        content.len()
                    );
                    dedupe_saved += text_tokens(content).saturating_sub(text_tokens(&stub));
                    stubs.push((m, b, stub));
                }
            }
        }
        let dedupe = !stubs.is_empty() && (disk || dedupe_saved >= DEDUPE_MIN_SAVED_TOKENS);
        if !disk && !dedupe {
            return None;
        }
        let messages = history.rewrite();
        let mut deduped = 0;
        if dedupe {
            for (m, b, stub) in stubs {
                if let Block::ToolResult { content, .. } = &mut messages[m].blocks[b] {
                    *content = stub;
                    deduped += 1;
                }
            }
        }
        let (mut masked, mut saved) = (0, 0);
        if disk && std::fs::create_dir_all(dir).is_ok() {
            for message in &mut messages[..cutoff] {
                for block in &mut message.blocks {
                    let Block::ToolResult { id, content, .. } = block else {
                        continue;
                    };
                    let name = name_of(id);
                    if !maskable(content) || exempt(&name) {
                        continue;
                    }
                    let sum = Sha256::digest(content.as_bytes());
                    let safe: String = name
                        .chars()
                        .map(|c| if c.is_ascii_alphanumeric() { c } else { '_' })
                        .collect();
                    let file = dir.join(format!(
                        "output_{}_{safe}.txt",
                        sum[..8]
                            .iter()
                            .map(|b| format!("{b:02x}"))
                            .collect::<String>()
                    ));
                    if std::fs::write(&file, content.as_bytes()).is_err() {
                        continue;
                    }
                    let estimate = text_tokens(content);
                    saved += estimate;
                    masked += 1;
                    *content = format!(
                        "{MASKED_PREFIX} {name} result, ~{estimate} tokens, saved to {}; view it with the shell if it is needed again]",
                        file.display()
                    );
                }
            }
        }
        (masked + deduped > 0).then(|| {
            format!(
                "Context: {masked} old tool outputs moved to disk (~{saved} tokens), {deduped} repeated results collapsed."
            )
        })
    }

    /// Deletes masked outputs older than seven days.
    pub fn prune(&self) {
        let Some(dir) = &self.dir else { return };
        let Ok(entries) = std::fs::read_dir(dir) else {
            return;
        };
        let now = SystemTime::now();
        for entry in entries.flatten() {
            let old = entry
                .metadata()
                .and_then(|meta| meta.modified())
                .ok()
                .and_then(|modified| now.duration_since(modified).ok())
                .is_some_and(|age| age > RETENTION);
            if old && entry.file_name().to_string_lossy().starts_with("output_") {
                let _ = std::fs::remove_file(entry.path());
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::Role;
    use serde_json::json;

    fn call(id: &str, name: &str) -> Message {
        Message {
            role: Role::Assistant,
            blocks: vec![Block::ToolUse {
                id: id.into(),
                name: name.into(),
                input: json!({}),
                extra: None,
            }],
        }
    }

    fn result(id: &str, content: String) -> Message {
        Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: id.into(),
                content,
                is_error: false,
            }],
        }
    }

    fn content(message: &Message) -> &str {
        match &message.blocks[0] {
            Block::ToolResult { content, .. } => content,
            _ => unreachable!(),
        }
    }

    #[test]
    fn limits_scale_with_the_window() {
        let base = OutputPolicy::default();
        assert_eq!(base.limit("read"), 6000);
        assert_eq!(base.limit("shell"), 3000);
        assert_eq!(base.limit("edit"), 4000);
        assert_eq!(OutputPolicy::new(1_000_000, None).limit("read"), 24_000);
        assert_eq!(OutputPolicy::new(16_000, None).limit("read"), 2000);
    }

    #[test]
    fn capped_reads_name_the_real_next_line() {
        let policy = OutputPolicy::new(16_000, None);
        let text: String = (1..=3000)
            .map(|n| format!("{n}: some line of code\n"))
            .collect();
        let out = policy.cap("read", format!("File: a.rs (3000 lines total)\n\n{text}"));
        assert!(text_tokens(&out) <= 2000, "{}", text_tokens(&out));
        let last = out.lines().rev().nth(2).unwrap();
        let shown: usize = last.split(':').next().unwrap().parse().unwrap();
        assert!(
            out.ends_with(&format!("[next: offset={}]", shown + 1)),
            "{out}"
        );
        let shell = policy.cap("shell", "x\n".repeat(20_000));
        assert!(shell.contains("bytes omitted") && shell.ends_with("x\n"));
    }

    #[test]
    fn old_outputs_go_to_disk_and_duplicates_point_at_the_latest() {
        let dir = tempfile::tempdir().unwrap();
        let policy = OutputPolicy::new(BASE_WINDOW, Some(dir.path().join("tool-outputs")));
        let big = |n: usize| format!("{n}{}", "data ".repeat(30_000));
        let mut messages = History::new();
        for n in 0..4 {
            messages.push(call(&format!("r{n}"), "read"));
            messages.push(result(&format!("r{n}"), big(n)));
        }
        messages.push(call("q", "question"));
        messages.push(result("q", "answer ".repeat(100)));
        for n in 0..6 {
            messages.push(call(&format!("g{n}"), "grep"));
            messages.push(result(&format!("g{n}"), "same ".repeat(200)));
        }
        let notice = policy.mask(&mut messages).unwrap();
        assert!(notice.contains("moved to disk"), "{notice}");
        assert!(content(&messages[1]).starts_with(MASKED_PREFIX));
        let file = content(&messages[1]).split("saved to ").nth(1).unwrap();
        let file = file.split("; view").next().unwrap();
        assert!(std::fs::read_to_string(file).unwrap().starts_with("0data"));
        assert_eq!(content(&messages[9]), "answer ".repeat(100));
        assert!(
            content(&messages[11]).starts_with(DEDUPED_PREFIX),
            "{}",
            content(&messages[11])
        );
        assert!(content(&messages[11]).contains("g5"));
        assert_eq!(content(&messages[21]), "same ".repeat(200));
        assert!(policy.mask(&mut messages).is_none());
    }
}
