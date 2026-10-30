//! The interactive terminal. While the REPL runs, one thread owns the keyboard
//! and keeps the input line on screen, also while a task works: plain text typed
//! then steers the task at its next model call, slash commands queue behind it,
//! Esc twice or Ctrl+C interrupts it, and questions are answered on the same
//! line (a single key where the question lists them). Everything printed goes
//! through [`write`], which draws it above the input instead of through it.

use std::{
    collections::VecDeque,
    io::{IsTerminal, Write},
    path::PathBuf,
    sync::{Arc, LazyLock, Mutex, OnceLock},
    time::{Duration, Instant},
};

use crossterm::event::{self, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers};
use futures_util::future::BoxFuture;
use tokio::sync::{Notify, mpsc, oneshot};
use unicode_segmentation::UnicodeSegmentation;
use unicode_width::UnicodeWidthChar;

use crate::{
    app::completion::{self, Candidate, Completer},
    domain::{Error, Result},
    engine::turn::TurnHook,
    session::Session,
    tools::ToolOutput,
};

/// Most candidates listed below the input at once.
const PANEL_ROWS: usize = 6;
/// Characters one input may hold.
const LINE_LIMIT: usize = 64 * 1024;
/// How long the second Esc has to follow the first to interrupt a task.
const ESC_WINDOW: Duration = Duration::from_secs(2);
/// Lines the input history keeps.
const HISTORY_LIMIT: usize = 1000;
/// History file size that triggers a rewrite with the newest entries.
const HISTORY_FILE_LIMIT: u64 = 2 * 1024 * 1024;
/// Rows a multi-line input may take before the rest scrolls out of view.
const INPUT_ROWS: usize = 12;
/// Steering messages waiting for the running task.
const STEER_LIMIT: usize = 16;

const STEER_PREFIX: &str =
    "[the user typed this while the task ran; treat it as an instruction from the user]";

/// What the keyboard thread hands the REPL.
#[derive(Debug, PartialEq, Eq)]
pub enum Input {
    /// A submitted line and the images attached to it (pasted paths, Alt+V).
    Line(String, Vec<PathBuf>),
    /// Ctrl+D on an empty line or a closed terminal.
    Closed,
}

static CONSOLE: OnceLock<Arc<Console>> = OnceLock::new();

/// How full the context is and what the session has used, for the usage row.
#[derive(Clone, Copy, Debug, Default, PartialEq)]
pub struct Meter {
    /// Tokens of the conversation as the model sees it.
    pub context: u64,
    pub window: u64,
    /// Where the automatic compaction starts.
    pub compact_at: u64,
    /// Tokens the session has used, input and output.
    pub session: u64,
    pub cost: Option<f64>,
}

impl Meter {
    /// The row and whether the context nears the compaction point (from 80% of it).
    pub fn row(&self) -> Option<(String, bool)> {
        use crate::app::render::short_count;
        if self.window == 0 {
            return None;
        }
        let percent = self.context * 100 / self.window;
        let mut row = format!(
            "  上下文 {}/{}（{percent}%）· 本会话 {} tokens",
            short_count(self.context),
            short_count(self.window),
            short_count(self.session)
        );
        if let Some(cost) = self.cost {
            row.push_str(&format!(" · ${cost:.2}"));
        }
        let near = self.compact_at > 0 && self.context * 5 >= self.compact_at * 4;
        if near {
            row.push_str(&format!(
                " · 快到 {} 自动压缩了，可先输入 /compact",
                short_count(self.compact_at)
            ));
        }
        Some((row, near))
    }
}

/// Status-row spinner frames; Braille dots where the terminal is known to draw them.
static SPINNER: LazyLock<&'static [&'static str]> = LazyLock::new(|| {
    let modern = ["WT_SESSION", "TERM_PROGRAM", "VSCODE_PID", "WEZTERM_PANE"]
        .iter()
        .any(|name| std::env::var_os(name).is_some())
        || !cfg!(windows);
    if modern {
        &["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"]
    } else {
        &["-", "\\", "|", "/"]
    }
});

/// `NO_COLOR` set: styles are left out, cursor control stays.
static NO_COLOR: LazyLock<bool> =
    LazyLock::new(|| std::env::var_os("NO_COLOR").is_some_and(|value| !value.is_empty()));

pub struct Console {
    state: Mutex<State>,
    interrupt: Notify,
    /// The agent panel was opened; its source refreshes it now.
    panel_wake: Notify,
    lines: tokio::sync::Mutex<mpsc::UnboundedReceiver<Input>>,
}

/// Whether the REPL should run the console: a terminal that understands control
/// sequences, unless `COVE_PLAIN_REPL` asks for plain line reading.
pub fn supported() -> bool {
    let plain = std::env::var("COVE_PLAIN_REPL")
        .is_ok_and(|value| matches!(value.trim(), "1" | "true" | "yes" | "on"));
    !plain
        && std::io::stdin().is_terminal()
        && std::io::stderr().is_terminal()
        && enable_virtual_terminal()
}

/// Styled presentation: the console runs and model text goes to the terminal.
pub fn rich() -> bool {
    active().is_some_and(|console| console.lock().stdout_terminal)
}

/// The console when the REPL owns the terminal.
pub fn active() -> Option<&'static Arc<Console>> {
    CONSOLE.get().filter(|console| console.lock().running)
}

/// Takes over the terminal: raw mode, bracketed paste and a keyboard thread.
/// `history` is the file typed lines are kept in across sessions, `cwd` resolves
/// pasted paths and clipboard images are saved in `scratch`.
pub fn start(
    history: Option<PathBuf>,
    cwd: PathBuf,
    scratch: PathBuf,
) -> Result<&'static Arc<Console>> {
    if CONSOLE.get().is_some() {
        return Err(Error::Session(
            "交互终端已在运行；请关闭多余的 cove 实例后重试".into(),
        ));
    }
    let (sender, receiver) = mpsc::unbounded_channel();
    let mut state = State::new(sender, History::load(history));
    state.running = true;
    state.stdout_terminal = std::io::stdout().is_terminal();
    state.cwd = cwd;
    state.scratch = scratch;
    let console = CONSOLE.get_or_init(|| {
        Arc::new(Console {
            state: Mutex::new(state),
            interrupt: Notify::new(),
            panel_wake: Notify::new(),
            lines: tokio::sync::Mutex::new(receiver),
        })
    });
    crossterm::terminal::enable_raw_mode()?;
    {
        let mut state = console.lock();
        state.pending.push_str("\x1b[?2004h");
        state.draw();
        state.flush();
    }
    std::thread::Builder::new()
        .name("console-keys".into())
        .spawn(move || keyboard(console))?;
    Ok(console)
}

/// Gives the terminal back: the input line is removed and raw mode ends.
pub fn stop() {
    if let Some(console) = CONSOLE.get() {
        let mut state = console.lock();
        if state.running {
            state.erase();
            state.pending.push_str("\x1b[?2004l\x1b[0m");
            state.running = false;
            state.flush();
            let _ = crossterm::terminal::disable_raw_mode();
        }
    }
}

/// Prints `text` above the input line; plain writes when no console runs.
pub fn write(stdout: bool, text: &str) -> std::io::Result<()> {
    if let Some(console) = active() {
        console.lock().output(stdout, text);
        return Ok(());
    }
    if stdout {
        let mut out = std::io::stdout();
        out.write_all(text.as_bytes())?;
        out.flush()
    } else {
        let mut out = std::io::stderr();
        out.write_all(text.as_bytes())?;
        out.flush()
    }
}

/// Resolves on Ctrl+C: the signal, or the keys while the console has raw mode.
pub async fn interrupted() -> std::io::Result<()> {
    match active() {
        Some(console) => {
            tokio::select! {
                signal = tokio::signal::ctrl_c() => signal,
                () = console.interrupt.notified() => Ok(()),
            }
        }
        None => tokio::signal::ctrl_c().await,
    }
}

