//! The console's history views: a panel listing sessions below the input, and a
//! full-screen reader of one session (its requests above, the selected turn's
//! conclusion below). Pure state and rendering; the console owns the terminal
//! (alternate screen, mouse capture) and routes keys and mouse events here.
//!
//! Data comes from `session::SessionSummary`, so turn counts, titles and times are
//! the same as everywhere else history is shown.

use std::cell::Cell;

use crossterm::event::{KeyCode, KeyEvent, KeyModifiers, MouseButton, MouseEvent, MouseEventKind};

use crate::session::{SessionSummary, TurnRecord, time};

const DIM: &str = "2";
const BORDER: &str = "90";
const USER: &str = "1;96";
const WARN: &str = "93";
const HIT: &str = "4;93";
const TITLE: &str = "1";
const SELECTED_BG: &str = "48;5;238";
const TITLE_BG: &str = "48;5;236";
const FLASH: &str = "1;92";
/// Turn inputs shown before the reader says `…（提问共 N 行）`.
const INPUT_LINES: usize = 3;

/// One session in the panel and the line that choosing it submits.
pub struct HistoryEntry {
    pub summary: SessionSummary,
    pub command: String,
    /// The session in use (listed by `/continue` while its task is unfinished).
    pub current: bool,
}

/// What a key in the session list asks the console to do.
#[derive(Debug, PartialEq, Eq)]
pub enum ListAction {
    None,
    /// Not a list key: the console edits the filter (the input line) with it.
    Edit,
    Close,
    /// Submit this line as if typed.
    Submit(String),
    /// Close and leave this in the input line to finish (`/resume rename <id> `).
    Prefill(String),
}

/// What a key in the reader asks the console to do.
#[derive(Debug, PartialEq, Eq)]
pub enum ReaderAction {
    None,
    /// Back to the list, or closed when the reader was opened alone (`/turns`).
    Leave,
    Submit(String),
}

pub struct HistoryView {
    title: String,
    note: Option<String>,
    entries: Vec<HistoryEntry>,
    show_cwd: bool,
    /// Ctrl+A submits this to list the other scope (`/resume all`, `/resume`).
    other_scope: Option<String>,
    selected: usize,
    reader: Option<Reader>,
    /// The reader was opened without a list (`/turns`): leaving it closes the view.
    reader_only: bool,
    flash: Option<String>,
    color: bool,
}

struct Reader {
    entry: usize,
    /// Position in the (filtered) list of inputs.
    selected: usize,
    query: String,
    filtering: bool,
    scroll: usize,
    selection: Option<Selection>,
    layout: Cell<Layout>,
}

#[derive(Clone, Copy, Default)]
struct Layout {
    list_top: usize,
    list_start: usize,
    list_rows: usize,
    answer_top: usize,
    page: usize,
    max_scroll: usize,
    inner: usize,
}

/// A mouse selection in the answer, as (answer line, display column).
#[derive(Clone, Copy, Debug)]
struct Selection {
    anchor: (usize, usize),
    head: (usize, usize),
    dragging: bool,
}

impl Selection {
    fn ordered(&self) -> ((usize, usize), (usize, usize)) {
        if self.anchor <= self.head {
            (self.anchor, self.head)
        } else {
            (self.head, self.anchor)
        }
    }

    /// The display columns `[from, to)` selected on `line`.
    fn columns(&self, line: usize) -> Option<(usize, usize)> {
        let ((start_line, start_col), (end_line, end_col)) = self.ordered();
        if line < start_line || line > end_line {
            return None;
        }
        let from = if line == start_line { start_col } else { 0 };
        let to = if line == end_line {
            end_col + 1
        } else {
            usize::MAX
        };
        Some((from, to))
    }
}

impl HistoryView {
    pub fn new(title: &str, entries: Vec<HistoryEntry>, show_cwd: bool, color: bool) -> Self {
        Self {
            title: title.to_owned(),
            note: None,
            entries,
            show_cwd,
            other_scope: None,
            selected: 0,
            reader: None,
            reader_only: false,
            flash: None,
            color,
        }
    }

    /// A dim line under the list (`3 个空会话未列出`).
    pub fn with_note(mut self, note: Option<String>) -> Self {
        self.note = note;
        self
    }

    /// What Ctrl+A submits to switch between this workspace and all of them.
    pub fn with_other_scope(mut self, command: &str) -> Self {
        self.other_scope = Some(command.to_owned());
        self
    }

