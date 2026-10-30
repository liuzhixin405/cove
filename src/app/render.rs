//! Terminal presentation for the interactive console: one titled row per tool call
//! with a short result under it, and model text with its markdown styled. Plain
//! (`-p`, piped) output keeps the machine-readable lines in `ui`.

use serde_json::Value;

const RESET: &str = "\x1b[0m";
const DIM: &str = "\x1b[2m";
const BOLD: &str = "\x1b[1m";
const RED: &str = "\x1b[31m";
const GREEN: &str = "\x1b[32m";
const CYAN: &str = "\x1b[36m";
const CODE: &str = "\x1b[38;5;180m";
const HEADING: &str = "\x1b[1;4m";

/// Longest tool detail shown in a title, in characters.
const DETAIL: usize = 90;

fn clip(text: &str, max: usize) -> String {
    let line = text.lines().next().unwrap_or_default().trim();
    if line.chars().count() <= max && !text.trim().contains('\n') {
        return line.to_owned();
    }
    let mut out: String = line.chars().take(max).collect();
    out.push('…');
    out
}

fn field<'a>(input: &'a Value, keys: &[&str]) -> Option<&'a str> {
    keys.iter()
        .find_map(|key| input.get(*key).and_then(Value::as_str))
        .filter(|value| !value.trim().is_empty())
}

/// The verb and the key argument of a tool call, e.g. `读取 src/main.rs`.
pub fn tool_title(name: &str, input: &Value) -> String {
    let path = || field(input, &["path", "file_path", "filePath"]);
    let (verb, detail) = match name {
        "read" => {
            let mut detail = path().unwrap_or_default().to_owned();
            let offset = input.get("offset").or(input.get("start_line"));
            if let Some(offset) = offset.and_then(Value::as_u64) {
                detail.push_str(&format!(":{offset}"));
            }
            ("读取", detail)
        }
        "write" => ("写入", path().unwrap_or_default().to_owned()),
        "edit" => ("编辑", path().unwrap_or_default().to_owned()),
        "glob" => (
            "查找文件",
            field(input, &["pattern"]).unwrap_or_default().to_owned(),
        ),
        "grep" => {
            let mut detail = format!("\"{}\"", field(input, &["pattern"]).unwrap_or_default());
            if let Some(scope) = field(input, &["path", "include"]) {
                detail.push_str(&format!(" · {scope}"));
            }
            ("搜索", detail)
        }
        "repo_map" => (
            "代码结构",
            field(input, &["query", "path"])
                .unwrap_or_default()
                .to_owned(),
        ),
        "shell" => (
            "运行",
            field(input, &["command"]).unwrap_or_default().to_owned(),
        ),
        "question" | "ask_user" => ("提问", String::new()),
        "todo" => ("更新待办", String::new()),
        "web_fetch" | "fetch" => (
            "获取网页",
            field(input, &["url"]).unwrap_or_default().to_owned(),
        ),
        "web_search" => (
            "搜索网页",
            field(input, &["query"]).unwrap_or_default().to_owned(),
        ),
        "task" | "agent" | "subagent" => (
            "子代理",
            field(input, &["description", "prompt", "task"])
                .unwrap_or_default()
                .to_owned(),
        ),
        _ => {
            // Other tools (MCP, extensions): the first short string argument.
            let detail = input
                .as_object()
                .and_then(|map| map.values().find_map(Value::as_str))
                .unwrap_or_default()
                .to_owned();
            return format!("{name} {}", clip(&detail, DETAIL))
                .trim_end()
                .to_owned();
        }
    };
    format!("{verb} {}", clip(&detail, DETAIL))
        .trim_end()
        .to_owned()
}

/// The `●` row that opens a tool call.
pub fn tool_started(title: &str) -> String {
    format!("{CYAN}●{RESET} {BOLD}{title}{RESET}\n")
}

/// One dim live-output row of a running command.
pub fn output_line(line: &str, stderr: bool) -> String {
    let color = if stderr { RED } else { DIM };
    format!("  {DIM}│{RESET} {color}{line}{RESET}\n")
}