impl Console {
    fn lock(&self) -> std::sync::MutexGuard<'_, State> {
        self.state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
    }

    /// The next line typed; slash commands typed while a task ran come first.
    pub async fn next_line(&self) -> Input {
        self.lines
            .lock()
            .await
            .recv()
            .await
            .unwrap_or(Input::Closed)
    }

    /// Marks a task as running (or done): the status row, steering and Esc twice.
    pub fn set_busy(&self, busy: bool) {
        let mut state = self.lock();
        state.busy = busy;
        state.esc_armed = None;
        state.activity.clear();
        if busy {
            state.started = Instant::now();
            state.tokens = (0, 0);
        } else {
            state.queued = 0;
            state.steered = 0;
        }
        state.redraw();
    }

    /// What the running task does now, for the status row (`思考中`, `运行 cargo test`).
    pub fn set_activity(&self, activity: &str) {
        let mut state = self.lock();
        if state.activity != activity {
            state.activity = activity.to_owned();
            state.redraw();
        }
    }

    /// Tokens a model call of the running task used; `input` (with cache) is also
    /// how full the context was for that call.
    pub fn add_tokens(&self, input: u64, output: u64) {
        let mut state = self.lock();
        state.tokens.0 += input;
        state.tokens.1 += output;
        state.meter.context = input;
        state.meter.session += input + output;
        state.redraw();
    }

    /// The usage row: `context` only when the history was rebuilt (start, compaction,
    /// another session); otherwise the size the last model call reported stays.
    pub fn set_meter(
        &self,
        context: Option<u64>,
        window: u64,
        compact_at: u64,
        session: u64,
        cost: Option<f64>,
    ) {
        let mut state = self.lock();
        if let Some(context) = context {
            state.meter.context = context;
        }
        state.meter.window = window;
        state.meter.compact_at = compact_at;
        state.meter.session = session;
        state.meter.cost = cost;
        state.redraw();
    }

    /// Seconds since the running task started.
    pub fn elapsed(&self) -> f64 {
        self.lock().started.elapsed().as_secs_f64()
    }

    /// Completion candidates for the next lines.
    pub fn set_completer(&self, completer: Completer) {
        self.lock().completer = Some(completer);
    }

    /// A note in the status row until the next line is submitted.
    pub fn set_hint(&self, hint: &str) {
        let mut state = self.lock();
        state.hint = (!hint.is_empty()).then(|| hint.to_owned());
        state.redraw();
    }

    /// Steering text the task has not taken; the REPL runs it as the next task.
    pub fn take_steer(&self) -> Option<String> {
        let mut state = self.lock();
        if state.steer.is_empty() {
            return None;
        }
        Some(state.steer.drain(..).collect::<Vec<_>>().join("\n"))
    }

    /// Asks `prompt` on the input line; the draft being typed is kept for later.
    pub async fn ask(&self, prompt: &str, masked: bool) -> Result<String> {
        let receiver = self.lock().ask(prompt, masked);
        receiver.await.unwrap_or(Err(Error::Cancelled))
    }

    /// [`ask`](Self::ask) for a blocking caller.
    pub fn ask_blocking(&self, prompt: &str, masked: bool) -> Result<String> {
        let receiver = self.lock().ask(prompt, masked);
        receiver.blocking_recv().unwrap_or(Err(Error::Cancelled))
    }

    /// Withdraws the question being asked, if any.
    pub fn cancel_ask(&self) {
        let mut state = self.lock();
        state.end_ask(Err(Error::Cancelled));
        state.redraw();
    }

    /// Opens a picker below the input: typing filters, Enter submits the chosen
    /// value as a typed line. False when a question is waiting.
    pub fn choose(&self, title: &str, choices: Vec<Choice>) -> bool {
        let mut state = self.lock();
        if state.ask.is_some() || choices.is_empty() {
            return false;
        }
        state.editor.take();
        state.candidates.clear();
        state.choices = Some(Choices {
            title: title.to_owned(),
            items: choices,
            selected: 0,
        });
        state.redraw();
        true
    }

    /// Whether the agent panel is open (Alt+M).
    pub fn panel_open(&self) -> bool {
        self.lock().panel.is_some()
    }

    /// Waits until the agent panel is opened.
    pub async fn panel_opened(&self) {
        self.panel_wake.notified().await;
    }

    /// New content for the open agent panel.
    pub fn set_panel(&self, lines: Vec<String>) {
        let mut state = self.lock();
        if state.panel.is_some() {
            state.panel = Some(lines);
            state.redraw();
        }
    }

    /// Clears the screen and scrollback, keeping the input.
    pub fn clear_screen(&self) {
        let mut state = self.lock();
        state.clear_screen();
        state.draw();
        state.flush();
    }
}

/// Delivers what the user typed while a task ran at its next model call.
pub struct SteerHook;

impl SteerHook {
    fn take() -> Option<String> {
        active()?
            .take_steer()
            .map(|text| format!("{STEER_PREFIX}\n{text}"))
    }
}

impl TurnHook for SteerHook {
    fn tool_note(
        &self,
        _name: &str,
        _input: &serde_json::Value,
        _output: &ToolOutput,
    ) -> Option<String> {
        Self::take()
    }

    fn finish_nudge<'a>(&'a self, _session: &'a Session) -> BoxFuture<'a, Option<String>> {
        Box::pin(async { Self::take() })
    }
}

/// Keys arriving closer together than this are one burst: a paste, not typing.
const BURST: Duration = Duration::from_millis(8);

/// Waits up to [`BURST`] for the next event of a burst.
fn burst_continues(queue: &mut VecDeque<Event>) -> bool {
    if !queue.is_empty() {
        return true;
    }
    match event::poll(BURST) {
        Ok(true) => match event::read() {
            Ok(next) => {
                queue.push_back(next);
                true
            }
            Err(_) => false,
        },
        _ => false,
    }
}

fn keyboard(console: &'static Arc<Console>) {
    let mut queue: VecDeque<Event> = VecDeque::new();
    // The line as a burst of keys found it; a burst of image paths attaches them.
    let mut burst: Option<(Vec<char>, usize)> = None;
    let mut pasted_cr = false;
    loop {
        let event = match queue.pop_front() {
            Some(event) => event,
            None => match event::poll(Duration::from_millis(100)) {
                Ok(true) => match event::read() {
                    Ok(event) => event,
                    Err(_) => break,
                },
                Ok(false) => {
                    let mut state = console.lock();
                    if !state.running {
                        return;
                    }
                    state.tick();
                    continue;
                }
                Err(_) => break,
            },
        };
        // A console without bracketed paste delivers a pasted newline as Enter
        // and a pasted tab as Tab; nobody types on within a few milliseconds.
        let mut pasted = false;
        if let Event::Key(key) = &event
            && key.kind != KeyEventKind::Release
            && matches!(key.code, KeyCode::Enter | KeyCode::Tab)
            && key.modifiers.is_empty()
        {
            let press = |queue: &VecDeque<Event>| {
                queue.iter().any(
                    |next| matches!(next, Event::Key(next) if next.kind != KeyEventKind::Release),
                )
            };
            pasted = press(&queue);
            // Its own key release may come first; a few events decide it.
            for _ in 0..4 {
                if pasted || !matches!(event::poll(BURST), Ok(true)) {
                    break;
                }
                match event::read() {
                    Ok(next) => queue.push_back(next),
                    Err(_) => break,
                }
                pasted = press(&queue);
            }
        }
        {
            let mut state = console.lock();
            if !state.running {
                return;
            }
            let action = match event {
                // The LF of a pasted CR LF; the CR already made the newline.
                Event::Key(key)
                    if key.kind != KeyEventKind::Release
                        && std::mem::take(&mut pasted_cr)
                        && key.code == KeyCode::Enter
                        && key.modifiers == KeyModifiers::CONTROL =>
                {
                    Action::None
                }
                Event::Key(key) if key.kind != KeyEventKind::Release => {
                    if burst.is_none() {
                        burst = Some((state.editor.buffer.clone(), state.editor.cursor));
                    }
                    pasted_cr = pasted && key.code == KeyCode::Enter;
                    if pasted {
                        let c = if key.code == KeyCode::Enter {
                            '\n'
                        } else {
                            ' '
                        };
                        state.editor.insert(c);
                        state.edited();
                        Action::None
                    } else {
                        state.key(key)
                    }
                }
                Event::Paste(text) => {
                    if !state.attach_pasted(&text) {
                        state.editor.insert_text(&text);
                        state.edited();
                    }
                    Action::None
                }
                _ => Action::None,
            };
            match action {
                Action::None => {}
                Action::Interrupt => console.interrupt.notify_waiters(),
                Action::Panel => console.panel_wake.notify_waiters(),
            }
            state.dirty = true;
        }
        // The line is drawn once a burst has been consumed, not once per key.
        if !burst_continues(&mut queue) {
            let mut state = console.lock();
            if let Some((before, cursor)) = burst.take() {
                state.burst_ended(before, cursor);
            }
            state.redraw();
        }
    }
    let state = console.lock();
    let _ = state.lines.send(Input::Closed);
}

enum Action {
    None,
    Interrupt,
    Panel,
}

/// One entry of a picker.
#[derive(Clone, Debug)]
pub struct Choice {
    /// The line submitted when it is chosen.
    pub value: String,
    pub label: String,
    pub description: String,
}

struct Choices {
    title: String,
    items: Vec<Choice>,
    selected: usize,
}

struct Ask {
    prompt: String,
    masked: bool,
    /// Keys that answer on an empty line without Enter.
    keys: Vec<char>,
    /// Numbered options as (number, label); Up/Down put a label on the line.
    options: Vec<(String, String)>,
    reply: oneshot::Sender<Result<String>>,
    draft: Vec<char>,
}

struct Search {
    query: String,
    /// History index of the current match.
    found: Option<usize>,
    draft: Vec<char>,
}

struct State {
    running: bool,
    stdout_terminal: bool,
    editor: Editor,
    completer: Option<Completer>,
    candidates: Vec<Candidate>,
    selected: usize,
    history: History,
    search: Option<Search>,
    ask: Option<Ask>,
    lines: mpsc::UnboundedSender<Input>,
    steer: VecDeque<String>,
    busy: bool,
    queued: usize,
    steered: usize,
    esc_armed: Option<Instant>,
    /// The running task: when it started, what it does, tokens in and out.
    started: Instant,
    activity: String,
    tokens: (u64, u64),
    /// The spinner frame last drawn.
    frame: u64,
    /// Context and session usage, shown on a row of its own.
    meter: Meter,
    hint: Option<String>,
    /// What Esc cleared; Ctrl+Z puts it back.
    cleared: Option<Vec<char>>,
    /// The cursor's row within the drawn input block, when it is on screen.
    drawn: Option<usize>,
    /// Column after the last output character; output resumes there.
    out_col: usize,
    dirty: bool,
    pending: String,
    cwd: PathBuf,
    scratch: PathBuf,
    /// Images for the next line, with their status-row descriptions.
    attachments: Vec<(PathBuf, String)>,
    choices: Option<Choices>,
    /// The agent panel's lines while it is open.
    panel: Option<Vec<String>>,
}