    /// Opens straight into the reader on the last turn (`/turns`).
    pub fn reading_alone(mut self) -> Self {
        if !self.entries.is_empty() {
            self.open_reader(0, None);
            self.reader_only = true;
        }
        self
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub fn reading(&self) -> bool {
        self.reader.is_some()
    }

    /// A session got a new title (from the model, in the background).
    pub fn retitle(&mut self, id: &str, title: &str) {
        for entry in &mut self.entries {
            if entry.summary.id == id {
                entry.summary.title = title.to_owned();
            }
        }
    }

    /// The typed filter changed: the selection starts over.
    pub fn filter_changed(&mut self) {
        self.selected = 0;
    }

    /// The terminal was resized: a selection's columns no longer match.
    pub fn resized(&mut self) {
        if let Some(reader) = &mut self.reader {
            reader.selection = None;
        }
    }

    fn paint(&self, code: &str, text: &str) -> String {
        if !self.color || code.is_empty() || text.is_empty() {
            text.to_owned()
        } else {
            format!("\x1b[{code}m{text}\x1b[0m")
        }
    }

    // ------------------------------------------------------------ list

    /// (entry, the turn the filter matched when not the title).
    fn matches(&self, query: &str) -> Vec<(usize, Option<usize>)> {
        let query = query.trim().to_lowercase();
        self.entries
            .iter()
            .enumerate()
            .filter_map(|(index, entry)| {
                if query.is_empty() || entry.summary.title.to_lowercase().contains(&query) {
                    return Some((index, None));
                }
                entry
                    .summary
                    .turns
                    .iter()
                    .position(|turn| turn.input.to_lowercase().contains(&query))
                    .map(|turn| (index, Some(turn)))
            })
            .collect()
    }

    /// The panel below the input: at most `max_rows` rows of `columns` - 1 width.
    pub fn list_rows(&self, query: &str, columns: usize, max_rows: usize) -> Vec<String> {
        let frame = Frame {
            total: columns.saturating_sub(1).clamp(24, 140),
        };
        let inner = frame.inner();
        let matches = self.matches(query);
        let right = if query.trim().is_empty() {
            "输入文字筛选".to_owned()
        } else {
            format!("筛选：{} · {} 条", query.trim(), matches.len())
        };
        let mut rows = vec![self.frame_top(&frame, &self.title, &right)];
        if matches.is_empty() {
            rows.push(self.frame_row(
                &frame,
                &Line::new().push(DIM, "没有匹配的会话（Backspace 修改筛选，Esc 关闭）"),
                None,
            ));
            rows.push(self.frame_bottom(&frame, "Esc 关闭", ""));
            return rows;
        }
        let selected = self.selected.min(matches.len() - 1);
        // Without room for the preview, the list keeps what room there is.
        let preview = max_rows >= 14;
        let fixed = 2 + usize::from(self.note.is_some()) + if preview { 6 } else { 0 };
        let room = max_rows.saturating_sub(fixed).clamp(1, 14);
        let mut flat: Vec<Option<usize>> = Vec::new();
        let mut last_group = "";
        let mut position = 0;
        for (offset, &(index, _)) in matches.iter().enumerate() {
            let group = group_of(&self.entries[index]);
            if group != last_group {
                flat.push(None);
                last_group = group;
            }
            if offset == selected {
                position = flat.len();
            }
            flat.push(Some(offset));
        }
        let shown = flat.len().min(room);
        let start = position.saturating_sub(shown / 2).min(flat.len() - shown);
        let mut groups = matches
            .iter()
            .map(|&(index, _)| group_of(&self.entries[index]));
        let mut headers: Vec<&str> = Vec::new();
        let mut previous = "";
        for group in groups.by_ref() {
            if group != previous {
                headers.push(group);
                previous = group;
            }
        }
        let mut header = 0;
        for (row_index, row) in flat.iter().enumerate() {
            if row.is_none() {
                header += 1;
            }
            if row_index < start || row_index >= start + shown {
                continue;
            }
            match row {
                None => rows.push(self.frame_row(
                    &frame,
                    &Line::new().push(DIM, headers[header - 1]),
                    None,
                )),
                Some(offset) => {
                    let (index, _) = matches[*offset];
                    let line =
                        self.entry_line(&self.entries[index], query, *offset == selected, inner);
                    rows.push(self.frame_row(
                        &frame,
                        &line,
                        (*offset == selected).then_some(SELECTED_BG),
                    ));
                }
            }
        }
        if let Some(note) = &self.note {
            rows.push(self.frame_row(&frame, &Line::new().push(DIM, note.as_str()), None));
        }
        if preview {
            let (index, hit) = matches[selected];
            rows.push(self.frame_sep(&frame, "预览"));
            for line in self.preview(&self.entries[index], hit, query, inner) {
                rows.push(self.frame_row(&frame, &line, None));
            }
        }
        let mut hint = "↑↓ 选择 · Enter 继续 · Tab 查看对话".to_owned();
        if query.trim().is_empty() {
            hint.push_str(" · 1-9 直接选");
        }
        if self.other_scope.is_some() {
            hint.push_str(if self.show_cwd {
                " · Ctrl+A 本工作区"
            } else {
                " · Ctrl+A 全部工作区"
            });
        }
        hint.push_str(" · Ctrl+R 改名 · Esc 关闭");
        rows.push(self.frame_bottom(
            &frame,
            &hint,
            &format!("{}/{}", selected + 1, matches.len()),
        ));
        rows
    }

    fn entry_line(&self, entry: &HistoryEntry, query: &str, selected: bool, inner: usize) -> Line {
        let summary = &entry.summary;
        let mut left = Line::new().push(
            if selected { USER } else { BORDER },
            if selected { "▌ " } else { "  " },
        );
        left = left.push(WARN, if summary.unfinished { "● " } else { "  " });
        left.0
            .extend(highlight(&summary.title, if selected { TITLE } else { "" }, query).0);
        let mut right = Line::new();
        if self.show_cwd {
            right = right.push(
                DIM,
                pad_right(&crate::text::clip_width(&summary.workspace_name(), 16), 18),
            );
        }
        let state = match (entry.current, summary.interrupted, summary.unfinished) {
            (true, _, _) => "当前",
            (_, true, _) => "已中断",
            (_, _, true) => "未完成",
            _ => "",
        };
        right = right
            .push(WARN, pad_right(state, 8))
            .push(DIM, format!("{:>3} 轮", summary.turns.len()))
            .push(DIM, pad_left(&time::relative(summary.updated), 14));
        spread(left, right, inner)
    }

    /// Earlier inputs, the latest one and the first sentence of its conclusion.
    fn preview(
        &self,
        entry: &HistoryEntry,
        hit: Option<usize>,
        query: &str,
        inner: usize,
    ) -> Vec<Line> {
        let turns = &entry.summary.turns;
        let label = |text: &str| Line::new().push(DIM, pad_right(text, 10));
        let mut rows: Vec<Line> = Vec::new();
        let Some(last_index) = turns.len().checked_sub(1) else {
            return vec![Line::new(); 5];
        };
        if let Some(turn) = hit.filter(|turn| *turn != last_index) {
            let mut found = label(&format!("命中 #{}", turn + 1));
            found.0.extend(
                highlight(
                    &snippet(&one_line(&turns[turn].input), query, inner / 2),
                    USER,
                    query,
                )
                .0,
            );
            rows.push(found);
        }
        let earlier: Vec<usize> = (0..last_index)
            .rev()
            .filter(|turn| Some(*turn) != hit)
            .take(2 - rows.len())
            .collect();
        for (offset, &turn) in earlier.iter().rev().enumerate() {
            rows.push(
                label(if offset == 0 { "之前" } else { "" })
                    .push(DIM, format!("#{} ", turn + 1))
                    .push(USER, one_line(&turns[turn].input)),
            );
        }
        while rows.len() < 2 {
            rows.insert(0, Line::new());
        }
        let last = &turns[last_index];
        rows.push(
            label("最近一问")
                .push(DIM, format!("#{} ", last_index + 1))
                .push(USER, one_line(&last.input)),
        );
        rows.push(match first_sentence(&last.reply) {
            Some(sentence) => label("最近结论").push("", sentence),
            None => label("最近结论").push(DIM, "（没有文字回复）"),
        });
        let summary = &entry.summary;
        let mut meta = vec![summary.model.rsplit('.').next().unwrap_or("").to_owned()];
        if summary.lines_added + summary.lines_removed > 0 {
            meta.push(format!(
                "+{} −{} 行",
                summary.lines_added, summary.lines_removed
            ));
        }
        if let Some(cost) = summary.cost {
            meta.push(format!("${cost:.2}"));
        }
        if summary.compacted {
            meta.push("压缩过".into());
        }
        meta.push(format!("#{}", &summary.id[..summary.id.len().min(8)]));
        rows.push(Line::new().push(DIM, meta.join(" · ")));
        rows
    }

    pub fn list_key(&mut self, key: KeyEvent, query: &str) -> ListAction {
        let control = key.modifiers.contains(KeyModifiers::CONTROL);
        let matches = self.matches(query);
        let count = matches.len();
        let chosen = matches
            .get(self.selected.min(count.saturating_sub(1)))
            .map(|&(index, hit)| (index, hit));
        match key.code {
            KeyCode::Up => self.selected = self.selected.saturating_sub(1),
            KeyCode::Down => self.selected = (self.selected + 1).min(count.saturating_sub(1)),
            KeyCode::PageUp => self.selected = self.selected.saturating_sub(8),
            KeyCode::PageDown => self.selected = (self.selected + 8).min(count.saturating_sub(1)),
            KeyCode::Home => self.selected = 0,
            KeyCode::End => self.selected = count.saturating_sub(1),
            KeyCode::Esc => return ListAction::Close,
            KeyCode::Char('c') if control => return ListAction::Close,
            KeyCode::Enter => {
                return match chosen {
                    Some((index, _)) => ListAction::Submit(self.entries[index].command.clone()),
                    None => ListAction::None,
                };
            }
            KeyCode::Tab | KeyCode::Right => {
                if let Some((index, hit)) = chosen {
                    self.open_reader(index, hit);
                }
            }
            KeyCode::Char('a') if control => {
                if let Some(command) = &self.other_scope {
                    return ListAction::Submit(command.clone());
                }
            }
            KeyCode::Char('r') if control => {
                if let Some((index, _)) = chosen {
                    let id = &self.entries[index].summary.id;
                    return ListAction::Prefill(format!(
                        "/resume rename {} ",
                        &id[..id.len().min(8)]
                    ));
                }
            }
            KeyCode::Char(digit @ '1'..='9') if !control && query.trim().is_empty() => {
                let index = usize::from(digit as u8 - b'1');
                if let Some(&(entry, _)) = matches.get(index) {
                    return ListAction::Submit(self.entries[entry].command.clone());
                }
            }
            _ => return ListAction::Edit,
        }
        ListAction::None
    }

    fn open_reader(&mut self, entry: usize, hit: Option<usize>) {
        let turns = self.entries[entry].summary.turns.len();
        self.reader = Some(Reader {
            entry,
            selected: hit.unwrap_or(turns.saturating_sub(1)),
            query: String::new(),
            filtering: false,
            scroll: 0,
            selection: None,
            layout: Cell::default(),
        });
    }

    // ------------------------------------------------------------ reader

    fn turn_matches(&self, reader: &Reader) -> Vec<usize> {
        let query = reader.query.to_lowercase();
        self.entries[reader.entry]
            .summary
            .turns
            .iter()
            .enumerate()
            .filter(|(_, turn)| query.is_empty() || turn.input.to_lowercase().contains(&query))
            .map(|(index, _)| index)
            .collect()
    }

    /// The reader's screen: exactly `rows` lines of `columns` - 1 width.
    pub fn reader_lines(&self, columns: usize, rows: usize) -> Vec<String> {
        let Some(reader) = &self.reader else {
            return Vec::new();
        };
        let entry = &self.entries[reader.entry];
        let summary = &entry.summary;
        let inner = columns.saturating_sub(1).max(20);
        let matches = self.turn_matches(reader);
        let selected = reader.selected.min(matches.len().saturating_sub(1));
        let mut layout = Layout {
            inner,
            ..Layout::default()
        };
        let mut lines = Vec::new();
        let right = if reader.filtering {
            format!(
                "筛选：{}▏ {} 条（Enter 完成 · Esc 取消） ",
                reader.query,
                matches.len()
            )
        } else if !reader.query.is_empty() {
            format!("筛选：{} · {} 条 ", reader.query, matches.len())
        } else {
            format!(
                "{} 轮 · {} ",
                summary.turns.len(),
                time::relative(summary.updated)
            )
        };
        let title = spread(
            Line::new().push(TITLE, format!(" {}", summary.title)),
            Line::new().push(DIM, right),
            inner,
        );
        lines.push(self.render_line(&title, inner, Some(TITLE_BG)));
        lines.push(self.rule(&format!("你的输入 · {} 条", matches.len()), "", inner));
        if matches.is_empty() {
            lines.push(self.render_line(
                &Line::new().push(DIM, "  没有匹配的输入（Backspace 修改，Esc 清空）"),
                inner,
                None,
            ));
        } else {
            let shown = matches.len().min((rows / 3).clamp(3, 10));
            let start = selected
                .saturating_sub(shown / 2)
                .min(matches.len() - shown);
            layout.list_top = lines.len();
            layout.list_start = start;
            layout.list_rows = shown;
            for (offset, &index) in matches.iter().enumerate().skip(start).take(shown) {
                let turn = &summary.turns[index];
                let is_selected = offset == selected;
                let mut left = Line::new()
                    .push(
                        if is_selected { USER } else { BORDER },
                        if is_selected { "▌" } else { " " },
                    )
                    .push(DIM, format!("{:>3}  ", index + 1));
                if turn.steer {
                    left = left.push(WARN, "插话 ");
                }
                left.0
                    .extend(highlight(&one_line(&turn.input), USER, &reader.query).0);
                let mut tags = Vec::new();
                if turn.images > 0 {
                    tags.push(format!("图片 {}", turn.images));
                }
                if turn.archived {
                    tags.push("已压缩".to_owned());
                }
                let line = spread(
                    left,
                    Line::new().push(DIM, format!("{} ", tags.join(" · "))),
                    inner,
                );
                lines.push(self.render_line(&line, inner, is_selected.then_some(SELECTED_BG)));
            }
        }
        let answer = matches
            .get(selected)
            .map(|&turn| self.answer_lines(&summary.turns[turn], inner))
            .unwrap_or_default();
        let page = rows.saturating_sub(lines.len() + 2).max(3);
        let max_scroll = answer.len().saturating_sub(page);
        let scroll = reader.scroll.min(max_scroll);
        let label = matches.get(selected).map_or(String::new(), |&turn| {
            format!("第 {}/{} 轮的回答", turn + 1, summary.turns.len())
        });
        let range = if answer.len() > page {
            format!(
                "{}-{} / {} 行",
                scroll + 1,
                (scroll + page).min(answer.len()),
                answer.len()
            )
        } else {
            String::new()
        };
        lines.push(self.rule(&label, &range, inner));
        layout.answer_top = lines.len();
        for (offset, line) in answer.iter().enumerate().skip(scroll).take(page) {
            match reader
                .selection
                .and_then(|selection| selection.columns(offset))
            {
                Some((from, to)) => lines.push(reverse_columns(line, from, to)),
                None => lines.push(line.clone()),
            }
        }
        while lines.len() + 1 < rows {
            lines.push(String::new());
        }
        let hint = match &self.flash {
            Some(flash) => self.paint(FLASH, &crate::text::clip_width(&format!(" {flash}"), inner)),
            None => self.paint(
                DIM,
                &crate::text::clip_width(
                    " ↑↓/单击 选择对话 · 滚轮/PgUp/PgDn 滚动 · 拖动选中即复制（可跨屏）· c 复制整段 · o 在编辑器打开 · / 筛选 · Enter 继续此会话 · Esc 返回",
                    inner,
                ),
            ),
        };
        lines.push(hint);
        layout.page = page;
        layout.max_scroll = max_scroll;
        reader.layout.set(layout);
        lines
    }

    /// The request (as the console echoes input, at most a few lines) and the
    /// conclusion rendered like a reply, wrapped to `inner` keeping styles.
    fn answer_lines(&self, turn: &TurnRecord, inner: usize) -> Vec<String> {
        let mut out = String::new();
        let input: Vec<&str> = turn.input.trim_end().lines().collect();
        for (index, row) in input.iter().take(INPUT_LINES).enumerate() {
            let lead = if index == 0 { ">" } else { " " };
            out.push_str(&format!(
                "{} {}\n",
                self.paint("1;36", lead),
                self.paint("1;96;48;5;237", &format!(" {} ", visible(row)))
            ));
        }
        if input.len() > INPUT_LINES {
            out.push_str(&self.paint(DIM, &format!("  …（提问共 {} 行）\n", input.len())));
        }
        if turn.images > 0 {
            out.push_str(&self.paint(DIM, &format!("  [附带 {} 张图片]\n", turn.images)));
        }
        out.push('\n');
        if turn.reply.is_empty() {
            out.push_str(&self.paint(DIM, "（这一轮没有文字回复）"));
        } else {
            let mut markdown = crate::app::render::Markdown::default();
            out.push_str(&self.paint("1;37", "●"));
            out.push(' ');
            out.push_str(&markdown.push(&visible(&turn.reply)));
            out.push_str(&markdown.finish());
        }
        let mut lines = Vec::new();
        for line in out.split('\n') {
            if line.is_empty() {
                lines.push(String::new());
            } else {
                let line = if self.color {
                    line.to_owned()
                } else {
                    plain(line)
                };
                lines.extend(
                    crate::app::render::wrap_cell(&line, inner)
                        .into_iter()
                        .map(|(text, _)| text),
                );
            }
        }
        while lines.last().is_some_and(String::is_empty) {
            lines.pop();
        }
        lines
    }

    fn current_turn(&self) -> Option<(&Reader, usize)> {
        let reader = self.reader.as_ref()?;
        let matches = self.turn_matches(reader);
        let turn = *matches.get(reader.selected.min(matches.len().saturating_sub(1)))?;
        Some((reader, turn))
    }

    pub fn reader_key(&mut self, key: KeyEvent) -> ReaderAction {
        self.flash = None;
        let control = key.modifiers.contains(KeyModifiers::CONTROL);
        if control && matches!(key.code, KeyCode::Char('c')) {
            // Ctrl+C copies a selection, as terminals do; without one it leaves.
            if let Some(text) = self.selection_text() {
                self.flash = Some(copy_text(&text, "选中内容"));
                return ReaderAction::None;
            }
            return self.leave();
        }
        let current = self.current_turn().map(|(_, turn)| turn);
        let entry_command = self
            .reader
            .as_ref()
            .map(|reader| self.entries[reader.entry].command.clone());
        let Some(reader) = self.reader.as_mut() else {
            return ReaderAction::None;
        };
        let layout = reader.layout.get();
        let step = layout.page.saturating_sub(1).max(1);
        let before = (reader.selected, reader.query.clone());
        match key.code {
            KeyCode::Up => {
                reader.selected = reader.selected.saturating_sub(1);
                reader.scroll = 0;
            }
            KeyCode::Down => {
                reader.selected += 1;
                reader.scroll = 0;
            }
            KeyCode::PageUp => {
                reader.scroll = reader.scroll.min(layout.max_scroll).saturating_sub(step)
            }
            KeyCode::PageDown => reader.scroll = (reader.scroll + step).min(layout.max_scroll),
            KeyCode::Home => reader.scroll = 0,
            KeyCode::End => reader.scroll = layout.max_scroll,
            KeyCode::Enter if reader.filtering => reader.filtering = false,
            KeyCode::Esc if reader.filtering => {
                reader.query.clear();
                reader.filtering = false;
            }
            KeyCode::Backspace if reader.filtering => {
                reader.query.pop();
            }
            KeyCode::Char(c) if reader.filtering && !control => {
                reader.query.push(c);
                reader.selected = 0;
                reader.scroll = 0;
            }
            KeyCode::Char(' ') => reader.scroll = (reader.scroll + step).min(layout.max_scroll),
            KeyCode::Char('/') => reader.filtering = true,
            KeyCode::Char('c' | 'C') => {
                let flash = current.map(|turn| {
                    let text = &self.entries[reader.entry].summary.turns[turn].reply;
                    copy_text(text, "回答（Markdown 原文）")
                });
                self.flash = flash;
                return ReaderAction::None;
            }
            KeyCode::Char('o' | 'O') => {
                let flash =
                    current.map(|turn| open_in_editor(&self.entries[reader.entry].summary, turn));
                self.flash = flash;
                return ReaderAction::None;
            }
            // Enter continues this session; `/turns` reads the session in use.
            KeyCode::Enter => {
                if let Some(command) = entry_command.filter(|command| !command.is_empty()) {
                    return ReaderAction::Submit(command);
                }
            }
            KeyCode::Esc if reader.selection.is_some() => reader.selection = None,
            KeyCode::Esc if !reader.query.is_empty() => reader.query.clear(),
            KeyCode::Esc | KeyCode::Left => return self.leave(),
            _ => {}
        }
        // A filter change keeps the turn that was selected when it is still listed.
        if before.1 != reader.query {
            let query = reader.query.to_lowercase();
            let turns = &self.entries[reader.entry].summary.turns;
            let listed: Vec<usize> = (0..turns.len())
                .filter(|index| {
                    query.is_empty() || turns[*index].input.to_lowercase().contains(&query)
                })
                .collect();
            reader.selected = current
                .and_then(|turn| listed.iter().position(|index| *index == turn))
                .unwrap_or(0);
        }
        let count = self.entries[reader.entry].summary.turns.len();
        reader.selected = reader.selected.min(count.saturating_sub(1));
        if before.0 != reader.selected || before.1 != reader.query {
            reader.selection = None;
        }
        ReaderAction::None
    }

    fn leave(&mut self) -> ReaderAction {
        self.reader = None;
        ReaderAction::Leave
    }

    /// Whether leaving the reader closes the whole view (`/turns`).
    pub fn reader_only(&self) -> bool {
        self.reader_only
    }

    /// Mouse in the reader: the wheel scrolls the answer, a click on an input
    /// selects that turn, and a drag in the answer selects text (auto-scrolling at
    /// the edges, so across screens); releasing copies it.
    pub fn mouse(&mut self, event: MouseEvent) {
        let (row, column) = (usize::from(event.row), usize::from(event.column));
        let Some(reader) = self.reader.as_mut() else {
            return;
        };
        let layout = reader.layout.get();
        let bottom = layout.answer_top + layout.page.saturating_sub(1);
        let point = |row: usize, scroll: usize| {
            let row = row.clamp(layout.answer_top, bottom);
            (scroll + row - layout.answer_top, column)
        };
        let in_answer = row >= layout.answer_top && row <= bottom;
        match event.kind {
            MouseEventKind::ScrollUp | MouseEventKind::ScrollDown => {
                reader.scroll = if event.kind == MouseEventKind::ScrollUp {
                    reader.scroll.min(layout.max_scroll).saturating_sub(3)
                } else {
                    (reader.scroll + 3).min(layout.max_scroll)
                };
                let scroll = reader.scroll;
                if let Some(selection) = reader.selection.as_mut().filter(|s| s.dragging) {
                    selection.head = point(row, scroll);
                }
            }
            MouseEventKind::Down(MouseButton::Left)
                if row >= layout.list_top && row < layout.list_top + layout.list_rows =>
            {
                let clicked = layout.list_start + (row - layout.list_top);
                if clicked != reader.selected {
                    reader.selected = clicked;
                    reader.scroll = 0;
                }
                reader.selection = None;
            }
            MouseEventKind::Down(MouseButton::Left) if in_answer => {
                let at = point(row, reader.scroll);
                reader.selection = Some(Selection {
                    anchor: at,
                    head: at,
                    dragging: true,
                });
            }
            MouseEventKind::Down(_) => reader.selection = None,
            MouseEventKind::Drag(MouseButton::Left) => {
                if reader.selection.is_some_and(|s| s.dragging) {
                    if row < layout.answer_top {
                        reader.scroll = reader.scroll.min(layout.max_scroll).saturating_sub(1);
                    } else if row > bottom {
                        reader.scroll = (reader.scroll + 1).min(layout.max_scroll);
                    }
                    let head = point(row, reader.scroll);
                    if let Some(selection) = reader.selection.as_mut() {
                        selection.head = head;
                    }
                }
            }
            MouseEventKind::Up(MouseButton::Left) => {
                let clicked = reader.selection.is_some_and(|s| s.anchor == s.head);
                if let Some(selection) = reader.selection.as_mut() {
                    selection.dragging = false;
                }
                if clicked {
                    reader.selection = None;
                } else if let Some(text) = self.selection_text() {
                    self.flash = Some(copy_text(&text, "选中内容"));
                }
            }
            _ => {}
        }
    }

    /// The text a mouse selection covers, as shown (styles removed).
    fn selection_text(&self) -> Option<String> {
        let (reader, turn) = self.current_turn()?;
        let selection = reader.selection?;
        let lines = self.answer_lines(
            &self.entries[reader.entry].summary.turns[turn],
            reader.layout.get().inner,
        );
        let ((start, _), (end, _)) = selection.ordered();
        let mut rows = Vec::new();
        let last = end.min(lines.len().saturating_sub(1));
        for (line, text) in lines.iter().enumerate().take(last + 1).skip(start) {
            if let Some((from, to)) = selection.columns(line) {
                rows.push(slice_columns(&plain(text), from, to).trim_end().to_owned());
            }
        }
        let text = rows.join("\n");
        (!text.trim().is_empty()).then_some(text)
    }

    // ------------------------------------------------------------ drawing

    fn render_line(&self, line: &Line, inner: usize, bg: Option<&str>) -> String {
        line.render(inner, bg, self.color)
    }

    fn rule(&self, label: &str, right: &str, inner: usize) -> String {
        let right = if right.is_empty() {
            String::new()
        } else {
            format!(" {right} ")
        };
        let fill = inner
            .saturating_sub(3 + crate::text::width(label) + 1 + crate::text::width(&right) + 2);
        format!(
            "{}{}{}{}",
            self.paint(BORDER, "── "),
            self.paint("1;97", label),
            self.paint(BORDER, &format!(" {}", "─".repeat(fill))),
            self.paint(BORDER, &format!("{right}──"))
        )
    }

    fn frame_top(&self, frame: &Frame, title: &str, right: &str) -> String {
        let right = format!(" {right} ");
        let title = crate::text::clip_width(
            title,
            frame.total.saturating_sub(8 + crate::text::width(&right)),
        );
        let fill = frame
            .total
            .saturating_sub(3 + crate::text::width(&title) + 1 + crate::text::width(&right) + 2);
        format!(
            "{}{}{}{}",
            self.paint(BORDER, "┌─ "),
            self.paint(TITLE, &title),
            self.paint(BORDER, &format!(" {}", "─".repeat(fill))),
            self.paint(BORDER, &format!("{right}─┐"))
        )
    }

    fn frame_sep(&self, frame: &Frame, label: &str) -> String {
        let fill = frame
            .total
            .saturating_sub(4 + crate::text::width(label) + 2);
        format!(
            "{}{}{}",
            self.paint(BORDER, "├─ "),
            self.paint(DIM, label),
            self.paint(BORDER, &format!(" {}┤", "─".repeat(fill)))
        )
    }

    fn frame_bottom(&self, frame: &Frame, hint: &str, right: &str) -> String {
        let right = if right.is_empty() {
            String::new()
        } else {
            format!(" {right} ")
        };
        let hint = crate::text::clip_width(
            hint,
            frame.total.saturating_sub(8 + crate::text::width(&right)),
        );
        let fill = frame
            .total
            .saturating_sub(3 + crate::text::width(&hint) + 1 + crate::text::width(&right) + 2);
        format!(
            "{}{}{}{}",
            self.paint(BORDER, "└─ "),
            self.paint(DIM, &hint),
            self.paint(BORDER, &format!(" {}", "─".repeat(fill))),
            self.paint(BORDER, &format!("{right}─┘"))
        )
    }

    fn frame_row(&self, frame: &Frame, line: &Line, bg: Option<&str>) -> String {
        format!(
            "{}{}{}",
            self.paint(BORDER, "│ "),
            line.render(frame.inner(), bg, self.color),
            self.paint(BORDER, " │")
        )
    }
}

fn group_of(entry: &HistoryEntry) -> &'static str {
    if entry.current {
        "当前会话"
    } else {
        time::group(entry.summary.updated)
    }
}