/// Rows under a finished tool: what it found or did, then `#id` for `/expand`.
/// `streamed`: the command's output was already shown live.
pub fn tool_finished(name: &str, failed: bool, output: &str, id: u64, streamed: bool) -> String {
    let mut rows = summary(name, failed, output, streamed);
    if rows.is_empty() {
        rows.push(if failed {
            "失败".into()
        } else {
            "完成".into()
        });
    }
    let last = rows.len() - 1;
    let mut out = String::new();
    for (index, row) in rows.iter().enumerate() {
        let branch = if index == last { "└" } else { "│" };
        let color = if failed { RED } else { "" };
        out.push_str(&format!("  {DIM}{branch}{RESET} {color}{row}{RESET}"));
        if index == last {
            out.push_str(&format!(" {DIM}#{id}{RESET}"));
        }
        out.push('\n');
    }
    out
}

fn lines_of(text: &str) -> Vec<&str> {
    text.lines()
        .filter(|line| !line.trim().is_empty())
        .collect()
}

/// Up to `count` lines from the end of `text`.
fn tail(text: &str, count: usize) -> Vec<String> {
    let lines = lines_of(text);
    let skip = lines.len().saturating_sub(count);
    let mut out = Vec::new();
    if skip > 0 {
        out.push(format!("… 前面还有 {skip} 行"));
    }
    out.extend(lines[skip..].iter().map(|line| clip(line, 160)));
    out
}

fn summary(name: &str, failed: bool, output: &str, streamed: bool) -> Vec<String> {
    // The refusal the model is told about (engine::agent::denied), in the user's words.
    if let Some(rest) = output.strip_prefix("The user denied this call (") {
        let reason = rest
            .split_once("用户回复：")
            .and_then(|(_, reason)| reason.split_once("). Do not retry"))
            .map(|(reason, _)| reason.trim());
        return vec![match reason {
            Some(reason) if !reason.is_empty() => format!("已拒绝：{reason}（已告诉模型）"),
            _ => "已拒绝（已告诉模型）".into(),
        }];
    }
    if failed && name != "shell" {
        return tail(output, 4);
    }
    match name {
        "read" => {
            if let Some(range) = output
                .lines()
                .rev()
                .find_map(|line| line.split_once("[showing lines ").map(|(_, rest)| rest))
            {
                let range = range.trim_end_matches(']').replace(" of ", "，共 ");
                return vec![format!("第 {range} 行")];
            }
            vec![format!("{} 行", output.lines().count())]
        }
        "grep" => {
            if output.trim() == "No matches found" {
                return vec!["没有匹配".into()];
            }
            let rows: Vec<&str> = output
                .lines()
                .filter(|line| !line.starts_with("... ") && !line.trim().is_empty())
                .collect();
            let mut files: Vec<&str> = rows
                .iter()
                .filter_map(|row| row.split(':').next())
                .collect();
            files.sort_unstable();
            files.dedup();
            let more = if output.contains("\n... ") { "+" } else { "" };
            vec![format!(
                "{}{more} 处匹配，{} 个文件",
                rows.len(),
                files.len()
            )]
        }
        "glob" => {
            let rows = lines_of(output);
            if rows.is_empty() || output.trim() == "No files found" {
                return vec!["没有文件".into()];
            }
            vec![format!("{} 个文件", rows.len())]
        }
        "shell" => shell_summary(failed, output, streamed),
        "write" | "edit" | "todo" => vec![clip(output, 160)],
        _ => {
            let rows = lines_of(output);
            match rows.len() {
                0 => Vec::new(),
                1 => vec![clip(rows[0], 160)],
                count => vec![format!("{} … 共 {count} 行", clip(rows[0], 120))],
            }
        }
    }
}

/// `exit_code=Some(0) timed_out=false ...\nstdout:\n...\nstderr:\n...` as a status
/// row, with the last lines when they were not streamed or the command failed.
fn shell_summary(failed: bool, output: &str, streamed: bool) -> Vec<String> {
    let header = output.lines().next().unwrap_or_default();
    let Some(code) = header.strip_prefix("exit_code=") else {
        return tail(output, 4);
    };
    let code = code.split_whitespace().next().unwrap_or_default();
    let code = code
        .strip_prefix("Some(")
        .and_then(|code| code.strip_suffix(')'))
        .unwrap_or("无");
    let body = output
        .split_once('\n')
        .map(|(_, body)| body)
        .unwrap_or_default();
    let (stdout, stderr) = body
        .strip_prefix("stdout:\n")
        .unwrap_or(body)
        .split_once("\nstderr:\n")
        .unwrap_or((body, ""));
    let mut rows = Vec::new();
    if !streamed {
        let shown = if failed && !stderr.trim().is_empty() {
            stderr
        } else {
            stdout
        };
        rows.extend(tail(shown, if failed { 6 } else { 3 }));
    }
    rows.push(if header.contains("timed_out=true") {
        "✗ 超时".into()
    } else if failed {
        format!("✗ 退出码 {code}")
    } else {
        format!("{GREEN}✓{RESET} 退出码 {code}")
    });
    rows
}