impl State {
    fn new(lines: mpsc::UnboundedSender<Input>, history: History) -> Self {
        Self {
            running: false,
            stdout_terminal: false,
            editor: Editor::default(),
            completer: None,
            candidates: Vec::new(),
            selected: 0,
            history,
            search: None,
            ask: None,
            lines,
            steer: VecDeque::new(),
            busy: false,
            queued: 0,
            steered: 0,
            esc_armed: None,
            started: Instant::now(),
            activity: String::new(),
            tokens: (0, 0),
            frame: 0,
            meter: Meter::default(),
            hint: None,
            cleared: None,
            drawn: None,
            out_col: 0,
            dirty: false,
            pending: String::new(),
            cwd: PathBuf::from("."),
            scratch: std::env::temp_dir(),
            attachments: Vec::new(),
            choices: None,
            panel: None,
        }
    }

    /// Attaches a paste made only of image paths; false for other text.
    fn attach_pasted(&mut self, text: &str) -> bool {
        if self.ask.is_some() {
            return false;
        }
        let Some(paths) = crate::app::paste::image_paths(text, &self.cwd) else {
            return false;
        };
        for path in paths {
            self.attach(path);
        }
        true
    }

    fn attach(&mut self, path: PathBuf) {
        if self.attachments.iter().any(|(known, _)| *known == path) {
            return;
        }
        if self.attachments.len() >= 20 {
            self.hint = Some("每条消息最多 20 个附件；先发送这条，其余的放到下一条".into());
            return;
        }
        let description = crate::app::paste::describe(&path);
        self.attachments.push((path, description));
    }

    /// A burst of keys that typed only image paths attaches them instead.
    fn burst_ended(&mut self, before: Vec<char>, cursor: usize) {
        let added = self.editor.buffer.len().saturating_sub(before.len());
        if added < 5
            || self.editor.buffer[..cursor] != before[..cursor]
            || self.editor.buffer[cursor + added..] != before[cursor..]
        {
            return;
        }
        let pasted: String = self.editor.buffer[cursor..cursor + added].iter().collect();
        if self.attach_pasted(&pasted) {
            self.editor.buffer = before;
            self.editor.cursor = cursor;
            self.edited();
        }
    }

    fn clipboard_image(&mut self) {
        match crate::app::paste::clipboard_image(&self.scratch) {
            Ok(path) => self.attach(path),
            Err(err) => self.hint = Some(err.to_string()),
        }
    }

    fn choice_key(&mut self, key: KeyEvent, control: bool) -> Action {
        let Some(choices) = self.choices.as_mut() else {
            return Action::None;
        };
        let query = self.editor.buffer.iter().collect::<String>().to_lowercase();
        let visible: Vec<usize> = filter(&choices.items, &query);
        match key.code {
            KeyCode::Up if !visible.is_empty() => {
                choices.selected = (choices.selected + visible.len() - 1) % visible.len();
            }
            KeyCode::Down if !visible.is_empty() => {
                choices.selected = (choices.selected + 1) % visible.len();
            }
            KeyCode::Esc => {
                self.choices = None;
                self.editor.take();
            }
            KeyCode::Char('c') if control => {
                self.choices = None;
                self.editor.take();
            }
            KeyCode::Enter => {
                let chosen = visible
                    .get(choices.selected.min(visible.len().saturating_sub(1)))
                    .map(|index| choices.items[*index].value.clone());
                if let Some(value) = chosen {
                    self.choices = None;
                    self.editor.restore(value.chars().collect());
                    return self.enter();
                }
            }
            KeyCode::Backspace => {
                self.editor.backspace();
                choices.selected = 0;
            }
            KeyCode::Char(c) if !control && !c.is_control() => {
                self.editor.insert(c);
                choices.selected = 0;
            }
            _ => {}
        }
        Action::None
    }

    fn ask(&mut self, prompt: &str, masked: bool) -> oneshot::Receiver<Result<String>> {
        let (reply, receiver) = oneshot::channel();
        self.end_ask(Err(Error::Cancelled));
        // A question's leading lines are output; its last line is the prompt.
        let (head, tail) = prompt.rsplit_once('\n').unwrap_or(("", prompt));
        if !head.trim().is_empty() {
            self.notice(&format!("{head}\n"));
        }
        let options = options(head);
        let mut keys = answer_keys(tail);
        // The question tool's footer, in English or Chinese.
        if options.len() <= 9
            && (head.contains("Answer with a number") || head.contains("输入编号"))
        {
            keys.extend(
                options
                    .iter()
                    .filter_map(|(number, _)| number.chars().next()),
            );
        }
        let draft = self.editor.take();
        self.ask = Some(Ask {
            prompt: visible(tail),
            masked,
            keys,
            options,
            reply,
            draft,
        });
        self.candidates.clear();
        self.redraw();
        receiver
    }

    /// Answers (or withdraws) the question and puts the draft back.
    fn end_ask(&mut self, answer: Result<String>) {
        if let Some(ask) = self.ask.take() {
            let _ = ask.reply.send(answer);
            self.editor.restore(ask.draft);
            self.edited();
        }
    }

    fn tick(&mut self) {
        if self.esc_armed.is_some_and(|at| at.elapsed() > ESC_WINDOW) {
            self.esc_armed = None;
            self.redraw();
        } else if self.dirty || self.busy && self.ask.is_none() && self.spinner_moved() {
            self.redraw();
        }
    }

    /// Advances the spinner with the clock; true when a new frame is due.
    fn spinner_moved(&mut self) -> bool {
        let frame = self.started.elapsed().as_millis() as u64 / 150;
        let moved = frame != self.frame;
        self.frame = frame;
        moved
    }

    fn clear_screen(&mut self) {
        self.pending.push_str("\x1b[H\x1b[2J\x1b[3J");
        self.drawn = None;
        self.out_col = 0;
    }

    /// Applies one key press.
    fn key(&mut self, key: KeyEvent) -> Action {
        let control = key.modifiers.contains(KeyModifiers::CONTROL);
        let alt = key.modifiers.contains(KeyModifiers::ALT);
        if self.search.is_some() {
            return self.search_key(key, control);
        }
        if self.choices.is_some() {
            return self.choice_key(key, control);
        }
        if key.code != KeyCode::Esc {
            self.esc_armed = None;
        }
        // A question's one-key answer on an empty line.
        if let (Some(ask), KeyCode::Char(c)) = (&self.ask, key.code)
            && !control
            && !alt
            && self.editor.is_empty()
            && ask.keys.contains(&c.to_ascii_lowercase())
        {
            self.editor.insert(c.to_ascii_lowercase());
            return self.enter();
        }
        match key.code {
            // Alt/Shift/Ctrl+Enter type a newline; a pasted LF arrives as Ctrl+Enter.
            KeyCode::Enter if alt || control || key.modifiers.contains(KeyModifiers::SHIFT) => {
                self.editor.insert('\n');
                self.edited();
            }
            KeyCode::Char('j') if control => {
                self.editor.insert('\n');
                self.edited();
            }
            KeyCode::Enter => return self.enter(),
            KeyCode::Char('c') if control => return self.control_c(),
            KeyCode::Char('d') if control && self.editor.is_empty() && self.ask.is_none() => {
                let _ = self.lines.send(Input::Closed);
            }
            KeyCode::Char('d') if control => {
                self.editor.delete();
                self.edited();
            }
            KeyCode::Char('a') if control => self.moved(self.editor.line_start()),
            KeyCode::Char('e') if control => self.moved(self.editor.line_end()),
            KeyCode::Char('b') if alt => self.moved(self.editor.word_left()),
            KeyCode::Char('f') if alt => self.moved(self.editor.word_right()),
            KeyCode::Char('u') if control => {
                self.editor.kill_to_start();
                self.edited();
            }
            KeyCode::Char('k') if control => {
                self.editor.kill_to_end();
                self.edited();
            }
            KeyCode::Char('w') if control => {
                self.editor.delete_word_back();
                self.edited();
            }
            KeyCode::Backspace if alt && self.editor.is_empty() => {
                // Alt+Backspace on an empty line takes back the last attachment.
                if let Some((path, _)) = self.attachments.pop()
                    && path.starts_with(&self.scratch)
                {
                    let _ = std::fs::remove_file(path);
                }
            }
            KeyCode::Backspace if alt || control => {
                self.editor.delete_word_back();
                self.edited();
            }
            KeyCode::Char('v' | 'V') if alt => self.clipboard_image(),
            KeyCode::Char('m' | 'M') if alt => {
                if self.panel.take().is_none() {
                    self.panel = Some(vec!["加载中...".into()]);
                    return Action::Panel;
                }
            }
            KeyCode::Char('r') if control && self.ask.is_none() => {
                self.search = Some(Search {
                    query: String::new(),
                    found: self.history.find("", self.history.lines.len()),
                    draft: self.editor.buffer.clone(),
                });
                self.candidates.clear();
            }
            KeyCode::Char('z') if control => {
                if self.editor.is_empty()
                    && let Some(cleared) = self.cleared.take()
                {
                    self.editor.restore(cleared);
                    self.hint = None;
                    self.edited();
                }
            }
            KeyCode::Char('l') if control => self.clear_screen(),
            KeyCode::Char(c) if !control && !c.is_control() => {
                if self.editor.buffer.len() < LINE_LIMIT {
                    self.editor.insert(c);
                    self.edited();
                }
            }
            KeyCode::Tab => self.complete(),
            KeyCode::Backspace => {
                self.editor.backspace();
                self.edited();
            }
            KeyCode::Delete => {
                self.editor.delete();
                self.edited();
            }
            KeyCode::Left if control || alt => self.moved(self.editor.word_left()),
            KeyCode::Right if control || alt => self.moved(self.editor.word_right()),
            KeyCode::Left => self.moved(self.editor.cluster_left()),
            KeyCode::Right => self.moved(self.editor.cluster_right()),
            KeyCode::Home => self.moved(self.editor.line_start()),
            KeyCode::End => self.moved(self.editor.line_end()),
            KeyCode::Up | KeyCode::Down => self.vertical(key.code == KeyCode::Up),
            KeyCode::Esc => return self.escape(),
            _ => {}
        }
        Action::None
    }