struct Frame {
    total: usize,
}

impl Frame {
    fn inner(&self) -> usize {
        self.total.saturating_sub(4)
    }
}

/// A row as styled pieces, so its width is known before styles are applied.
#[derive(Default, Clone)]
struct Line(Vec<(String, String)>);

impl Line {
    fn new() -> Self {
        Self::default()
    }

    fn push(mut self, code: &str, text: impl Into<String>) -> Self {
        self.0.push((code.to_owned(), text.into()));
        self
    }

    fn width(&self) -> usize {
        self.0
            .iter()
            .map(|(_, text)| crate::text::width(text))
            .sum()
    }

    /// Exactly `inner` columns; `bg` colors the whole row (the selection).
    fn render(&self, inner: usize, bg: Option<&str>, color: bool) -> String {
        let style = |code: &str| match (code.is_empty(), bg) {
            (true, None) => String::new(),
            (true, Some(bg)) => bg.to_owned(),
            (false, None) => code.to_owned(),
            (false, Some(bg)) => format!("{code};{bg}"),
        };
        let paint = |code: &str, text: &str| {
            if !color || code.is_empty() || text.is_empty() {
                text.to_owned()
            } else {
                format!("\x1b[{code}m{text}\x1b[0m")
            }
        };
        let mut out = String::new();
        let mut used = 0;
        for (code, text) in &self.0 {
            if used >= inner {
                break;
            }
            let text = crate::text::clip_width(&visible(text), inner - used);
            used += crate::text::width(&text);
            out.push_str(&paint(&style(code), &text));
        }
        out.push_str(&paint(&style(""), &" ".repeat(inner - used)));
        out
    }
}