/// The closing row of a task.
pub fn report(
    failed_tools: u32,
    tools: u32,
    model_calls: u32,
    tokens: u64,
    cost: Option<f64>,
    seconds: f64,
    verification: &str,
) -> String {
    let mut parts = vec![format!("{model_calls} 次模型调用")];
    if tools > 0 {
        parts.push(if failed_tools > 0 {
            format!("{tools} 个工具（{failed_tools} 个失败）")
        } else {
            format!("{tools} 个工具")
        });
    }
    parts.push(format!("{} tokens", short_count(tokens)));
    if let Some(cost) = cost {
        parts.push(format!("${cost:.2}"));
    }
    parts.push(duration(seconds));
    if !verification.is_empty() {
        parts.push(verification.to_owned());
    }
    format!(
        "\n{GREEN}✓{RESET} {DIM}完成 · {}{RESET}\n",
        parts.join(" · ")
    )
}

/// `52.1k`, `200k`, `1.2M`.
pub fn short_count(count: u64) -> String {
    let (value, unit) = match count {
        0..1000 => return count.to_string(),
        1000..1_000_000 => (count as f64 / 1000.0, "k"),
        _ => (count as f64 / 1_000_000.0, "M"),
    };
    let text = format!("{value:.1}");
    format!("{}{unit}", text.strip_suffix(".0").unwrap_or(&text))
}

pub fn duration(seconds: f64) -> String {
    if seconds < 60.0 {
        format!("{seconds:.0}s")
    } else {
        let whole = seconds as u64;
        format!("{}m{:02}s", whole / 60, whole % 60)
    }
}

/// Styles streamed markdown as it arrives: headings, bold, inline code, lists,
/// quotes, rules and fenced code. Only a line's opening marker or a lone `*` is
/// held back until the next characters decide it.
#[derive(Default)]
pub struct Markdown {
    pending: String,
    /// Something was written on the current line.
    mid_line: bool,
    fence: bool,
    bold: bool,
    code: bool,
    /// The style reapplied after inline code or bold ends (headings, code blocks).
    line_style: &'static str,
    /// Rows of a table being read; it is drawn aligned once it ends.
    table: Vec<String>,
}

impl Markdown {
    pub fn push(&mut self, text: &str) -> String {
        self.pending.push_str(text);
        // The terminal resets styles after every write, so a line continued from an
        // earlier push opens with the styles still in effect.
        let resume = if self.mid_line {
            self.active()
        } else {
            String::new()
        };
        let out = self.render(false);
        if out.is_empty() {
            return out;
        }
        resume + &out
    }

    /// The styles in effect at this point of the line.
    fn active(&self) -> String {
        let mut out = self.restyle();
        if self.code {
            out.push_str(CODE);
        }
        out
    }

    /// Writes what is held back and closes open styles.
    pub fn finish(&mut self) -> String {
        let mut out = self.render(true);
        if self.bold || self.code || !self.line_style.is_empty() {
            out.push_str(RESET);
        }
        self.bold = false;
        self.code = false;
        self.line_style = "";
        // What follows starts on a fresh row (the caller ends the line).
        self.mid_line = false;
        self.fence = false;
        out
    }