    fn vertical(&mut self, up: bool) {
        if !self.candidates.is_empty() {
            let count = self.candidates.len();
            self.selected = if up {
                (self.selected + count - 1) % count
            } else {
                (self.selected + 1) % count
            };
        } else if let Some(cursor) = self.editor.vertical(up) {
            self.moved(cursor);
        } else if let Some(ask) = &self.ask {
            // Up/Down walk a question's options while the line holds none or one.
            let line: String = self.editor.buffer.iter().collect();
            let at = ask.options.iter().position(|(_, label)| *label == line);
            if (line.is_empty() || at.is_some()) && !ask.options.is_empty() {
                let count = ask.options.len();
                let next = match (at, up) {
                    (None, true) => count - 1,
                    (None, false) => 0,
                    (Some(at), true) => (at + count - 1) % count,
                    (Some(at), false) => (at + 1) % count,
                };
                let label = ask.options[next].1.chars().collect();
                self.editor.restore(label);
            }
        } else if let Some(line) = self.history.step(up, &self.editor.buffer) {
            self.editor.restore(line);
            self.candidates.clear();
        }
    }

    fn control_c(&mut self) -> Action {
        if !self.candidates.is_empty() {
            self.candidates.clear();
        } else if !self.editor.is_empty() {
            self.editor.take();
            self.edited();
        } else if self.ask.is_some() {
            self.end_ask(Err(Error::Cancelled));
        } else if self.busy {
            self.notice("[interrupted] 正在停止任务；之后用 /continue 接着做\n");
            return Action::Interrupt;
        } else {
            self.hint = Some("输入 /exit 或按 Ctrl+D 退出".into());
        }
        Action::None
    }

    fn escape(&mut self) -> Action {
        if !self.candidates.is_empty() {
            self.candidates.clear();
        } else if self.panel.is_some() {
            self.panel = None;
        } else if !self.editor.is_empty() {
            self.cleared = Some(self.editor.take());
            self.hint = Some("已清空；按 Ctrl+Z 恢复".into());
            self.edited();
        } else if self.ask.is_some() {
            self.end_ask(Err(Error::Cancelled));
        } else if self.busy && esc_interrupts() {
            if self
                .esc_armed
                .take()
                .is_some_and(|at| at.elapsed() <= ESC_WINDOW)
            {
                self.notice("[interrupted] 正在停止任务；之后用 /continue 接着做\n");
                return Action::Interrupt;
            }
            self.esc_armed = Some(Instant::now());
        }
        Action::None
    }

    fn enter(&mut self) -> Action {
        // `\` at the end continues the input on the next line.
        if self.editor.cursor == self.editor.buffer.len()
            && self.editor.buffer.last() == Some(&'\\')
        {
            self.editor.backspace();
            self.editor.insert('\n');
            self.edited();
            return Action::None;
        }
        if self.ask.is_none() {
            self.accept_candidate();
        }
        let line: String = self.editor.take().into_iter().collect();
        self.candidates.clear();
        self.cleared = None;
        self.hint = None;
        if let Some(ask) = &self.ask {
            let shown = if ask.masked {
                "*".repeat(line.chars().count().min(8))
            } else {
                line.clone()
            };
            let answer = ask
                .options
                .iter()
                .find(|(_, label)| *label == line)
                .map_or(line.clone(), |(number, _)| number.clone());
            let echo = format!("{}{}\n", ask.prompt, visible(&shown));
            self.notice(&echo);
            self.end_ask(Ok(answer));
            return Action::None;
        }
        if line.chars().count() > 1 {
            self.history.push(&line);
        }
        let line = if line.trim().is_empty() && !self.attachments.is_empty() {
            "Please analyze these attachments.".to_owned()
        } else {
            line
        };
        if line.trim().is_empty() {
            // An empty line lets the REPL show what waits (approvals).
            if !self.busy {
                let _ = self.lines.send(Input::Line(String::new(), Vec::new()));
            }
            return Action::None;
        }
        // Attachments cannot join a running task; such a message queues.
        if self.busy && !line.trim_start().starts_with('/') && self.attachments.is_empty() {
            if self.steer.len() >= STEER_LIMIT {
                self.notice(&format!(
                    "[steer] 已有 {STEER_LIMIT} 条消息在等待，这条没有送达；等任务结束后再发送\n"
                ));
            } else {
                self.steer.push_back(line.trim().to_owned());
                self.steered += 1;
                let echo = self.user_echo(&line, "33", "");
                self.notice(&format!(
                    "{echo}{}\n",
                    self.style("2", "  ↳ 将在下一次模型调用时交给正在运行的任务")
                ));
            }
            return Action::None;
        }
        let echo = if self.busy {
            self.queued += 1;
            self.user_echo(&line, "33", "  [排队中]")
        } else {
            self.user_echo(&line, "36", "")
        };
        self.notice(&echo);
        let attachments = self.attachments.drain(..).map(|(path, _)| path).collect();
        let _ = self.lines.send(Input::Line(line, attachments));
        Action::None
    }

    fn search_key(&mut self, key: KeyEvent, control: bool) -> Action {
        let Some(search) = self.search.as_mut() else {
            return Action::None;
        };
        let newest = self.history.lines.len();
        match key.code {
            KeyCode::Char('r') if control => {
                if let Some(at) = search.found
                    && let Some(older) = self.history.find(&search.query, at)
                {
                    search.found = Some(older);
                }
            }
            KeyCode::Char('g' | 'c') if control => {
                let search = self.search.take().expect("searching");
                self.editor.restore(search.draft);
            }
            KeyCode::Esc => {
                let search = self.search.take().expect("searching");
                self.editor.restore(search.draft);
            }
            KeyCode::Char(c) if !control && !c.is_control() => {
                search.query.push(c);
                let from = search.found.map_or(newest, |at| at + 1);
                search.found = self.history.find(&search.query, from);
            }
            KeyCode::Backspace => {
                search.query.pop();
                search.found = self.history.find(&search.query, newest);
            }
            _ => {
                // Enter or a movement key takes the match into the line.
                let search = self.search.take().expect("searching");
                match search.found.and_then(|at| self.history.lines.get(at)) {
                    Some(line) => {
                        self.editor.restore(line.chars().collect());
                        self.history.at = search.found;
                    }
                    None => self.editor.restore(search.draft),
                }
                if key.code == KeyCode::Enter {
                    return self.enter();
                }
            }
        }
        Action::None
    }

    fn moved(&mut self, cursor: usize) {
        let cursor = cursor.min(self.editor.buffer.len());
        if cursor != self.editor.cursor {
            self.editor.cursor = cursor;
            self.refresh_candidates();
        }
    }

    /// After an edit: the history walk restarts and the candidates follow.
    fn edited(&mut self) {
        self.history.reset();
        self.refresh_candidates();
    }

    /// Recomputes the candidates for the word before the cursor.
    fn refresh_candidates(&mut self) {
        self.selected = 0;
        self.candidates = match (&self.completer, &self.ask) {
            (Some(completer), None) => {
                let line = self.editor.before_cursor();
                let word = completion::current_word(&line);
                if (line.starts_with('/') && !line.contains('\n')) || word.starts_with('@') {
                    completer.suggest(&line)
                } else {
                    Vec::new()
                }
            }
            _ => Vec::new(),
        };
    }

    fn complete(&mut self) {
        let Some(chosen) = self.candidates.get(self.selected).cloned() else {
            return;
        };
        let line = self.editor.before_cursor();
        let typed = completion::current_word(&line).chars().count();
        let common = completion::common_prefix(&self.candidates);
        if self.candidates.len() > 1 && common.chars().count() > typed {
            self.editor.replace_word(&common, false);
        } else {
            self.editor.replace_word(&chosen.text, true);
            // A command that takes nothing more is complete as it is.
            let complete = self.editor.before_cursor() == chosen.text
                && chosen.text.starts_with('/')
                && self
                    .completer
                    .as_ref()
                    .is_some_and(|completer| !completer.takes_arguments(&chosen.text));
            if !chosen.text.ends_with('/') && !complete {
                if self.editor.buffer.get(self.editor.cursor) == Some(&' ') {
                    self.editor.cursor += 1;
                } else {
                    self.editor.insert(' ');
                }
            }
        }
        self.edited();
    }

    /// Enter on a partly typed command or argument takes the selected candidate;
    /// a typed `@path`, a bare `/` and an exact match are sent as written.
    fn accept_candidate(&mut self) {
        let line = self.editor.before_cursor();
        let word = completion::current_word(&line);
        if word.is_empty() || word == "/" || word.starts_with('@') {
            return;
        }
        if let Some(chosen) = self.candidates.get(self.selected).cloned()
            && chosen.text != word
        {
            self.editor.replace_word(&chosen.text, true);
        }
    }

    // --- drawing -----------------------------------------------------------