/// Left content and right-aligned details; the left side is cut to fit.
fn spread(left: Line, right: Line, inner: usize) -> Line {
    let right_width = right.width();
    let room = inner.saturating_sub(right_width + 2);
    let mut out = Line::new();
    let mut used = 0;
    for (code, text) in left.0 {
        if used >= room {
            break;
        }
        let text = crate::text::clip_width(&visible(&text), room - used);
        used += crate::text::width(&text);
        out = out.push(&code, text);
    }
    let gap = inner.saturating_sub(used + right_width);
    let mut out = out.push("", " ".repeat(gap));
    out.0.extend(right.0);
    out
}

fn highlight(text: &str, base: &str, query: &str) -> Line {
    let query = query.trim().to_lowercase();
    let original: Vec<char> = text.chars().collect();
    let lower: Vec<char> = text.chars().flat_map(char::to_lowercase).collect();
    let needle: Vec<char> = query.chars().collect();
    if needle.is_empty() || lower.len() != original.len() {
        return Line::new().push(base, text);
    }
    let mut line = Line::new();
    let mut at = 0;
    while at < original.len() {
        if lower[at..].starts_with(&needle) {
            line = line.push(
                HIT,
                original[at..at + needle.len()].iter().collect::<String>(),
            );
            at += needle.len();
        } else {
            let start = at;
            while at < original.len() && !lower[at..].starts_with(&needle) {
                at += 1;
            }
            line = line.push(base, original[start..at].iter().collect::<String>());
        }
    }
    line
}

