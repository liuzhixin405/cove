//! Recent full tool outputs for `/expand`. The terminal shows a tool result's first
//! lines with a `#n` tag; the full text stays here, bounded in count and size.

use std::{
    collections::VecDeque,
    sync::{LazyLock, Mutex},
};

/// Outputs kept; older ones can no longer be expanded.
pub const KEEP: usize = 50;
/// Largest output kept per block.
const BLOCK_LIMIT: usize = 512 * 1024;
/// Lines shown inline before the rest is left to `/expand`.
pub const INLINE_LINES: usize = 12;

/// `verbose`: tool results are shown in full (still bounded by what is kept).
pub static VERBOSE: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);
/// Lines `/expand` shows without `all`.
pub const EXPAND_LINES: usize = 200;

pub struct Block {
    pub id: u64,
    pub name: String,
    pub failed: bool,
    pub output: String,
}

#[derive(Default)]
pub struct Blocks {
    inner: Mutex<(u64, VecDeque<Block>)>,
}

/// The process's terminal output store (one terminal per process).
pub static BLOCKS: LazyLock<Blocks> = LazyLock::new(Blocks::default);

impl Blocks {
    /// Stores an output and returns its tag number.
    pub fn add(&self, name: &str, failed: bool, output: &str) -> u64 {
        let Ok(mut guard) = self.inner.lock() else {
            return 0;
        };
        let (next, blocks) = &mut *guard;
        *next += 1;
        let mut output = output.to_owned();
        if output.len() > BLOCK_LIMIT {
            output.truncate(output.floor_char_boundary(BLOCK_LIMIT));
            output.push_str("\n[保存的输出超过 512 KiB，后面部分已截掉]");
        }
        blocks.push_back(Block {
            id: *next,
            name: name.to_owned(),
            failed,
            output,
        });
        while blocks.len() > KEEP {
            blocks.pop_front();
        }
        *next
    }

    /// `/expand [n] [all]` text.
    pub fn expand(&self, id: Option<u64>, all: bool) -> String {
        let Ok(guard) = self.inner.lock() else {
            return "工具输出暂时无法读取，请稍后再试 /expand\n".into();
        };
        let block = match id {
            None => guard.1.back(),
            Some(id) => guard.1.iter().find(|block| block.id == id),
        };
        let Some(block) = block else {
            return match id {
                None => {
                    "还没有可展开的工具输出；工具运行后，/expand 显示最近一次的完整输出。\n".into()
                }
                Some(id) => format!(
                    "没有 #{id}（只保留最近 {KEEP} 个工具输出）；/expand 不带编号显示最近一个。\n"
                ),
            };
        };
        let lines: Vec<&str> = block.output.lines().collect();
        let shown = if all {
            lines.len()
        } else {
            lines.len().min(EXPAND_LINES)
        };
        let mut out = format!(
            "#{} {} [{}] 共 {} 行\n",
            block.id,
            block.name,
            if block.failed { "failed" } else { "ok" },
            lines.len()
        );
        for line in &lines[..shown] {
            out.push_str(line);
            out.push('\n');
        }
        if shown < lines.len() {
            out.push_str(&format!(
                "[还有 {} 行；/expand {} all 显示全部]\n",
                lines.len() - shown,
                block.id
            ));
        }
        out
    }
}

/// Inline display of a finished tool: the first lines, the tag, and a pointer to
/// `/expand` when lines were left out.
pub fn inline(output: &str, id: u64) -> String {
    let lines: Vec<&str> = output.lines().collect();
    let shown = if VERBOSE.load(std::sync::atomic::Ordering::Relaxed) {
        usize::MAX
    } else {
        INLINE_LINES
    };
    let mut out = String::new();
    for (index, line) in lines.iter().take(shown).enumerate() {
        if index > 0 {
            out.push('\n');
        }
        out.push_str(line);
        if index == 0 {
            out.push_str(&format!("  #{id}"));
        }
    }
    if lines.is_empty() {
        out.push_str(&format!("#{id}"));
    }
    if lines.len() > shown {
        out.push_str(&format!(
            "\n  ... 还有 {} 行（/expand {id}）",
            lines.len() - shown
        ));
    }
    out
}

/// Parses `/expand` arguments: an optional number (with or without `#`) and `all`.
pub fn parse(args: &[&str]) -> Option<(Option<u64>, bool)> {
    let mut id = None;
    let mut all = false;
    for arg in args {
        match arg.trim_start_matches('#') {
            "all" => all = true,
            number => id = Some(number.parse().ok()?),
        }
    }
    Some((id, all))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keeps_the_latest_outputs_and_expands_by_tag() {
        let blocks = Blocks::default();
        for index in 0..60 {
            blocks.add("shell", false, &format!("out {index}"));
        }
        assert!(blocks.expand(Some(1), false).contains("没有 #1"));
        assert!(blocks.expand(Some(60), false).contains("out 59"));
        assert!(blocks.expand(None, false).starts_with("#60 shell [ok]"));
        let long: String = (0..300).map(|n| format!("line {n}\n")).collect();
        let id = blocks.add("read", true, &long);
        let shown = blocks.expand(Some(id), false);
        assert!(shown.contains("line 199\n") && !shown.contains("line 200\n"));
        assert!(shown.contains("还有 100 行"));
        assert!(blocks.expand(Some(id), true).contains("line 299\n"));
    }

    #[test]
    fn inline_tags_and_points_to_expand() {
        assert_eq!(inline("one\ntwo", 3), "one  #3\ntwo");
        let long: String = (0..20).map(|n| format!("l{n}\n")).collect();
        let text = inline(&long, 4);
        assert!(text.starts_with("l0  #4\n") && text.ends_with("还有 8 行（/expand 4）"));
        assert_eq!(parse(&["#5", "all"]), Some((Some(5), true)));
        assert_eq!(parse(&[]), Some((None, false)));
        assert_eq!(parse(&["x"]), None);
    }
}
