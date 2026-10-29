use std::{
    io::{IsTerminal, Write},
    time::Duration,
};

use crossterm::{
    ExecutableCommand,
    cursor::MoveLeft,
    event::{self, Event, KeyCode, KeyEventKind, KeyModifiers},
    terminal::{self, Clear, ClearType},
};
use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;
use unicode_width::UnicodeWidthChar;

use crate::{
    app::{completion::Completer, console},
    domain::{Error, Event as AgentEvent, Result, UserInput},
    engine::agent::TaskReport,
};

pub struct ConsoleInput;

impl UserInput for ConsoleInput {
    fn answer(&self, prompt: String, cancel: CancellationToken) -> BoxFuture<'_, Result<String>> {
        Box::pin(async move {
            if let Some(console) = console::active() {
                let prompt = if console::rich() {
                    approval_box(&prompt)
                } else {
                    prompt
                };
                return tokio::select! {
                    answer = console.ask(&prompt, false) => answer,
                    () = cancel.cancelled() => {
                        console.cancel_ask();
                        Err(Error::Cancelled)
                    }
                };
            }
            if !std::io::stdin().is_terminal() || !std::io::stderr().is_terminal() {
                return Err(Error::Approval("console input requires a terminal".into()));
            }
            tokio::task::spawn_blocking(move || read_answer(&prompt, &cancel, false))
                .await
                .map_err(|err| Error::Tool(format!("console input failed: {err}")))?
        })
    }
}

/// An approval question's leading lines framed in yellow, warnings in red; the
/// last line (where the answer is typed) and other questions stay as they are.
fn approval_box(prompt: &str) -> String {
    let Some((head, tail)) = prompt.rsplit_once('\n') else {
        return prompt.to_owned();
    };
    if !head.starts_with("需要批准") {
        return prompt.to_owned();
    }
    let mut out = String::from("\n");
    for (index, line) in head.lines().enumerate() {
        if index == 0 {
            out.push_str(&format!("\x1b[33m┌\x1b[0m \x1b[1;33m{line}\x1b[0m\n"));
        } else if line.starts_with("警告") {
            out.push_str(&format!("\x1b[33m│\x1b[0m \x1b[31m{line}\x1b[0m\n"));
        } else {
            out.push_str(&format!("\x1b[33m│\x1b[0m {line}\n"));
        }
    }
    let always = if tail.contains("a=") {
        "a 本项目内总是允许 · "
    } else {
        ""
    };
    out.push_str(&format!(
        "\x1b[33m└\x1b[0m \x1b[2my 允许 · {always}回车 拒绝 · n 加理由：拒绝并告诉模型原因\x1b[0m\n{tail}"
    ));
    out
}

struct RawMode;

impl Drop for RawMode {
    fn drop(&mut self) {
        if let Err(err) = terminal::disable_raw_mode() {
            eprintln!("无法恢复终端输入模式：{err}；如果终端显示异常，请关闭后重新打开它");
        }
    }
}

/// Blocking terminal prompt for startup setup; `masked` echoes `*` (for API keys).
pub fn prompt_line(prompt: &str, masked: bool) -> Result<String> {
    if let Some(console) = console::active() {
        return console.ask_blocking(prompt, masked);
    }
    if !std::io::stdin().is_terminal() || !std::io::stderr().is_terminal() {
        return Err(Error::Approval(
            "这里需要在终端里输入；请在终端窗口中运行 cove，或先写好配置文件（见 docs/config.example.json）".into(),
        ));
    }
    read_answer(prompt, &CancellationToken::new(), masked)
}

fn read_answer(prompt: &str, cancel: &CancellationToken, masked: bool) -> Result<String> {
    let mut output = std::io::stderr();
    write!(output, "\n{}", visible(prompt))?;
    output.flush()?;
    terminal::enable_raw_mode()?;
    let _guard = RawMode;
    let mut answer = String::new();
    loop {
        if cancel.is_cancelled() {
            write!(output, "\r\n")?;
            return Err(Error::Cancelled);
        }
        if !event::poll(Duration::from_millis(50))? {
            continue;
        }
        if let Event::Key(key) = event::read()? {
            if key.kind == KeyEventKind::Release {
                continue;
            }
            match key.code {
                KeyCode::Enter => {
                    write!(output, "\r\n")?;
                    return Ok(answer);
                }
                KeyCode::Esc => {
                    cancel.cancel();
                }
                KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => {
                    cancel.cancel();
                }
                KeyCode::Char('d')
                    if key.modifiers.contains(KeyModifiers::CONTROL) && answer.is_empty() =>
                {
                    write!(output, "\r\n")?;
                    return Err(Error::InputClosed);
                }
                KeyCode::Char(c) if !c.is_control() && answer.len() < 4096 => {
                    answer.push(c);
                    if masked {
                        write!(output, "*")?;
                    } else {
                        write!(output, "{c}")?;
                    }
                }
                KeyCode::Backspace => {
                    if let Some(last) = answer.pop() {
                        let width = if masked {
                            1
                        } else {
                            last.width().unwrap_or(0) as u16
                        };
                        if width > 0 {
                            output
                                .execute(MoveLeft(width))?
                                .execute(Clear(ClearType::UntilNewLine))?;
                        }
                    }
                }
                _ => {}
            }
            output.flush()?;
        }
    }
}