fn pad_right(text: &str, columns: usize) -> String {
    format!(
        "{text}{}",
        " ".repeat(columns.saturating_sub(crate::text::width(text)))
    )
}

fn pad_left(text: &str, columns: usize) -> String {
    format!(
        "{}{text}",
        " ".repeat(columns.saturating_sub(crate::text::width(text)))
    )
}

/// Stored text made safe to print; tabs become spaces so columns add up.
fn visible(text: &str) -> String {
    crate::text::visible(text).replace('\t', " ")
}

fn one_line(text: &str) -> String {
    crate::text::one_line(&visible(text))
}

/// The first line of prose in a reply: headings, tables and code are skipped.
fn first_sentence(reply: &str) -> Option<String> {
    let mut fence = false;
    for line in reply.lines() {
        let trimmed = line.trim();
        if trimmed.starts_with("```") {
            fence = !fence;
            continue;
        }
        if fence || trimmed.is_empty() || trimmed.starts_with('|') {
            continue;
        }
        let text = trimmed
            .trim_start_matches(['#', '>', '-', '*', ' '])
            .replace("**", "")
            .replace('`', "");
        if !text.trim().is_empty() {
            return Some(one_line(&text));
        }
    }
    None
}

fn snippet(text: &str, query: &str, max: usize) -> String {
    let lower = text.to_lowercase();
    let Some(at) = lower.find(&query.trim().to_lowercase()) else {
        return crate::text::clip_width(text, max);
    };
    let before = text[..at.min(text.len())].chars().count();
    if before <= 8 {
        return crate::text::clip_width(text, max);
    }
    let rest: String = text.chars().skip(before - 8).collect();
    format!("…{}", crate::text::clip_width(&rest, max))
}