    /// A submitted line as the user's turn: a blank row, `>` and the text on a
    /// shaded band, so it stands apart from replies and tool rows.
    fn user_echo(&self, line: &str, marker: &str, tag: &str) -> String {
        let mut out = String::from("\n");
        for (index, row) in visible(line).lines().enumerate() {
            let lead = if index == 0 { ">" } else { " " };
            out.push_str(&format!(
                "{} {}",
                self.style(&format!("1;{marker}"), lead),
                self.style("48;5;237", &format!(" {row} "))
            ));
            if index == 0 {
                out.push_str(&self.style("2", tag));
            }
            out.push('\n');
        }
        out
    }

    fn style(&self, code: &str, text: &str) -> String {
        if *NO_COLOR {
            text.to_owned()
        } else {
            format!("\x1b[{code}m{text}\x1b[0m")
        }
    }

    fn columns(&self) -> usize {
        usize::from(
            crossterm::terminal::size()
                .map_or(80, |(columns, _)| columns)
                .max(20),
        )
    }

    /// A message of the console's own on a row of its own, also when model text
    /// stopped mid-line.
    fn notice(&mut self, text: &str) {
        if self.out_col > 0 {
            self.output(
                false,
                &format!(
                    "
{text}"
                ),
            );
        } else {
            self.output(false, text);
        }
    }

    /// Output above the input: the input is taken off, the output continues
    /// where it stopped, and the input is drawn again below it.
    fn output(&mut self, stdout: bool, text: &str) {
        if text.is_empty() {
            return;
        }
        if stdout && !self.stdout_terminal {
            let mut out = std::io::stdout();
            let _ = out.write_all(text.as_bytes());
            let _ = out.flush();
            return;
        }
        if !self.running {
            return;
        }
        let columns = self.columns();
        self.erase();
        if self.out_col > 0 {
            self.pending
                .push_str(&format!("\x1b[1A\x1b[{}G", self.out_col + 1));
        }
        let mut column = self.out_col;
        let mut escape = false;
        let mut sequence = String::new();
        for c in visible(text).chars() {
            if escape || c == '\x1b' {
                // Styles take no room; NO_COLOR drops them.
                sequence.push(c);
                escape = c == '\x1b' || !c.is_ascii_alphabetic();
                if !escape {
                    if !*NO_COLOR || !sequence.ends_with('m') {
                        self.pending.push_str(&sequence);
                    }
                    sequence.clear();
                }
                continue;
            }
            match c {
                '\n' => {
                    self.pending.push_str("\r\n");
                    column = 0;
                }
                '\t' => {
                    let spaces = (8 - column % 8).min(columns - column.min(columns));
                    self.pending.push_str(&" ".repeat(spaces.max(1)));
                    column += spaces.max(1);
                }
                c => {
                    let width = c.width().unwrap_or(0);
                    if column + width > columns {
                        column = 0;
                    }
                    self.pending.push(c);
                    column += width;
                }
            }
            column = column.min(columns);
        }
        if column >= columns {
            // The terminal waits at the margin; the next row starts clean.
            self.pending.push_str("\r\n");
            column = 0;
        } else if column > 0 {
            self.pending.push_str("\x1b[0m\r\n");
        }
        self.out_col = column;
        self.draw();
        self.flush();
    }

    /// Takes the input block off the screen; the cursor is left where it began.
    fn erase(&mut self) {
        if let Some(row) = self.drawn.take() {
            if row > 0 {
                self.pending.push_str(&format!("\x1b[{row}A"));
            }
            self.pending.push_str("\r\x1b[J");
        }
    }

    fn redraw(&mut self) {
        self.dirty = false;
        if !self.running {
            return;
        }
        self.erase();
        self.draw();
        self.flush();
    }

    fn flush(&mut self) {
        if self.pending.is_empty() {
            return;
        }
        let mut out = std::io::stderr();
        let _ = out.write_all(std::mem::take(&mut self.pending).as_bytes());
        let _ = out.flush();
    }

    /// Draws the status row, the input and the candidates; the cursor must be at
    /// the start of a row.
    fn draw(&mut self) {
        if !self.running {
            return;
        }
        let columns = self.columns();
        let mut screen = Screen::new(columns);
        if let Some(status) = self.status() {
            screen.text(&self.style("2", &clip(&status, columns - 1)));
            screen.newline();
        }
        if let Some((meter, near)) = self.meter.row() {
            screen.text(&self.style(if near { "33" } else { "2" }, &clip(&meter, columns - 1)));
            screen.newline();
        }
        let cursor = if let Some(search) = &self.search {
            let found = search.found.and_then(|at| self.history.lines.get(at));
            let label = if found.is_some() || search.query.is_empty() {
                "(搜索历史)"
            } else {
                "(搜索历史：无匹配)"
            };
            screen.text(&self.style("36", &format!("{label} {} > ", visible(&search.query))));
            let at = screen.position();
            let shown = found.map_or(String::new(), |line| visible(line).replace('\n', " "));
            screen.text(&clip(&shown, columns.saturating_sub(at.1 + 1)));
            at
        } else {
            self.draw_input(&mut screen, columns)
        };
        let rows = [
            self.panel(columns),
            self.choice_rows(columns),
            self.agent_rows(columns),
        ];
        for row in rows.into_iter().flatten() {
            screen.newline();
            screen.text(&row);
        }
        let (text, end) = screen.finish();
        self.pending.push_str(&text);
        let (row, column) = if cursor.1 >= columns {
            (cursor.0 + 1, 0)
        } else {
            cursor
        };
        if end > row {
            self.pending.push_str(&format!("\x1b[{}A", end - row));
        }
        self.pending.push_str(&format!("\r\x1b[{}G", column + 1));
        self.drawn = Some(row);
    }

    /// The prompt and the input lines; returns where the cursor goes.
    fn draw_input(&self, screen: &mut Screen, columns: usize) -> (usize, usize) {
        let (prompt, masked) = match &self.ask {
            Some(ask) => (self.style("1", &ask.prompt), ask.masked),
            None if self.busy => (self.style("33", "> "), false),
            None => (self.style("36", "> "), false),
        };
        screen.text(&prompt);
        let indent = " ".repeat(screen.column.min(8));
        if self.editor.is_empty() {
            let at = screen.position();
            let placeholder = if self.ask.is_some() {
                ""
            } else if self.busy {
                "(输入文字可引导当前任务；/命令会排队执行)"
            } else {
                if cfg!(windows) {
                    "(/ 查看命令，@路径 或 Alt+V 添加附件，Ctrl+J 换行)"
                } else {
                    "(/ 查看命令，@路径 添加附件，Ctrl+J 换行)"
                }
            };
            screen.text(&self.style("2", &clip(placeholder, columns.saturating_sub(at.1 + 1))));
            return at;
        }
        let shown: Vec<char> = if masked {
            vec!['*'; self.editor.buffer.len()]
        } else {
            self.editor.buffer.clone()
        };
        // A tall input shows the lines around the cursor.
        let lines: Vec<&[char]> = shown.split(|c| *c == '\n').collect();
        let cursor_line = self.editor.buffer[..self.editor.cursor]
            .iter()
            .filter(|c| **c == '\n')
            .count();
        let first = cursor_line.saturating_sub(INPUT_ROWS - 1);
        let mut at = screen.position();
        let mut offset: usize = lines[..first].iter().map(|line| line.len() + 1).sum();
        for (index, line) in lines.iter().enumerate().skip(first).take(INPUT_ROWS) {
            if index > first {
                screen.newline();
                screen.text(&indent);
            }
            for (column, c) in line.iter().enumerate() {
                if offset + column == self.editor.cursor {
                    at = screen.position();
                }
                screen.text(&visible(&c.to_string()));
            }
            if offset + line.len() == self.editor.cursor {
                at = screen.position();
            }
            offset += line.len() + 1;
        }
        at
    }

    fn status(&self) -> Option<String> {
        let mut parts = Vec::new();
        if self.esc_armed.is_some() {
            parts.push("再按一次 Esc 中断任务（或按 Ctrl+C）".to_owned());
        } else if self.ask.is_some() {
            parts.push("等待你的回答".to_owned());
        } else if self.busy {
            let elapsed = self.started.elapsed().as_secs_f64();
            let activity = if self.activity.is_empty() {
                "运行中"
            } else {
                &self.activity
            };
            let mut text = format!(
                "{} {activity}… {}",
                SPINNER[(self.frame as usize) % SPINNER.len()],
                crate::app::render::duration(elapsed)
            );
            if self.tokens != (0, 0) {
                text.push_str(&format!(
                    " · ↑{} ↓{} tokens",
                    crate::app::render::short_count(self.tokens.0),
                    crate::app::render::short_count(self.tokens.1)
                ));
            }
            text.push_str(" · 按两次 Esc 中断");
            if self.steered > 0 {
                text.push_str(&format!("；{} 条引导待送达", self.steered));
            }
            if self.queued > 0 {
                text.push_str(&format!("；{} 条排队", self.queued));
            }
            parts.push(text);
        }
        if self.search.is_some() {
            parts.push("Ctrl+R 更早的匹配，Enter 选用，Esc 取消".to_owned());
        } else if !self.candidates.is_empty() {
            parts.push("上/下 选择，Tab 补全".to_owned());
        }
        if !self.attachments.is_empty() {
            let mut shown: Vec<String> = self
                .attachments
                .iter()
                .take(4)
                .enumerate()
                .map(|(index, (_, description))| format!("#{} {description}", index + 1))
                .collect();
            if self.attachments.len() > 4 {
                shown.push("...".into());
            }
            parts.push(format!(
                "附件（{}）：{}（Alt+Backspace 移除）",
                self.attachments.len(),
                shown.join(" | ")
            ));
        }
        if self.choices.is_some() {
            parts.push("输入可筛选，上/下 选择，Enter 打开，Esc 关闭".to_owned());
        }
        if let Some(hint) = &self.hint {
            parts.push(hint.clone());
        }
        (!parts.is_empty()).then(|| {
            format!(
                "  {}",
                visible(&parts.join(" | ")).replace(['\n', '\t'], " ")
            )
        })
    }