pub fn visible(text: &str) -> String {
    text.chars()
        .map(|c| {
            if c.is_control() && c != '\n' && c != '\t' {
                '\u{fffd}'
            } else {
                c
            }
        })
        .collect()
}

static SHOW_REASONING: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);

/// `show_reasoning`: print reasoning text live (to stderr) instead of hiding it.
pub fn set_show_reasoning(show: bool) {
    SHOW_REASONING.store(show, std::sync::atomic::Ordering::Relaxed);
}

pub async fn consume_events(
    mut receiver: tokio::sync::mpsc::Receiver<AgentEvent>,
    cancel: CancellationToken,
) -> std::io::Result<()> {
    if console::rich() {
        let result = rich_events(receiver).await;
        if result.is_err() {
            cancel.cancel();
        }
        return result;
    }
    let result = async {
        let err = |text: String| console::write(false, &text);
        // Output already shown live is not repeated when the tool finishes.
        let mut streamed = false;
        while let Some(event) = receiver.recv().await {
            match event {
                AgentEvent::Output {
                    line,
                    stderr: error,
                } => {
                    streamed = true;
                    err(format!(
                        "  {} {}\n",
                        if error { "!" } else { "|" },
                        visible(&line)
                    ))?;
                }
                AgentEvent::ToolFinished {
                    name,
                    failed,
                    output,
                } if std::mem::take(&mut streamed) => {
                    let id = crate::app::blocks::BLOCKS.add(&name, failed, &output);
                    err(format!(
                        "[{}] {}: {}  #{id}\n",
                        if failed { "failed" } else { "ok" },
                        visible(&name),
                        visible(output.lines().next().unwrap_or_default())
                    ))?
                }
                AgentEvent::Notice(text) => err(format!("\n[notice] {}\n", visible(&text)))?,
                AgentEvent::Text(text) => console::write(true, &visible(&text))?,
                AgentEvent::Reasoning(text) => {
                    if SHOW_REASONING.load(std::sync::atomic::Ordering::Relaxed) {
                        // Dimmed on a terminal, so it reads as thinking, not the answer.
                        if std::io::stderr().is_terminal() {
                            err(format!("\x1b[2m{}\x1b[0m", visible(&text)))?;
                        } else {
                            err(visible(&text))?;
                        }
                    }
                }
                AgentEvent::ToolStarted { name, .. } => {
                    streamed = false;
                    err(format!("\n[tool] {}\n", visible(&name)))?
                }
                AgentEvent::ToolFinished {
                    name,
                    failed,
                    output,
                } => {
                    // Tool results are shown shortened; `/expand <n>` prints them in full.
                    let id = crate::app::blocks::BLOCKS.add(&name, failed, &output);
                    err(format!(
                        "[{}] {}: {}\n",
                        if failed { "failed" } else { "ok" },
                        visible(&name),
                        visible(&crate::app::blocks::inline(&output, id))
                    ))?
                }
                AgentEvent::Usage { .. } => {}
            }
        }
        console::write(true, "\n")
    }
    .await;
    if result.is_err() {
        cancel.cancel();
    }
    result
}

/// What the console shows last, so each kind of output opens its own block.
#[derive(PartialEq)]
enum Block {
    None,
    Reply,
    Reasoning,
    Tool,
}