/// Styles removed.
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

/// The display columns `[from, to)` of `text`; a wide character counts by its start.
fn slice_columns(text: &str, from: usize, to: usize) -> String {
    let mut out = String::new();
    let mut column = 0;
    for c in text.chars() {
        if column >= to {
            break;
        }
        if column >= from {
            out.push(c);
        }
        column += crate::text::char_width(c);
    }
    out
}

/// `text` with the display columns `[from, to)` shown reversed (the selection).
fn reverse_columns(text: &str, from: usize, to: usize) -> String {
    const ON: &str = "\x1b[7m";
    const OFF: &str = "\x1b[27m";
    let mut out = String::new();
    let mut column = 0;
    let mut inside = false;
    let mut chars = text.chars().peekable();
    while let Some(c) = chars.next() {
        if c == '\x1b' {
            let mut sequence = String::from(c);
            for next in chars.by_ref() {
                sequence.push(next);
                if next.is_ascii_alphabetic() {
                    break;
                }
            }
            out.push_str(&sequence);
            // A reset inside the line would end the reversal.
            if inside {
                out.push_str(ON);
            }
            continue;
        }
        if !inside && column >= from && column < to {
            out.push_str(ON);
            inside = true;
        }
        if inside && column >= to {
            out.push_str(OFF);
            inside = false;
        }
        out.push(c);
        column += crate::text::char_width(c);
    }
    // Selected past the line's end: one reversed space shows the line break is too.
    if to == usize::MAX && column >= from {
        if !inside {
            out.push_str(ON);
        }
        out.push(' ');
        inside = true;
    }
    if inside {
        out.push_str(OFF);
    }
    out
}