    fn render(&mut self, flush: bool) -> String {
        let mut out = String::new();
        loop {
            if self.pending.is_empty() {
                if flush && !self.table.is_empty() {
                    out.push_str(&table(&std::mem::take(&mut self.table)));
                }
                return out;
            }
            if !self.mid_line && !self.fence {
                let end = self.pending.find('\n');
                let line = &self.pending[..end.unwrap_or(self.pending.len())];
                if line.trim_start().starts_with('|') {
                    if end.is_none() && !flush {
                        return out;
                    }
                    let row = line.to_owned();
                    let taken = end.map_or(self.pending.len(), |end| end + 1);
                    self.pending.drain(..taken);
                    self.table.push(row);
                    continue;
                }
                // Any other line ends the table; leading spaces alone may still be a row.
                let decided = end.is_some() || flush || !line.trim().is_empty();
                if !self.table.is_empty() && decided {
                    out.push_str(&table(&std::mem::take(&mut self.table)));
                }
            }
            if !self.mid_line {
                match self.line_start(flush) {
                    Some(text) => out.push_str(&text),
                    None => return out,
                }
                continue;
            }
            let Some(c) = self.pending.chars().next() else {
                return out;
            };
            if c == '\n' {
                self.pending.remove(0);
                if self.bold || self.code || !self.line_style.is_empty() {
                    out.push_str(RESET);
                }
                self.bold = false;
                self.code = false;
                if !self.fence {
                    self.line_style = "";
                }
                out.push('\n');
                self.mid_line = false;
                continue;
            }
            if self.fence {
                let end = self.pending.find('\n').unwrap_or(self.pending.len());
                out.extend(self.pending.drain(..end));
                continue;
            }
            match c {
                '`' => {
                    self.pending.remove(0);
                    self.code = !self.code;
                    out.push_str(if self.code { CODE } else { RESET });
                    if !self.code {
                        out.push_str(&self.restyle());
                    }
                }
                '*' if !self.code => {
                    let mut chars = self.pending.chars();
                    chars.next();
                    match chars.next() {
                        None if !flush => return out,
                        Some('*') => {
                            self.pending.drain(..2);
                            self.bold = !self.bold;
                            out.push_str(if self.bold { BOLD } else { RESET });
                            if !self.bold {
                                out.push_str(&self.restyle());
                            }
                        }
                        _ => {
                            self.pending.remove(0);
                            out.push('*');
                        }
                    }
                }
                _ => {
                    let end = self
                        .pending
                        .find(['\n', '`', '*'])
                        .unwrap_or(self.pending.len())
                        .max(c.len_utf8());
                    out.extend(self.pending.drain(..end));
                }
            }
        }
    }

    fn restyle(&self) -> String {
        let mut out = self.line_style.to_owned();
        if self.bold {
            out.push_str(BOLD);
        }
        out
    }

    /// Decides the opening of a line; `None` while more text is needed.
    fn line_start(&mut self, flush: bool) -> Option<String> {
        let line_end = self.pending.find('\n');
        let line = &self.pending[..line_end.unwrap_or(self.pending.len())];
        let complete = line_end.is_some() || flush;
        let trimmed = line.trim_start();
        let indent = line.len() - trimmed.len();
        let maybe_fence = trimmed.is_empty() && self.fence || "```".starts_with(trimmed);
        if !complete && maybe_fence {
            return None;
        }
        if trimmed.starts_with("```") {
            // Fence lines are shown dim, whole.
            if !complete {
                return None;
            }
            let fence = line.to_owned();
            self.pending.drain(..line.len());
            self.fence = !self.fence;
            self.line_style = if self.fence { CODE } else { "" };
            self.mid_line = true;
            return Some(format!("{DIM}{fence}{RESET}"));
        }
        if self.fence {
            self.mid_line = true;
            return Some(CODE.to_owned());
        }
        // Too short to tell yet: `#`, `-`, `>`, a rule or a numbered item may follow.
        let undecided = trimmed.is_empty()
            || trimmed.len() < 4
                && trimmed.chars().all(|c| {
                    matches!(c, '#' | '-' | '*' | '_' | '+' | '>' | ' ') || c.is_ascii_digit()
                });
        if undecided && !complete {
            return None;
        }
        let rule = trimmed.len() >= 3
            && ["-", "*", "_"]
                .iter()
                .any(|mark| trimmed.chars().all(|c| c.to_string() == *mark || c == ' '));
        if rule && complete {
            self.pending.drain(..line.len());
            self.mid_line = true;
            return Some(format!("{DIM}{}{RESET}", "─".repeat(40)));
        }
        if !complete
            && ["---", "***", "___"]
                .iter()
                .any(|mark| mark.starts_with(trimmed) || trimmed.starts_with(mark))
            && trimmed.len() < 8
        {
            return None;
        }
        self.mid_line = true;
        let hashes = trimmed.chars().take_while(|c| *c == '#').count();
        if (1..=6).contains(&hashes) && trimmed[hashes..].starts_with(' ') {
            self.pending.drain(..indent + hashes + 1);
            self.line_style = if hashes <= 2 { HEADING } else { BOLD };
            return Some(self.line_style.to_owned());
        }
        for marker in ["- ", "* ", "+ "] {
            if trimmed.starts_with(marker) {
                self.pending.drain(..indent + 2);
                return Some(format!("{}{CYAN}•{RESET} ", " ".repeat(indent)));
            }
        }
        if let Some(rest) = trimmed.strip_prefix('>') {
            let skip = indent + 1 + usize::from(rest.starts_with(' '));
            self.pending.drain(..skip);
            self.line_style = DIM;
            return Some(format!("{DIM}│ "));
        }
        Some(String::new())
    }
}