/// The console's view of a task: replies under a white `●` with markdown styled,
/// reasoning dim and italic under `✻`, each tool as a cyan `●` title with its result
/// under `└`, and what the task does now in the status row.
async fn rich_events(mut receiver: tokio::sync::mpsc::Receiver<AgentEvent>) -> std::io::Result<()> {
    use crate::app::render;
    const REASONING: &str = "\x1b[2;3m";
    let console = console::active();
    let activity = |text: &str| {
        if let Some(console) = console {
            console.set_activity(text);
        }
    };
    let mut markdown = render::Markdown::default();
    let mut block = Block::None;
    // Whether the last character written ended a line.
    let mut line_done = true;
    // Running tools: name, title and whether the title was written; the title row
    // last written, while nothing followed it except its own live output.
    let mut running: Vec<(String, String, bool)> = Vec::new();
    let mut open: Option<String> = None;
    let mut streamed = false;
    // Hidden reasoning since then; a dim row says how long it took.
    let mut thinking: Option<std::time::Instant> = None;
    activity("思考中");
    // Starts a block of another kind on a fresh row, after a blank row.
    let open_block = |block: &mut Block,
                      next: Block,
                      line_done: &mut bool,
                      markdown: &mut render::Markdown|
     -> std::io::Result<()> {
        let mut out = markdown.finish();
        if !*line_done || !out.is_empty() && !out.ends_with('\n') {
            out.push('\n');
        }
        out.push('\n');
        *line_done = true;
        *block = next;
        console::write(true, &out)
    };
    while let Some(event) = receiver.recv().await {
        if !matches!(event, AgentEvent::Reasoning(_) | AgentEvent::Usage { .. })
            && let Some(since) = thinking.take()
        {
            open_block(&mut block, Block::Reasoning, &mut line_done, &mut markdown)?;
            console::write(false, &thought(since.elapsed().as_secs_f64()))?;
        }
        match event {
            AgentEvent::Text(text) => {
                if block != Block::Reply {
                    open_block(&mut block, Block::Reply, &mut line_done, &mut markdown)?;
                    console::write(true, "\x1b[1;37m●\x1b[0m ")?;
                    activity("回复中");
                    open = None;
                }
                let out = markdown.push(&visible(&text));
                if !out.is_empty() {
                    line_done = out.ends_with('\n');
                    console::write(true, &out)?;
                }
            }
            AgentEvent::Reasoning(text) => {
                activity("思考中");
                if !SHOW_REASONING.load(std::sync::atomic::Ordering::Relaxed) {
                    thinking.get_or_insert_with(std::time::Instant::now);
                    continue;
                }
                if block != Block::Reasoning {
                    open_block(&mut block, Block::Reasoning, &mut line_done, &mut markdown)?;
                    console::write(false, "\x1b[2m✻ 思考过程\x1b[0m\n")?;
                    open = None;
                }
                let text = visible(&text);
                line_done = text.ends_with('\n');
                console::write(false, &format!("{REASONING}{text}\x1b[0m"))?;
            }
            AgentEvent::ToolStarted { name, input } => {
                // The title is written with the first live output or the result,
                // so calls running side by side do not leave a row of bare titles.
                let title = render::tool_title(&name, &input);
                if block != Block::Tool || !line_done {
                    open_block(&mut block, Block::Tool, &mut line_done, &mut markdown)?;
                }
                activity(&title);
                running.push((name, title, false));
                streamed = false;
            }
            AgentEvent::Output { line, stderr } => {
                streamed = true;
                let mut out = String::new();
                if let Some((_, title, shown)) = running.last_mut()
                    && !*shown
                {
                    *shown = true;
                    out.push_str(&render::tool_started(&visible(title)));
                    open = Some(title.clone());
                }
                out.push_str(&render::output_line(&visible(&line), stderr));
                console::write(false, &out)?;
            }
            AgentEvent::ToolFinished {
                name,
                failed,
                output,
            } => {
                let id = crate::app::blocks::BLOCKS.add(&name, failed, &output);
                let title = running
                    .iter()
                    .position(|(running, _, _)| *running == name)
                    .map(|at| running.remove(at).1);
                let mut out = String::new();
                if let Some(title) = title
                    && open.as_ref() != Some(&title)
                {
                    out.push_str(&render::tool_started(&visible(&title)));
                }
                // `verbose`: the whole result above its summary (output shown live is not repeated).
                let verbose =
                    crate::app::blocks::VERBOSE.load(std::sync::atomic::Ordering::Relaxed);
                if verbose && !streamed {
                    for line in output.lines() {
                        out.push_str(&render::output_line(&visible(line), false));
                    }
                }
                out.push_str(&render::tool_finished(
                    &name,
                    failed,
                    &visible(&output),
                    id,
                    std::mem::take(&mut streamed),
                ));
                console::write(false, &out)?;
                open = None;
                activity(match running.last() {
                    Some((_, title, _)) => title,
                    None => "思考中",
                });
            }
            AgentEvent::Notice(text) => {
                if !line_done || block != Block::None {
                    open_block(&mut block, Block::None, &mut line_done, &mut markdown)?;
                }
                console::write(false, &format!("\x1b[33m!\x1b[0m {}\n", visible(&text)))?;
                open = None;
            }
            AgentEvent::Usage { tokens, .. } => {
                if let Some(console) = console {
                    console.add_tokens(
                        tokens.input + tokens.cache_read + tokens.cache_write,
                        tokens.output,
                    );
                }
            }
        }
    }
    let mut out = markdown.finish();
    if !line_done || !out.is_empty() && !out.ends_with('\n') {
        out.push('\n');
    }
    console::write(true, &out)
}