    /// The picker: a title with the position, five entries around the selection.
    fn choice_rows(&self, columns: usize) -> Vec<String> {
        let Some(choices) = &self.choices else {
            return Vec::new();
        };
        let query = self.editor.buffer.iter().collect::<String>().to_lowercase();
        let visible = filter(&choices.items, &query);
        if visible.is_empty() {
            return vec![self.style(
                "2",
                &clip(
                    &format!(
                        "{} | 无匹配，按 Backspace 修改筛选或 Esc 关闭",
                        choices.title
                    ),
                    columns - 1,
                ),
            )];
        }
        let selected = choices.selected.min(visible.len() - 1);
        let mut rows = vec![self.style(
            "2",
            &clip(
                &format!("{} | {}/{}", choices.title, selected + 1, visible.len()),
                columns - 1,
            ),
        )];
        let start = selected
            .saturating_sub(2)
            .min(visible.len().saturating_sub(5));
        for (offset, index) in visible.iter().enumerate().skip(start).take(5) {
            let item = &choices.items[*index];
            let label = clip(&visible_text(&item.label), columns - 3);
            let room = columns.saturating_sub(3 + width(&label) + 3);
            let description = clip(&visible_text(&item.description), room);
            let entry = if offset == selected {
                format!("> {}", self.style("7", &label))
            } else {
                format!("  {label}")
            };
            if description.is_empty() {
                rows.push(entry);
            } else {
                rows.push(format!("{entry} | {}", self.style("2", &description)));
            }
        }
        rows
    }

    /// The agent panel (Alt+M), at most eight rows.
    fn agent_rows(&self, columns: usize) -> Vec<String> {
        let Some(lines) = &self.panel else {
            return Vec::new();
        };
        let mut rows = vec![self.style("2", &clip("子代理 | Alt+M 或 Esc 关闭", columns - 1))];
        rows.push(format!(
            "主任务：{}",
            if self.busy { "运行中" } else { "空闲" }
        ));
        let lines: Vec<&String> = lines
            .iter()
            .filter(|line| !line.trim().is_empty())
            .collect();
        if lines.is_empty() {
            rows.push("没有子代理".into());
        }
        for line in lines.into_iter().take(6) {
            rows.push(clip(&visible_text(line), columns - 1));
        }
        rows
    }

    /// Candidate rows: the selection marked and reversed, descriptions dimmed,
    /// each within one terminal row.
    fn panel(&self, columns: usize) -> Vec<String> {
        if self.candidates.is_empty() || self.search.is_some() {
            return Vec::new();
        }
        let start = self.selected.saturating_sub(PANEL_ROWS - 1);
        let shown = &self.candidates[start..self.candidates.len().min(start + PANEL_ROWS)];
        let label = shown
            .iter()
            .map(|candidate| width(&candidate.text))
            .max()
            .unwrap_or(0)
            .min(32);
        let mut rows = Vec::new();
        for (index, candidate) in shown.iter().enumerate() {
            let text = clip(&visible(&candidate.text), columns - 3);
            let pad = " ".repeat(label.saturating_sub(width(&text)));
            let room = columns.saturating_sub(3 + width(&text) + pad.len() + 2);
            let description = clip(&visible(&candidate.description), room);
            let entry = if start + index == self.selected {
                format!("> {}", self.style("7", &text))
            } else {
                format!("  {text}")
            };
            rows.push(format!("{entry}{pad}  {}", self.style("2", &description)));
        }
        let more = self.candidates.len() - shown.len();
        if more > 0 {
            rows.push(format!("  {}", self.style("2", &format!("还有 {more} 项"))));
        }
        rows
    }
}

/// Indexes of the choices whose label or description contains `query`.
fn filter(items: &[Choice], query: &str) -> Vec<usize> {
    items
        .iter()
        .enumerate()
        .filter(|(_, item)| {
            format!("{} {}", item.label, item.description)
                .to_lowercase()
                .contains(query)
        })
        .map(|(index, _)| index)
        .collect()
}

/// Untrusted text for a single row: control characters (ESC included) shown as
/// U+FFFD, tabs and newlines as spaces.
fn visible_text(text: &str) -> String {
    text.chars()
        .map(|c| match c {
            '\n' | '\t' => ' ',
            c if c.is_control() => '\u{fffd}',
            c => c,
        })
        .collect()
}

/// `COVE_ESC_INTERRUPT=0` keeps a bare Esc from interrupting tasks.
fn esc_interrupts() -> bool {
    !std::env::var("COVE_ESC_INTERRUPT").is_ok_and(|value| value.trim() == "0")
}

/// One-key answers from a prompt's trailing `[y/N/a=always ...]`. The default
/// (upper case) is left out: Enter gives it, and typing it leaves room for more
/// (`n <reason>`).
fn answer_keys(prompt: &str) -> Vec<char> {
    let trimmed = prompt.trim_end();
    let Some(inner) = trimmed
        .strip_suffix(']')
        .and_then(|rest| rest.rfind('[').map(|at| &rest[at + 1..]))
    else {
        return Vec::new();
    };
    inner
        .split('/')
        .filter_map(|token| {
            let token = token.trim();
            let first = token.chars().next()?;
            let single = token.len() == 1 || token[first.len_utf8()..].starts_with('=');
            (first.is_ascii_alphanumeric() && !first.is_ascii_uppercase() && single)
                .then_some(first)
        })
        .collect()
}

/// Numbered option lines (`  2. SQLite - embedded`) as (number, label).
fn options(text: &str) -> Vec<(String, String)> {
    text.lines()
        .filter_map(|line| {
            let line = line.trim_start();
            let (number, rest) = line.split_once(". ")?;
            if number.is_empty() || !number.chars().all(|c| c.is_ascii_digit()) {
                return None;
            }
            let label = rest.split(" - ").next().unwrap_or(rest).trim();
            Some((number.to_owned(), label.to_owned()))
        })
        .collect()
}

/// Lays text out the way the terminal will: rows and columns from the block's
/// start, wrapping at the margin.
struct Screen {
    columns: usize,
    text: String,
    row: usize,
    column: usize,
}

impl Screen {
    fn new(columns: usize) -> Self {
        Self {
            columns,
            text: String::new(),
            row: 0,
            column: 0,
        }
    }

    fn position(&self) -> (usize, usize) {
        (self.row, self.column)
    }

    /// Appends text without newlines; control sequences take no room.
    fn text(&mut self, text: &str) {
        let mut escape = false;
        for c in text.chars() {
            self.text.push(c);
            if escape {
                escape = !c.is_ascii_alphabetic();
                continue;
            }
            if c == '\x1b' {
                escape = true;
                continue;
            }
            let width = c.width().unwrap_or(0);
            if self.column + width > self.columns {
                self.row += 1;
                self.column = 0;
            }
            self.column += width;
        }
    }

    fn newline(&mut self) {
        self.text.push_str("\r\n");
        self.row += 1;
        self.column = 0;
    }

    /// The text to write and the row the terminal's cursor ends on.
    fn finish(mut self) -> (String, usize) {
        if self.column >= self.columns {
            self.newline();
        }
        (self.text, self.row)
    }
}

/// The line being typed; `cursor` indexes `buffer`.
#[derive(Default)]
struct Editor {
    buffer: Vec<char>,
    cursor: usize,
}

impl Editor {
    fn is_empty(&self) -> bool {
        self.buffer.is_empty()
    }

    fn take(&mut self) -> Vec<char> {
        self.cursor = 0;
        std::mem::take(&mut self.buffer)
    }

    fn restore(&mut self, buffer: Vec<char>) {
        self.cursor = buffer.len();
        self.buffer = buffer;
    }

    fn before_cursor(&self) -> String {
        self.buffer[..self.cursor].iter().collect()
    }

    fn insert(&mut self, c: char) {
        self.buffer.insert(self.cursor, c);
        self.cursor += 1;
    }

    /// Pasted text: newlines kept (CR LF as one), tabs as spaces, other control
    /// characters dropped.
    fn insert_text(&mut self, text: &str) {
        let text = text.replace("\r\n", "\n").replace('\r', "\n");
        for c in text.chars() {
            if self.buffer.len() >= LINE_LIMIT {
                break;
            }
            match c {
                '\t' => self.insert(' '),
                '\n' => self.insert('\n'),
                c if !c.is_control() => self.insert(c),
                _ => {}
            }
        }
    }

    /// Grapheme cluster boundaries of the buffer as char indexes, so an emoji
    /// with its selector or a letter with combining marks moves as one.
    fn boundaries(&self) -> Vec<usize> {
        let text: String = self.buffer.iter().collect();
        let mut at = 0;
        let mut bounds = vec![0];
        for cluster in text.graphemes(true) {
            at += cluster.chars().count();
            bounds.push(at);
        }
        bounds
    }

    fn cluster_left(&self) -> usize {
        self.boundaries()
            .into_iter()
            .rev()
            .find(|at| *at < self.cursor)
            .unwrap_or(0)
    }

    fn cluster_right(&self) -> usize {
        self.boundaries()
            .into_iter()
            .find(|at| *at > self.cursor)
            .unwrap_or(self.buffer.len())
    }

    fn backspace(&mut self) {
        let start = self.cluster_left();
        self.buffer.drain(start..self.cursor);
        self.cursor = start;
    }