/// A markdown table with its columns aligned: the header bold, `│` between
/// cells, `─┼─` under the header; inline code and bold keep their styles.
fn table(rows: &[String]) -> String {
    let separator = |cells: &[String]| {
        cells.iter().all(|cell| {
            let cell = cell.trim_matches(':');
            !cell.is_empty() && cell.chars().all(|c| c == '-')
        })
    };
    let parsed: Vec<Vec<String>> = rows
        .iter()
        .map(|row| {
            let row = row.trim();
            let row = row.strip_prefix('|').unwrap_or(row);
            let row = row.strip_suffix('|').unwrap_or(row);
            row.split('|').map(|cell| cell.trim().to_owned()).collect()
        })
        .collect();
    let header = parsed.len() > 1 && separator(&parsed[1]);
    let body: Vec<Vec<(String, usize)>> = parsed
        .iter()
        .enumerate()
        .filter(|(index, _)| !(header && *index == 1))
        .map(|(_, cells)| cells.iter().map(|cell| inline(cell)).collect())
        .collect();
    let columns = body.iter().map(Vec::len).max().unwrap_or(0);
    let widths: Vec<usize> = (0..columns)
        .map(|column| {
            body.iter()
                .filter_map(|row| row.get(column).map(|(_, width)| *width))
                .max()
                .unwrap_or(0)
        })
        .collect();
    let mut out = String::new();
    for (index, row) in body.iter().enumerate() {
        let bold = header && index == 0;
        let cells: Vec<String> = (0..columns)
            .map(|column| {
                let (text, width) = row.get(column).cloned().unwrap_or_default();
                let pad = " ".repeat(widths[column] - width);
                if bold {
                    format!("{BOLD}{text}{RESET}{pad}")
                } else {
                    format!("{text}{pad}")
                }
            })
            .collect();
        out.push_str(&format!("  {}\n", cells.join(&format!(" {DIM}│{RESET} "))));
        if bold {
            let rule: Vec<String> = widths.iter().map(|width| "─".repeat(*width)).collect();
            out.push_str(&format!("  {DIM}{}{RESET}\n", rule.join("─┼─")));
        }
    }
    out
}