/// The row standing for hidden reasoning; the first one tells how to show it.
fn thought(seconds: f64) -> String {
    static HINTED: std::sync::atomic::AtomicBool = std::sync::atomic::AtomicBool::new(false);
    let hint = if HINTED.swap(true, std::sync::atomic::Ordering::Relaxed) {
        ""
    } else {
        "（配置 show_reasoning: true 可显示思考内容）"
    };
    format!(
        "\x1b[2;3m✻ 思考了 {}{hint}\x1b[0m\n",
        crate::app::render::duration(seconds.max(1.0))
    )
}

/// REPL lines: from the console on a terminal, else from plain stdin lines.
pub struct ReplInput {
    lines: Option<tokio::sync::mpsc::Receiver<Result<Option<String>>>>,
    /// Images the console attached to the last line read.
    pub attachments: Vec<std::path::PathBuf>,
}

impl ReplInput {
    pub fn open() -> Self {
        if console::active().is_some() {
            return Self {
                lines: None,
                attachments: Vec::new(),
            };
        }
        let (sender, receiver) = tokio::sync::mpsc::channel(1);
        std::thread::spawn(move || {
            use std::io::{BufRead, Read};
            let stdin = std::io::stdin();
            let mut reader = std::io::BufReader::new(stdin.lock());
            loop {
                let mut line = String::new();
                let read = (&mut reader).take(64 * 1024 + 1).read_line(&mut line);
                let result = match read {
                    Ok(0) => Ok(None),
                    Ok(_) if line.len() <= 64 * 1024 => Ok(Some(line)),
                    Ok(_) => Err(Error::Limit(
                        "输入行超过 64 KiB；请把长文本存成文件，再用 @路径 附加".into(),
                    )),
                    Err(err) => Err(Error::Io(err)),
                };
                let finished = !matches!(result, Ok(Some(_)));
                if sender.blocking_send(result).is_err() || finished {
                    break;
                }
            }
        });
        Self {
            lines: Some(receiver),
            attachments: Vec::new(),
        }
    }

    /// The next line; on the console, `completer` offers completions while typing.
    pub async fn read(&mut self, completer: Completer) -> Result<Option<String>> {
        if let Some(console) = console::active() {
            console.set_completer(completer);
            return Ok(match console.next_line().await {
                console::Input::Line(line, attachments) => {
                    self.attachments = attachments;
                    Some(line)
                }
                console::Input::Closed => None,
            });
        }
        let receiver = self
            .lines
            .as_mut()
            .ok_or_else(|| Error::Session("输入源不可用；请重新启动 cove".into()))?;
        tokio::select! {
            signal = tokio::signal::ctrl_c() => { signal?; Err(Error::Cancelled) },
            line = receiver.recv() => line.unwrap_or(Ok(None)),
        }
    }
}

pub fn print_text(text: &str, diagnostic: bool) -> std::io::Result<()> {
    console::write(!diagnostic, &visible(text))
}

pub fn print_error(text: &str) -> std::io::Result<()> {
    console::write(false, &format!("错误：{}\n", visible(text)))
}

pub fn print_report(report: &TaskReport) -> std::io::Result<()> {
    let cost = report
        .estimated_cost
        .map(|cost| format!("预估费用 ${cost:.6}"))
        .unwrap_or_else(|| "费用未知".into());
    let verification = match report.verification {
        Some(crate::safety::verify::Outcome::Passed) => "验证通过",
        Some(crate::safety::verify::Outcome::Failed) => "验证未通过",
        Some(crate::safety::verify::Outcome::NotRun) => "验证未运行",
        None => "无自动验证",
    };
    if console::rich() {
        let seconds = console::active().map_or(0.0, |console| console.elapsed());
        let verification = if report.verification.is_some() {
            verification
        } else {
            ""
        };
        return console::write(
            false,
            &crate::app::render::report(
                report.tools_failed,
                report.tools_run,
                report.model_calls,
                report.usage.input + report.usage.output,
                report.estimated_cost,
                seconds,
                verification,
            ),
        );
    }
    console::write(
        false,
        &format!(
            "\n[summary] 模型调用 {} 次，工具 {} 个（失败 {}），输入 {} / 输出 {} tokens，{}；{verification}；这是模型自行结束，不代表已独立验收\n",
            report.model_calls,
            report.tools_run,
            report.tools_failed,
            report.usage.input,
            report.usage.output,
            cost
        ),
    )
}