    fn delete(&mut self) {
        let end = self.cluster_right();
        self.buffer.drain(self.cursor..end);
    }

    fn word_left(&self) -> usize {
        let mut at = self.cursor;
        while at > 0 && !is_word(self.buffer[at - 1]) {
            at -= 1;
        }
        while at > 0 && is_word(self.buffer[at - 1]) {
            at -= 1;
        }
        at
    }

    fn word_right(&self) -> usize {
        let mut at = self.cursor;
        let len = self.buffer.len();
        while at < len && !is_word(self.buffer[at]) {
            at += 1;
        }
        while at < len && is_word(self.buffer[at]) {
            at += 1;
        }
        at
    }

    fn delete_word_back(&mut self) {
        let start = self.word_left();
        self.buffer.drain(start..self.cursor);
        self.cursor = start;
    }

    fn kill_to_start(&mut self) {
        let start = self.line_start();
        self.buffer.drain(start..self.cursor);
        self.cursor = start;
    }

    fn kill_to_end(&mut self) {
        let end = self.line_end();
        self.buffer.drain(self.cursor..end);
    }

    /// Start of the cursor's line within a multi-line input.
    fn line_start(&self) -> usize {
        self.buffer[..self.cursor]
            .iter()
            .rposition(|c| *c == '\n')
            .map_or(0, |at| at + 1)
    }

    fn line_end(&self) -> usize {
        self.buffer[self.cursor..]
            .iter()
            .position(|c| *c == '\n')
            .map_or(self.buffer.len(), |at| self.cursor + at)
    }

    /// The cursor one line up or down in a multi-line input; `None` on the
    /// first (last) line, where Up (Down) walks the history instead.
    fn vertical(&self, up: bool) -> Option<usize> {
        let start = self.line_start();
        let column = self.cursor - start;
        if up {
            if start == 0 {
                return None;
            }
            let previous_end = start - 1;
            let previous_start = self.buffer[..previous_end]
                .iter()
                .rposition(|c| *c == '\n')
                .map_or(0, |at| at + 1);
            Some(previous_start + column.min(previous_end - previous_start))
        } else {
            let end = self.line_end();
            if end == self.buffer.len() {
                return None;
            }
            let next_start = end + 1;
            let next_end = self.buffer[next_start..]
                .iter()
                .position(|c| *c == '\n')
                .map_or(self.buffer.len(), |at| next_start + at);
            Some(next_start + column.min(next_end - next_start))
        }
    }

    /// Replaces the word before the cursor with `text`; `whole` also replaces the
    /// rest of the word after the cursor.
    fn replace_word(&mut self, text: &str, whole: bool) {
        let line = self.before_cursor();
        let start = self.cursor - completion::current_word(&line).chars().count();
        let mut end = self.cursor;
        while whole && self.buffer.get(end).is_some_and(|c| !c.is_whitespace()) {
            end += 1;
        }
        self.buffer.splice(start..end, text.chars());
        self.cursor = start + text.chars().count();
    }
}

/// Letters (CJK included), digits and `_`.
fn is_word(c: char) -> bool {
    c.is_alphanumeric() || c == '_'
}

/// Typed lines, oldest first, in a file of one JSON string per line (so
/// multi-line entries survive).
struct History {
    lines: Vec<String>,
    path: Option<PathBuf>,
    /// The entry Up/Down shows, and the draft it replaced.
    at: Option<usize>,
    draft: Vec<char>,
}

impl History {
    fn load(path: Option<PathBuf>) -> Self {
        let mut lines: Vec<String> = path
            .as_ref()
            .and_then(|path| std::fs::read_to_string(path).ok())
            .map(|text| {
                text.lines()
                    .filter_map(|line| serde_json::from_str::<String>(line).ok())
                    .filter(|line| keeps(line))
                    .collect()
            })
            .unwrap_or_default();
        let overflow = lines.len().saturating_sub(HISTORY_LIMIT);
        lines.drain(..overflow);
        Self {
            lines,
            path,
            at: None,
            draft: Vec::new(),
        }
    }

    fn reset(&mut self) {
        self.at = None;
    }

    /// The line Up (or Down) shows, or `None` when there is nothing further.
    fn step(&mut self, up: bool, current: &[char]) -> Option<Vec<char>> {
        let next = match (self.at, up) {
            (None, true) if !self.lines.is_empty() => {
                self.draft = current.to_vec();
                self.lines.len() - 1
            }
            (Some(at), true) if at > 0 => at - 1,
            (Some(at), false) if at + 1 < self.lines.len() => at + 1,
            (Some(_), false) => {
                self.at = None;
                return Some(std::mem::take(&mut self.draft));
            }
            _ => return None,
        };
        self.at = Some(next);
        Some(self.lines[next].chars().collect())
    }

    /// The newest entry before index `before` containing `query`, ignoring case.
    fn find(&self, query: &str, before: usize) -> Option<usize> {
        let query = query.to_lowercase();
        self.lines[..before.min(self.lines.len())]
            .iter()
            .rposition(|line| line.to_lowercase().contains(&query))
    }

    fn push(&mut self, line: &str) {
        self.at = None;
        if !keeps(line) || self.lines.last().is_some_and(|last| last == line) {
            return;
        }
        self.lines.push(line.to_owned());
        let overflow = self.lines.len().saturating_sub(HISTORY_LIMIT);
        self.lines.drain(..overflow);
        if let Some(path) = &self.path {
            let _ = self.save(path, line);
        }
    }

    /// Appends the entry; a file grown past its limit is rewritten with the
    /// entries kept in memory.
    fn save(&self, path: &std::path::Path, line: &str) -> std::io::Result<()> {
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        let large = std::fs::metadata(path).is_ok_and(|meta| meta.len() > HISTORY_FILE_LIMIT);
        if large {
            let text: String = self
                .lines
                .iter()
                .filter_map(|line| serde_json::to_string(line).ok())
                .map(|line| line + "\n")
                .collect();
            let parent = path.parent().unwrap_or(std::path::Path::new("."));
            let mut file = tempfile::NamedTempFile::new_in(parent)?;
            file.write_all(text.as_bytes())?;
            return crate::session::replace_with(file, path);
        }
        let mut options = std::fs::OpenOptions::new();
        options.create(true).append(true);
        #[cfg(unix)]
        std::os::unix::fs::OpenOptionsExt::mode(&mut options, 0o600);
        let mut file = options.open(path)?;
        let json = serde_json::to_string(line).map_err(std::io::Error::other)?;
        file.write_all(format!("{json}\n").as_bytes())
    }
}

/// Whether a line may be kept: no keys, tokens or passwords in it.
fn keeps(line: &str) -> bool {
    if line.trim().is_empty() {
        return false;
    }
    let lower = line.trim_start().to_lowercase();
    let mut words = lower.split_whitespace();
    let command = words.next().unwrap_or_default();
    let key = words.next().unwrap_or_default();
    const SECRET_KEYS: [&str; 6] = [
        "api_key", "api-key", "apikey", "token", "secret", "password",
    ];
    if command == "/api-key"
        || (command == "/config" && SECRET_KEYS.iter().any(|secret| key.contains(secret)))
    {
        return false;
    }
    if (command == "/base-url" || (command == "/config" && key == "base_url"))
        && lower.split("://").nth(1).is_some_and(|rest| {
            rest.split('/')
                .next()
                .is_some_and(|host| host.contains('@'))
        })
    {
        return false;
    }
    !crate::extensions::skill_review::contains_secret(line)
        && crate::safety::permission::safety::secrets(line).is_empty()
}

fn width(text: &str) -> usize {
    text.chars().map(|c| c.width().unwrap_or(0)).sum()
}

/// `text` cut to at most `room` columns.
fn clip(text: &str, room: usize) -> String {
    let mut used = 0;
    text.chars()
        .take_while(|c| {
            used += c.width().unwrap_or(0);
            used <= room
        })
        .collect()
}

/// Control characters other than newline, tab and ESC shown as U+FFFD; the
/// callers have already neutralized ESC in untrusted text.
fn visible(text: &str) -> String {
    text.chars()
        .map(|c| {
            if c.is_control() && c != '\n' && c != '\t' && c != '\x1b' {
                '\u{fffd}'
            } else {
                c
            }
        })
        .collect()
}

/// Turns on control-sequence processing for the console; false where the
/// console cannot do it (then the REPL reads plain lines).
#[cfg(windows)]
fn enable_virtual_terminal() -> bool {
    use windows_sys::Win32::System::Console::{
        ENABLE_PROCESSED_OUTPUT, ENABLE_VIRTUAL_TERMINAL_PROCESSING, GetConsoleMode, GetStdHandle,
        STD_ERROR_HANDLE, SetConsoleMode,
    };
    // SAFETY: a standard handle and a local out-parameter.
    unsafe {
        let handle = GetStdHandle(STD_ERROR_HANDLE);
        let mut mode = 0;
        if GetConsoleMode(handle, &mut mode) == 0 {
            return false;
        }
        SetConsoleMode(
            handle,
            mode | ENABLE_PROCESSED_OUTPUT | ENABLE_VIRTUAL_TERMINAL_PROCESSING,
        ) != 0
    }
}