/// A table cell with `code` and **bold** styled, and its width on screen.
fn inline(cell: &str) -> (String, usize) {
    use unicode_width::UnicodeWidthChar;
    let mut out = String::new();
    let mut width = 0;
    let (mut code, mut bold) = (false, false);
    let mut chars = cell.chars().peekable();
    while let Some(c) = chars.next() {
        match c {
            '`' => {
                code = !code;
                out.push_str(if code { CODE } else { RESET });
                if !code && bold {
                    out.push_str(BOLD);
                }
            }
            '*' if !code && chars.peek() == Some(&'*') => {
                chars.next();
                bold = !bold;
                out.push_str(if bold { BOLD } else { RESET });
            }
            c => {
                width += c.width().unwrap_or(0);
                out.push(c);
            }
        }
    }
    if code || bold {
        out.push_str(RESET);
    }
    (out, width)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn plain(text: &str) -> String {
        let mut out = String::new();
        let mut escape = false;
        for c in text.chars() {
            if c == '\x1b' {
                escape = true;
            } else if escape {
                escape = !c.is_ascii_alphabetic();
            } else {
                out.push(c);
            }
        }
        out
    }

    fn render(chunks: &[&str]) -> String {
        let mut markdown = Markdown::default();
        let mut out: String = chunks.iter().map(|chunk| markdown.push(chunk)).collect();
        out.push_str(&markdown.finish());
        out
    }

    #[test]
    fn markers_become_styles_across_chunk_boundaries() {
        let out = render(&[
            "## 方",
            "案\n- 先",
            "读 **代",
            "码*",
            "* 再改 `main",
            ".rs`\n",
        ]);
        assert_eq!(plain(&out), "方案\n• 先读 代码 再改 main.rs\n");
        assert!(out.contains(BOLD) && out.contains(CODE));
    }

    #[test]
    fn fenced_code_is_left_as_written() {
        let out = render(&["```rust\nlet a = *b ** c;\n", "```\n", "完成"]);
        assert_eq!(plain(&out), "```rust\nlet a = *b ** c;\n```\n完成");
    }

    #[test]
    fn plain_text_streams_without_waiting_for_the_line_end() {
        let mut markdown = Markdown::default();
        assert_eq!(plain(&markdown.push("好的，我来")), "好的，我来");
        assert_eq!(plain(&markdown.push("看看")), "看看");
        // A list marker needs the next character, a number the next two.
        assert_eq!(markdown.push("\n-"), "\n");
        assert_eq!(plain(&markdown.push(" 一")), "• 一");
        assert_eq!(
            plain(&markdown.push("\n---\n")),
            format!("\n{}\n", "─".repeat(40))
        );
        assert_eq!(plain(&markdown.push("1. 二")), "1. 二");
    }

    #[test]
    fn styles_carry_over_to_the_next_push() {
        let mut markdown = Markdown::default();
        markdown.push("见 `price-engine-");
        // The console reset the style after the last write; this one restores it.
        assert!(markdown.push("quote-binance` 目录").starts_with(CODE));
        markdown.push("\n## 二、");
        assert!(markdown.push("开始").starts_with(HEADING));
    }

    #[test]
    fn tables_are_aligned() {
        let out = render(&[
            "| 链路 | 从哪里看起 |\n|---|---|\n| 行",
            "情 | `a` → b |\n| 保证金 | c |\n\n",
            "完",
        ]);
        assert_eq!(
            plain(&out),
            "  链路   │ 从哪里看起\n  ───────┼───────────\n  行情   │ a → b     \n  保证金 │ c         \n\n完"
        );
    }

    #[test]
    fn a_lone_star_is_kept() {
        assert_eq!(plain(&render(&["2 * 3 = 6"])), "2 * 3 = 6");
        assert_eq!(plain(&render(&["末尾*"])), "末尾*");
    }

    #[test]
    fn tool_titles_name_the_key_argument() {
        let input = serde_json::json!({"path": "src/main.rs", "offset": 40});
        assert_eq!(tool_title("read", &input), "读取 src/main.rs:40");
        let input = serde_json::json!({"pattern": "fn main", "include": "*.rs"});
        assert_eq!(tool_title("grep", &input), "搜索 \"fn main\" · *.rs");
        let input = serde_json::json!({"command": "cargo test\ncargo build"});
        assert_eq!(tool_title("shell", &input), "运行 cargo test…");
        assert_eq!(tool_title("todo", &serde_json::json!({})), "更新待办");
    }

    #[test]
    fn results_are_summarized() {
        let grep = "src/a.rs:1:fn main\nsrc/a.rs:9:fn main2\nsrc/b.rs:3:fn main";
        assert_eq!(summary("grep", false, grep, false), ["3 处匹配，2 个文件"]);
        let shell = "exit_code=Some(1) timed_out=false truncated=false encoding_warning=false\nstdout:\nok\nstderr:\nerror: boom\n";
        let rows = summary("shell", true, shell, false);
        assert_eq!(rows, ["error: boom", "✗ 退出码 1"]);
        let rows = summary("shell", false, &shell.replace("Some(1)", "Some(0)"), true);
        assert_eq!(plain(&rows.join("|")), "✓ 退出码 0");
        let read = "1\tuse a;\n2\tuse b;\n... [showing lines 1-2 of 90]\n[next: offset=3]";
        assert_eq!(summary("read", false, read, false), ["第 1-2，共 90 行"]);
        let refused = "The user denied this call (运行 shell 命令（无沙箱）：curl x；用户回复：不要上传). Do not retry it.";
        assert_eq!(
            summary("shell", true, refused, false),
            ["已拒绝：不要上传（已告诉模型）"]
        );
    }
}