/// Writes the clipboard, retrying while another program holds it (about 0.5 s).
fn copy_text(text: &str, what: &str) -> String {
    if text.trim().is_empty() {
        return "没有可复制的文字".into();
    }
    // `arboard` is a Windows-only dependency (see Cargo.toml), like the image paste
    // path in `paste.rs`; elsewhere the reader's editor is the way to copy.
    #[cfg(not(windows))]
    {
        let _ = what;
        "仅 Windows 支持写入本机剪贴板；请用 o 在编辑器中打开后复制".into()
    }
    #[cfg(windows)]
    {
        if std::env::var_os("SSH_CONNECTION").is_some() || std::env::var_os("SSH_TTY").is_some() {
            return "远程会话无法写入本机剪贴板；请用 o 在编辑器中打开后复制".into();
        }
        let mut failure = String::new();
        for _ in 0..10 {
            match arboard::Clipboard::new()
                .and_then(|mut clipboard| clipboard.set_text(text.to_owned()))
            {
                Ok(()) => {
                    return format!("✓ 已复制{what}（{} 字）到剪贴板", text.chars().count());
                }
                Err(error) => failure = error.to_string(),
            }
            std::thread::sleep(std::time::Duration::from_millis(50));
        }
        format!("复制失败：{failure}")
    }
}