#[cfg(not(windows))]
fn enable_virtual_terminal() -> bool {
    true
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_usage_row_names_context_session_and_when_to_compact() {
        let mut meter = Meter {
            context: 52_100,
            window: 200_000,
            compact_at: 127_500,
            session: 1_250_000,
            cost: Some(3.2),
        };
        assert_eq!(
            meter.row(),
            Some((
                "  上下文 52.1k/200k（26%）· 本会话 1.2M tokens · $3.20".into(),
                false
            ))
        );
        meter.context = 110_000;
        let (row, near) = meter.row().unwrap();
        assert!(
            near && row.ends_with("快到 127.5k 自动压缩了，可先输入 /compact"),
            "{row}"
        );
        assert_eq!(Meter::default().row(), None);
    }

    fn state() -> (State, mpsc::UnboundedReceiver<Input>) {
        let (sender, receiver) = mpsc::unbounded_channel();
        let state = State::new(sender, History::load(None));
        (state, receiver)
    }

    fn press(state: &mut State, code: KeyCode, modifiers: KeyModifiers) -> bool {
        matches!(state.key(KeyEvent::new(code, modifiers)), Action::Interrupt)
    }

    fn typed(state: &mut State, text: &str) {
        for c in text.chars() {
            press(state, KeyCode::Char(c), KeyModifiers::NONE);
        }
    }

    fn line(state: &State) -> String {
        state.editor.buffer.iter().collect()
    }

    #[test]
    fn lines_queue_steer_and_interrupt_while_busy() {
        let (mut state, mut lines) = state();
        typed(&mut state, "hello");
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        assert_eq!(
            lines.try_recv(),
            Ok(Input::Line("hello".into(), Vec::new()))
        );
        state.busy = true;
        typed(&mut state, "use the v2 API");
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        assert!(lines.try_recv().is_err());
        assert_eq!(
            state.steer.front().map(String::as_str),
            Some("use the v2 API")
        );
        typed(&mut state, "/status");
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        assert_eq!(
            lines.try_recv(),
            Ok(Input::Line("/status".into(), Vec::new()))
        );
        // Esc twice on an empty line interrupts; once only arms.
        assert!(!press(&mut state, KeyCode::Esc, KeyModifiers::NONE));
        assert!(press(&mut state, KeyCode::Esc, KeyModifiers::NONE));
        assert!(press(&mut state, KeyCode::Char('c'), KeyModifiers::CONTROL));
        state.busy = false;
        assert!(!press(
            &mut state,
            KeyCode::Char('c'),
            KeyModifiers::CONTROL
        ));
        assert!(state.hint.as_deref().unwrap().contains("/exit"));
        press(&mut state, KeyCode::Char('d'), KeyModifiers::CONTROL);
        assert_eq!(lines.try_recv(), Ok(Input::Closed));
    }

    #[test]
    fn editing_keys_move_by_words_clusters_and_lines() {
        let (mut state, _lines) = state();
        typed(&mut state, "fix the 档案 bug");
        press(&mut state, KeyCode::Char('w'), KeyModifiers::CONTROL);
        assert_eq!(line(&state), "fix the 档案 ");
        press(&mut state, KeyCode::Left, KeyModifiers::CONTROL);
        assert_eq!(state.editor.cursor, 8);
        press(&mut state, KeyCode::Char('k'), KeyModifiers::CONTROL);
        assert_eq!(line(&state), "fix the ");
        press(&mut state, KeyCode::Esc, KeyModifiers::NONE);
        assert_eq!(line(&state), "");
        press(&mut state, KeyCode::Char('z'), KeyModifiers::CONTROL);
        assert_eq!(line(&state), "fix the ");
        // An emoji with its selector is one step.
        state.editor.restore("a⚠\u{fe0f}".chars().collect());
        press(&mut state, KeyCode::Backspace, KeyModifiers::NONE);
        assert_eq!(line(&state), "a");
        // Multi-line: Ctrl+J, a trailing backslash, Up within the input.
        state.editor.restore("one".chars().collect());
        press(&mut state, KeyCode::Char('j'), KeyModifiers::CONTROL);
        typed(&mut state, "two\\");
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        typed(&mut state, "three");
        assert_eq!(line(&state), "one\ntwo\nthree");
        press(&mut state, KeyCode::Up, KeyModifiers::NONE);
        assert_eq!(state.editor.cursor, "one\ntwo".len());
        press(&mut state, KeyCode::Char('u'), KeyModifiers::CONTROL);
        assert_eq!(line(&state), "one\n\nthree");
        let mut editor = Editor::default();
        editor.insert_text("a\r\nb\tc\x07");
        assert_eq!(editor.buffer.iter().collect::<String>(), "a\nb c");
    }

    #[test]
    fn history_walks_searches_and_keeps_no_secrets() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("input_history.jsonl");
        let (sender, _lines) = mpsc::unbounded_channel();
        let mut state = State::new(sender, History::load(Some(path.clone())));
        for entry in [
            "first",
            "second\nline",
            "second\nline",
            "/config api_key sk-abc",
            "third",
        ] {
            state.editor.restore(entry.chars().collect());
            press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        }
        assert_eq!(state.history.lines, ["first", "second\nline", "third"]);
        typed(&mut state, "draft");
        press(&mut state, KeyCode::Up, KeyModifiers::NONE);
        assert_eq!(line(&state), "third");
        press(&mut state, KeyCode::Up, KeyModifiers::NONE);
        assert_eq!(line(&state), "second\nline");
        press(&mut state, KeyCode::Down, KeyModifiers::NONE);
        press(&mut state, KeyCode::Down, KeyModifiers::NONE);
        assert_eq!(line(&state), "draft");
        press(&mut state, KeyCode::Char('r'), KeyModifiers::CONTROL);
        typed(&mut state, "fir");
        press(&mut state, KeyCode::Right, KeyModifiers::NONE);
        assert_eq!(line(&state), "first");
        // The file survives a restart.
        let reloaded = History::load(Some(path));
        assert_eq!(reloaded.lines, ["first", "second\nline", "third"]);
        assert!(!keeps("/config token abc"));
        assert!(!keeps("/base-url https://user:pw@host/v1"));
        assert!(keeps("/base-url https://host/v1"));
    }

    #[test]
    fn questions_take_one_key_answers_and_options() {
        let (mut state, _lines) = state();
        typed(&mut state, "draft");
        let mut reply = state.ask("Approve shell `ls`? [y/N/a=always in this project] ", false);
        assert_eq!(state.ask.as_ref().unwrap().keys, ['y', 'a']);
        typed(&mut state, "a");
        assert_eq!(reply.try_recv().unwrap().unwrap(), "a");
        assert_eq!(line(&state), "draft");
        let prompt = "[Q1/1]\nWhich DB?\n  1. Postgres - server\n  2. SQLite\nAnswer with a number or your own text\n> ";
        let mut reply = state.ask(prompt, false);
        state.editor.take();
        press(&mut state, KeyCode::Down, KeyModifiers::NONE);
        press(&mut state, KeyCode::Down, KeyModifiers::NONE);
        assert_eq!(line(&state), "SQLite");
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        assert_eq!(reply.try_recv().unwrap().unwrap(), "2");
        let mut reply = state.ask(prompt, false);
        state.editor.take();
        typed(&mut state, "1");
        assert_eq!(reply.try_recv().unwrap().unwrap(), "1");
        let mut reply = state.ask("Key: ", true);
        press(&mut state, KeyCode::Esc, KeyModifiers::NONE);
        press(&mut state, KeyCode::Esc, KeyModifiers::NONE);
        assert!(reply.try_recv().unwrap().is_err());
    }

    #[test]
    fn pickers_filter_and_submit_and_pasted_images_attach() {
        let (mut state, mut lines) = state();
        state.choices = Some(Choices {
            title: "Resume".into(),
            items: vec![
                Choice {
                    value: "/resume aaa".into(),
                    label: "1. fix login".into(),
                    description: String::new(),
                },
                Choice {
                    value: "/resume bbb".into(),
                    label: "2. add search".into(),
                    description: "idle".into(),
                },
            ],
            selected: 0,
        });
        typed(&mut state, "sea");
        assert_eq!(filter(&state.choices.as_ref().unwrap().items, "sea"), [1]);
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        assert_eq!(
            lines.try_recv(),
            Ok(Input::Line("/resume bbb".into(), Vec::new()))
        );
        assert!(state.choices.is_none());

        let dir = tempfile::tempdir().unwrap();
        let image = dir.path().join("shot.png");
        let mut bytes = Vec::new();
        let mut encoder = png::Encoder::new(&mut bytes, 1, 1);
        encoder.set_color(png::ColorType::Rgba);
        encoder
            .write_header()
            .unwrap()
            .write_image_data(&[0; 4])
            .unwrap();
        std::fs::write(&image, bytes).unwrap();
        state.cwd = dir.path().to_owned();
        typed(&mut state, "see ");
        let (before, cursor) = (state.editor.buffer.clone(), state.editor.cursor);
        typed(&mut state, "shot.png");
        state.burst_ended(before, cursor);
        assert_eq!(line(&state), "see ");
        assert_eq!(state.attachments.len(), 1);
        state.editor.take();
        press(&mut state, KeyCode::Enter, KeyModifiers::NONE);
        assert_eq!(
            lines.try_recv(),
            Ok(Input::Line(
                "Please analyze these attachments.".into(),
                vec![image]
            ))
        );
        assert!(state.attachments.is_empty());
    }

    #[test]
    fn screen_layout_wraps_like_the_terminal() {
        let mut screen = Screen::new(10);
        screen.text("\x1b[36m> \x1b[0m12345678");
        assert_eq!(screen.position(), (0, 10));
        screen.text("9");
        assert_eq!(screen.position(), (1, 1));
        screen.text("档案档案档");
        assert_eq!(screen.position(), (2, 2));
        let (_, end) = screen.finish();
        assert_eq!(end, 2);
        assert_eq!(answer_keys("Continue? [c/s]"), ['c', 's']);
        assert!(answer_keys("Run it?").is_empty());
    }
}