/// Saves the turn as Markdown in the temp directory and opens it in an editor
/// (VS Code when on PATH, else the system's default), for copying any part.
fn open_in_editor(summary: &SessionSummary, turn: usize) -> String {
    let record = &summary.turns[turn];
    let dir = std::env::temp_dir().join("cove-history");
    if let Err(error) = std::fs::create_dir_all(&dir) {
        return format!("打开失败：{error}");
    }
    let path = dir.join(format!(
        "{}-{}.md",
        &summary.id[..summary.id.len().min(8)],
        turn + 1
    ));
    let body = format!(
        "# {}\n\n> 第 {}/{} 轮 · {}\n\n## 提问\n\n{}\n\n## 回答\n\n{}\n",
        summary.title,
        turn + 1,
        summary.turns.len(),
        time::stamp(summary.updated),
        record.input.trim(),
        record.reply.trim()
    );
    if let Err(error) = std::fs::write(&path, body) {
        return format!("打开失败：{error}");
    }
    let target = path.clone();
    std::thread::spawn(move || {
        let quiet = |command: &mut std::process::Command| {
            command
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .status()
                .is_ok_and(|status| status.success())
        };
        let opened = if cfg!(windows) {
            quiet(
                std::process::Command::new("cmd")
                    .args(["/C", "code", "--reuse-window"])
                    .arg(&target),
            ) || quiet(
                std::process::Command::new("cmd")
                    .args(["/C", "start", ""])
                    .arg(&target),
            )
        } else {
            quiet(
                std::process::Command::new("code")
                    .arg("--reuse-window")
                    .arg(&target),
            ) || quiet(
                std::process::Command::new(if cfg!(target_os = "macos") {
                    "open"
                } else {
                    "xdg-open"
                })
                .arg(&target),
            )
        };
        let _ = opened;
    });
    format!("正在用编辑器打开 {}", crate::paths::display(&path))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crossterm::event::KeyEventKind;

    fn summary(id: &str, title: &str, turns: &[(&str, &str)]) -> SessionSummary {
        SessionSummary {
            id: id.into(),
            title: title.into(),
            cwd: "/w".into(),
            updated: crate::session::now(),
            unfinished: false,
            interrupted: false,
            turns: turns
                .iter()
                .map(|(input, reply)| TurnRecord {
                    input: (*input).into(),
                    reply: (*reply).into(),
                    images: 0,
                    steer: false,
                    archived: false,
                })
                .collect(),
            model: "m".into(),
            cost: None,
            lines_added: 0,
            lines_removed: 0,
            compacted: false,
        }
    }

    fn view() -> HistoryView {
        let entries = vec![
            HistoryEntry {
                summary: summary(
                    "aaaa1111",
                    "修复登录",
                    &[("登录报错", "已修复 **登录**"), ("加测试", "测试已加")],
                ),
                command: "/continue aaaa1111".into(),
                current: false,
            },
            HistoryEntry {
                summary: summary(
                    "bbbb2222",
                    "部署脚本",
                    &[("写部署脚本", "脚本在 deploy.sh")],
                ),
                command: "/continue bbbb2222".into(),
                current: false,
            },
        ];
        HistoryView::new("继续会话", entries, false, false)
    }

    fn key(code: KeyCode) -> KeyEvent {
        KeyEvent {
            code,
            modifiers: KeyModifiers::NONE,
            kind: KeyEventKind::Press,
            state: crossterm::event::KeyEventState::NONE,
        }
    }

    fn mouse(kind: MouseEventKind, row: u16, column: u16) -> MouseEvent {
        MouseEvent {
            kind,
            column,
            row,
            modifiers: KeyModifiers::NONE,
        }
    }

    #[test]
    fn the_list_filters_numbers_and_submits_the_chosen_command() {
        let mut view = view();
        let rows = view.list_rows("", 100, 30).join("\n");
        assert!(
            rows.contains("修复登录") && rows.contains("部署脚本"),
            "{rows}"
        );
        assert!(rows.contains("今天") && rows.contains("最近一问"), "{rows}");
        assert!(
            rows.lines().all(|row| crate::text::width(row) <= 99),
            "{rows}"
        );
        assert_eq!(view.list_key(key(KeyCode::Down), ""), ListAction::None);
        assert_eq!(
            view.list_key(key(KeyCode::Enter), ""),
            ListAction::Submit("/continue bbbb2222".into())
        );
        // A filter by a turn's input; digits pick only while nothing is typed.
        let rows = view.list_rows("测试", 100, 30).join("\n");
        // The match is the latest turn, already shown as the latest request.
        assert!(
            rows.contains("1 条") && rows.contains("#2 加测试"),
            "{rows}"
        );
        assert_eq!(
            view.list_key(key(KeyCode::Char('2')), "测试"),
            ListAction::Edit
        );
        assert_eq!(
            view.list_key(key(KeyCode::Char('1')), ""),
            ListAction::Submit("/continue aaaa1111".into())
        );
        assert_eq!(view.list_key(key(KeyCode::Char('x')), ""), ListAction::Edit);
        assert_eq!(view.list_key(key(KeyCode::Esc), ""), ListAction::Close);
        let mut ctrl_r = key(KeyCode::Char('r'));
        ctrl_r.modifiers = KeyModifiers::CONTROL;
        view.selected = 0;
        assert_eq!(
            view.list_key(ctrl_r, ""),
            ListAction::Prefill("/resume rename aaaa1111 ".into())
        );
    }

    #[test]
    fn the_reader_switches_turns_and_shows_only_the_conclusion() {
        let mut view = view();
        view.list_key(key(KeyCode::Tab), "");
        assert!(view.reading());
        let screen = view.reader_lines(80, 24).join("\n");
        assert!(
            screen.contains("第 2/2 轮的回答") && screen.contains("测试已加"),
            "{screen}"
        );
        assert_eq!(view.reader_lines(80, 24).len(), 24);
        view.reader_key(key(KeyCode::Up));
        let screen = view.reader_lines(80, 24).join("\n");
        assert!(
            screen.contains("第 1/2 轮的回答") && screen.contains("已修复 登录"),
            "{screen}"
        );
        assert!(!screen.contains("**"), "{screen}");
        assert_eq!(
            view.reader_key(key(KeyCode::Enter)),
            ReaderAction::Submit("/continue aaaa1111".into())
        );
        assert_eq!(view.reader_key(key(KeyCode::Esc)), ReaderAction::Leave);
        assert!(!view.reading());
    }

    #[test]
    fn dragging_selects_text_across_the_answer() {
        let mut view = view();
        view.list_key(key(KeyCode::Tab), "");
        view.reader_key(key(KeyCode::Up));
        let screen = view.reader_lines(80, 24);
        let top = screen
            .iter()
            .position(|line| line.contains("第 1/2 轮的回答"))
            .unwrap()
            + 1;
        let top = u16::try_from(top).unwrap();
        // From the request's text to the character under the pointer ("修" spans 4-5).
        view.mouse(mouse(MouseEventKind::Down(MouseButton::Left), top, 3));
        view.mouse(mouse(MouseEventKind::Drag(MouseButton::Left), top + 2, 4));
        assert_eq!(view.selection_text().as_deref(), Some("登录报错\n\n● 已修"));
        let highlighted = view.reader_lines(80, 24);
        assert!(highlighted.iter().any(|line| line.contains("\x1b[7m")));
        // A click without moving selects nothing.
        view.mouse(mouse(MouseEventKind::Down(MouseButton::Left), top, 3));
        view.mouse(mouse(MouseEventKind::Up(MouseButton::Left), top, 3));
        assert_eq!(view.selection_text(), None);
    }

    #[test]
    fn a_new_title_replaces_the_listed_one() {
        let mut view = view();
        view.retitle("bbbb2222", "部署到测试环境");
        let rows = view.list_rows("", 100, 30).join("\n");
        assert!(
            rows.contains("部署到测试环境") && !rows.contains("部署脚本"),
            "{rows}"
        );
    }

    #[test]
    fn the_wheel_scrolls_and_never_switches_turns() {
        let long: String = (1..=60).map(|n| format!("第 {n} 行\n")).collect();
        let entries = vec![HistoryEntry {
            summary: summary("cccc3333", "长回答", &[("a", "short"), ("b", &long)]),
            command: "/resume cccc3333".into(),
            current: false,
        }];
        let mut view = HistoryView::new("恢复会话", entries, false, false).reading_alone();
        let _ = view.reader_lines(80, 24);
        view.mouse(mouse(MouseEventKind::ScrollDown, 20, 0));
        view.mouse(mouse(MouseEventKind::ScrollDown, 20, 0));
        let screen = view.reader_lines(80, 24).join("\n");
        assert!(
            screen.contains("第 2/2 轮的回答") && screen.contains("7-"),
            "{screen}"
        );
        assert!(view.reader_only());
    }

    #[test]
    fn copying_nothing_says_so_without_touching_the_clipboard() {
        assert_eq!(copy_text("  \n", "选中内容"), "没有可复制的文字");
    }
}
