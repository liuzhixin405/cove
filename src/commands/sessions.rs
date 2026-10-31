//! Session history, export, cost ledger, Git snapshots and memory commands:
//! `/resume …`, `/status cost …`, `/undo …`, `/diff`, `/config memory …`, plus the
//! bare-input shortcuts (`继续`/`continue`, a number after a history list).

use std::{path::PathBuf, sync::Arc, sync::Mutex, time::Duration};

use tokio_util::sync::CancellationToken;

use crate::{
    app::Application,
    app::ui::print_text,
    config::Settings,
    domain::{Block, Error, Message, Provider, Result, Role},
    engine::turn::TurnHook,
    memory::dream::Dreamer,
    memory::extract::{Extractor, is_local},
    memory::recall::{Embedder, EmbeddingConfig, INLINE_LIMIT, MemoryRecall, clip},
    memory::store::{Memory, ProvenanceSource, Scope},
    safety::permission::{Authorizer, Effect, Mode},
    session::notes::SessionNotes,
    session::snapshot::{SnapshotInfo, Snapshots, Snapshotting},
    session::usage::{UsageLedger, aggregate, csv, parse_period, render, timestamp},
    session::{MAX_SESSIONS, Session, SessionPersistence, State, Store, history_lines},
    tools::ToolExecutor,
};

pub(crate) const HELP: &str = "\
/resume [list|all|search <words>|<n|id>|detail <n|id>|delete <n|id>|clear [all] [confirm]|clean|export [file]]  会话历史（列表后直接输入编号即可恢复；新会话中输入 继续 回到上一个任务）   /history  同上
/continue [<n|id>]  列出最近 5 个当前工作区会话供选择，或直接恢复指定会话；未完成任务会继续运行
/status usage  本会话：账户、花费、API 与实际耗时、代码改动、各模型 token   /usage  同上
/status cost [month|YYYY-MM|30d|24h|today|all] [export [file]]  按服务商/模型统计用量；export 导出 CSV
/undo [list|<#n|hash>|files <#n|hash> <file…>|confirm [token]]  文件工具回滚用 /undo confirm；快照可恢复整个工作区（含 Shell 的影响）
/config memory [list|add [global] <text>|remove <id>|search <query>|stats|source <id>|dream [status|run]]  长期记忆：查看、添加、删除、搜索、整理";

/// A previewed snapshot restore waiting for `/undo confirm <token>`; kept on disk so a
/// later one-shot invocation can confirm it.
#[derive(serde::Serialize, serde::Deserialize)]
struct PendingRestore {
    token: String,
    target: SnapshotInfo,
    paths: Vec<String>,
}

/// Area services shared by the commands and the agent hooks.
pub(crate) struct Services {
    pub(crate) ledger: Arc<UsageLedger>,
    pub(crate) snapshots: Option<Arc<Snapshots>>,
    pub(crate) snapshot_error: Option<String>,
    pub(crate) memory: Arc<Memory>,
    pub(crate) recall: Arc<MemoryRecall>,
    pub(crate) notes: Arc<SessionNotes>,
    pub(crate) extractor: Arc<Extractor>,
    pub(crate) dreamer: Arc<Dreamer>,
    max_sessions: i64,
    last_list: Mutex<Option<Vec<String>>>,
}

/// Records the session that model calls are attributed to in the ledger.
struct LedgerSession(Arc<UsageLedger>);

impl TurnHook for LedgerSession {
    fn task_note<'a>(
        &'a self,
        session: &'a Session,
    ) -> futures_util::future::BoxFuture<'a, Option<String>> {
        self.0.set_session(&session.id);
        Box::pin(async { None })
    }
}

impl Services {
    pub(crate) async fn start(
        settings: &Settings,
        store: &Store,
        memory: Arc<Memory>,
        authorizer: Arc<dyn Authorizer>,
        background: Arc<dyn Provider>,
        ledger: Arc<UsageLedger>,
    ) -> Arc<Self> {
        // Snapshotting a home directory or a drive would stage everything on it.
        let (snapshots, snapshot_error) = match crate::safety::protected::too_wide(&settings.cwd) {
            Some(what) => (None, Some(format!("自动快照已关闭：工作区是 {what}"))),
            None => match Snapshots::open(store.root(), &settings.cwd).await {
                Ok(snapshots) => (Some(snapshots), None),
                Err(err) => (None, Some(err.to_string())),
            },
        };
        let embedder = settings
            .file
            .get("memory_embedding")
            .cloned()
            .and_then(|value| serde_json::from_value::<EmbeddingConfig>(value).ok())
            .map(|config| Arc::new(Embedder::new(config, authorizer.clone())));
        let auto_off = if settings.no_auto {
            Some("--no-auto".to_owned())
        } else if is_local(&settings.base_url) {
            Some("local provider".to_owned())
        } else {
            None
        };
        let max_sessions = settings
            .file
            .get("max_sessions")
            .and_then(serde_json::Value::as_i64)
            .unwrap_or(MAX_SESSIONS);
        Arc::new(Self {
            snapshots,
            snapshot_error,
            recall: Arc::new(MemoryRecall::new(memory.clone(), embedder)),
            notes: Arc::new(SessionNotes::new(store.root(), &settings.cwd)),
            extractor: Arc::new(Extractor::new(
                memory.clone(),
                background.clone(),
                auto_off.clone(),
                settings.budget,
            )),
            dreamer: Arc::new(Dreamer::new(
                memory.clone(),
                background,
                store.clone(),
                store.root().to_path_buf(),
                settings.cwd.clone(),
                settings.prices,
                auto_off,
                settings.budget,
            )),
            memory,
            ledger,
            max_sessions,
            last_list: Mutex::new(None),
        })
    }

    /// Snapshots before mutating tools, except in read-only mode (nothing can change).
    pub(crate) fn wrap_tools(
        &self,
        tools: Arc<dyn ToolExecutor>,
        mode: Mode,
    ) -> Arc<dyn ToolExecutor> {
        // Plan mode cannot change anything; every other mode is snapshotted.
        match (&self.snapshots, mode) {
            (Some(snapshots), mode) if mode != Mode::Plan => Arc::new(Snapshotting {
                inner: tools,
                snapshots: snapshots.clone(),
            }),
            _ => tools,
        }
    }

    /// Loop extensions in order: ledger attribution, snapshot task reset, memories,
    /// session notes, extraction, dream.
    pub(crate) fn hooks(&self) -> Vec<Arc<dyn TurnHook>> {
        let mut hooks: Vec<Arc<dyn TurnHook>> = vec![Arc::new(LedgerSession(self.ledger.clone()))];
        if let Some(snapshots) = &self.snapshots {
            hooks.push(snapshots.clone());
        }
        hooks.push(self.recall.clone());
        hooks.push(self.notes.clone());
        hooks.push(self.extractor.clone());
        hooks.push(Arc::new(self.dreamer.clone()));
        hooks
    }

    /// Startup maintenance: ledger attribution and `max_sessions` pruning.
    pub(crate) fn maintain(&self, store: &Store, current: &str) {
        self.ledger.set_session(current);
        if let Ok(Some(report)) = store.prune(self.max_sessions, current)
            && report.sessions > 0
        {
            let _ = print_text(
                &format!(
                    "[history] 已清理 {} 个旧会话（max_sessions={}）\n",
                    report.sessions, self.max_sessions
                ),
                true,
            );
        }
    }

    /// Lets background extraction finish (bounded) and stops a running dream.
    pub(crate) async fn shutdown(&self) {
        self.extractor.wait(Duration::from_secs(10)).await;
        self.dreamer.shutdown(Duration::from_secs(5)).await;
    }

    fn remember_list(&self, ids: Option<Vec<String>>) {
        if let Ok(mut list) = self.last_list.lock() {
            *list = ids;
        }
    }
}

// ------------------------------------------------------------------ usage

/// `/status usage`: who pays, what this session cost and what it changed.
fn usage(app: &Application) -> String {
    let settings = &app.settings;
    let accounting = &app.session.accounting;
    let mut out = String::from("账户\n");
    out.push_str(&row(
        "认证方式",
        &auth_method(&settings.provider, &settings.key_source),
    ));
    out.push_str(&row(
        "模型",
        &format!("{} ({})", settings.model, settings.base_url),
    ));
    out.push_str(&format!("\n用量（会话 {}）\n", &app.session.id[..8]));
    let cost = accounting.cost.map_or(
        "未知（该模型没有价格信息）".to_owned(),
        |cost| format!("${cost:.2}"),
    );
    out.push_str(&row("总花费", &cost));
    out.push_str(&row(
        "API 耗时",
        &duration(std::time::Duration::from_millis(accounting.api_ms)),
    ));
    let wall = crate::session::now().saturating_sub(app.session.created);
    out.push_str(&row(
        "实际耗时",
        &format!(
            "自会话开始 {}",
            duration(std::time::Duration::from_secs(wall))
        ),
    ));
    out.push_str(&row(
        "代码改动",
        &format!(
            "新增 {} 行，删除 {} 行（文件工具）",
            accounting.lines_added, accounting.lines_removed
        ),
    ));
    out.push_str(&row("模型调用", &accounting.model_calls.to_string()));
    out.push_str("\n各模型用量\n");
    if accounting.by_model.is_empty() {
        out.push_str("  暂无\n");
    }
    for (model, spend) in &accounting.by_model {
        let cost = spend
            .cost
            .map_or("花费未知".to_owned(), |cost| format!("${cost:.2}"));
        out.push_str(&format!(
            "  {model}\n    输入 {}，输出 {}，缓存读取 {}，缓存写入 {}，{} 次调用（{cost}）\n",
            tokens(spend.usage.input),
            tokens(spend.usage.output),
            tokens(spend.usage.cache_read),
            tokens(spend.usage.cache_write),
            spend.calls
        ));
    }
    out
}

fn row(label: &str, value: &str) -> String {
    // CJK characters take two columns.
    let width: usize = label
        .chars()
        .map(|c| if c.is_ascii() { 1 } else { 2 })
        .sum();
    format!(
        "  {label}{}{value}\n",
        " ".repeat(16usize.saturating_sub(width))
    )
}

/// The provider as a person would name it, and where its key came from.
fn auth_method(provider: &str, key_source: &str) -> String {
    let name = match provider {
        "bedrock" => "Amazon Bedrock",
        "anthropic" | "claude" => "Anthropic API",
        "openai" => "OpenAI API",
        "deepseek" => "DeepSeek",
        "openrouter" => "OpenRouter",
        "openai-compatible" => "OpenAI 兼容端点",
        other => other,
    };
    match key_source {
        "config" => format!("{name}（密钥来自配置文件）"),
        "" => name.to_owned(),
        variable => format!("{name}（密钥来自 {variable}）"),
    }
}

/// `3h 23m 47s`, `5m 2s`, `12s`.
fn duration(duration: std::time::Duration) -> String {
    let seconds = duration.as_secs();
    let (hours, minutes, seconds) = (seconds / 3600, seconds / 60 % 60, seconds % 60);
    match (hours, minutes) {
        (0, 0) => format!("{seconds}s"),
        (0, _) => format!("{minutes}m {seconds}s"),
        _ => format!("{hours}h {minutes}m {seconds}s"),
    }
}

/// `950`, `3.8k`, `319.5M`.
fn tokens(count: u64) -> String {
    match count {
        0..1_000 => count.to_string(),
        1_000..1_000_000 => format!("{:.1}k", count as f64 / 1e3),
        _ => format!("{:.1}M", count as f64 / 1e6),
    }
}

fn say(text: &str) -> Result<()> {
    print_text(text, false)?;
    Ok(())
}

/// Bare input shortcuts; `None` when `line` is an ordinary prompt.
pub(crate) async fn intercept(app: &mut Application, line: &str) -> Option<Result<bool>> {
    let listed = app
        .sessions
        .last_list
        .lock()
        .ok()
        .and_then(|mut list| list.take());
    let trimmed = line.trim();
    if let (Some(ids), Ok(number)) = (&listed, trimmed.parse::<usize>()) {
        let Some(id) = number.checked_sub(1).and_then(|index| ids.get(index)) else {
            return Some(Err(Error::Session(format!(
                "上次列表中没有第 {number} 项；输入 /resume 重新列出会话"
            ))));
        };
        let id = id.clone();
        return Some(resume(app, &id, true).await);
    }
    let continuing = CONTINUE_WORDS.contains(&trimmed.to_lowercase().as_str());
    if continuing && app.session.unfinished() {
        return Some(app.execute(None).await.map(|_| false));
    }
    if app.session.has_conversation() {
        return None;
    }
    // A fresh session: "继续" goes back to this workspace's most relevant task, and a
    // request retyped from an unfinished task continues that task.
    let target = if continuing {
        most_relevant(app)
    } else {
        project_sessions(app, false)
            .into_iter()
            .enumerate()
            .find(|(_, session)| {
                session.id != app.session.id
                    && session.unfinished()
                    && session
                        .first_request()
                        .is_some_and(|first| same_request(first, trimmed))
            })
            .map(|(index, session)| (index + 1, session))
    };
    let (number, session) = target?;
    let note = if continuing {
        format!("[resume] 回到任务 #{number}：{}\n", session.display_title())
    } else {
        format!("[resume] 这个请求就是未完成的任务 #{number}；接着做，不从头开始\n")
    };
    Some(
        async {
            say(&note)?;
            if resume(app, &session.id, true).await? {
                return Ok(true);
            }
            if app.session.unfinished() {
                app.execute(None).await?;
            } else if continuing {
                app.execute(Some(line.to_owned())).await?;
            }
            Ok(false)
        }
        .await,
    )
}

pub(crate) async fn handle(
    app: &mut Application,
    line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    app.sessions.ledger.set_session(&app.session.id);
    let first = args.first().copied().unwrap_or_default();
    if first != "/resume" && first != "/continue" {
        app.sessions.remember_list(None);
    }
    match args {
        ["/continue"] => continue_sessions(app, None).await?,
        ["/continue", reference] => continue_sessions(app, Some(*reference)).await?,
        ["/resume" | "/history", rest @ ..] => {
            if history(app, line, rest).await? {
                return Ok(Some(true));
            }
        }
        ["/status", "cost", rest @ ..] => cost(app, rest).await?,
        ["/status", "usage"] | ["/usage"] => say(&usage(app))?,
        ["/undo", "list"] => undo_list(app).await?,
        ["/undo", "confirm", token] => undo_apply(app, token).await?,
        ["/undo", "files", target, files @ ..] if !files.is_empty() => {
            undo_preview(app, target, files.iter().map(|f| f.to_string()).collect()).await?
        }
        ["/undo", target] if *target != "confirm" => undo_preview(app, target, Vec::new()).await?,
        ["/undo"] => undo_overview(app).await?,
        ["/diff"] => {
            let Some(text) = task_diff(app).await? else {
                return Ok(None);
            };
            say(&text)?;
        }
        ["/config", "memory", sub, ..] if !sub.starts_with('{') => {
            memory(app, line, &args[2..]).await?
        }
        ["/config", "memory"] => memory(app, line, &["list"]).await?,
        _ => return Ok(None),
    }
    Ok(Some(false))
}

// ---------------------------------------------------------------- history

/// On the console, the listed sessions also open as a picker.
fn offer_picker(app: &Application, sessions: &[Session], show_cwd: bool) {
    let Some(console) = crate::app::console::active() else {
        return;
    };
    let lines = crate::session::history_lines(sessions, Some(&app.session.id), show_cwd);
    let choices = sessions
        .iter()
        .zip(lines.lines())
        .filter(|(session, _)| session.id != app.session.id)
        .map(|(session, line)| crate::app::console::Choice {
            value: format!("/resume {}", session.id),
            label: line.trim().to_owned(),
            description: String::new(),
        })
        .collect();
    console.choose("恢复会话", choices);
}

/// Sessions of this workspace (or all), newest first; sessions in which nothing was
/// ever asked are left out (a fresh current session too: it has nothing to show).
fn project_sessions(app: &Application, all: bool) -> Vec<Session> {
    let (sessions, _) = app.store.list_lenient();
    sessions
        .into_iter()
        .filter(|session| all || session.cwd == app.settings.cwd)
        .filter(Session::has_conversation)
        .collect()
}

/// The session of this workspace most worth going back to (unfinished first, then
/// real work), with its number in `/resume`'s list.
fn most_relevant(app: &Application) -> Option<(usize, Session)> {
    let mut best: Option<(i32, usize, Session)> = None;
    for (index, session) in project_sessions(app, false)
        .into_iter()
        .enumerate()
        .take(30)
    {
        let score = session.resume_score();
        if session.id == app.session.id || score <= 0 {
            continue;
        }
        if best.as_ref().is_none_or(|(top, ..)| score > *top) {
            best = Some((score, index + 1, session));
        }
    }
    best.map(|(_, number, session)| (number, session))
}

fn turns(count: usize) -> String {
    format!("{count} 轮")
}

fn ago(secs: u64) -> String {
    let age = crate::session::now().saturating_sub(secs);
    match age {
        0..60 => "刚刚".into(),
        60..3600 => format!("{} 分钟前", age / 60),
        3600..86400 => format!("{} 小时前", age / 3600),
        _ => format!("{} 天前", age / 86400),
    }
}

/// The line shown at startup when this workspace has work to go back to.
pub(crate) fn startup_notice(app: &Application) -> Option<String> {
    if app.session.has_conversation() {
        return None;
    }
    let (number, session) = most_relevant(app)?;
    let state = if session.unfinished() {
        "未完成"
    } else {
        "已完成"
    };
    Some(format!(
        "上次在这里的任务：#{number} {}（{}，{state}，{}）。输入 /continue 选择会话，/resume 查看全部。\n",
        session.display_title(),
        turns(session.user_turns()),
        ago(session.updated)
    ))
}

/// Lists a short, workspace-local history for `/continue`, then resumes the
/// selected session. Completed sessions restore context without an automatic turn.
async fn continue_sessions(app: &mut Application, reference: Option<&str>) -> Result<()> {
    if let Some(reference) = reference {
        let id = pick(app, reference)?;
        if id != app.session.id {
            resume(app, &id, false).await?;
        }
        if app.session.unfinished() {
            app.execute(None).await?;
        } else {
            say("已恢复会话上下文；可以直接继续对话。\n")?;
        }
        return Ok(());
    }

    let sessions: Vec<_> = project_sessions(app, false).into_iter().take(5).collect();
    if sessions.is_empty() {
        say("没有可继续的历史会话。\n")?;
        return Ok(());
    }
    let lines = history_lines(&sessions, Some(&app.session.id), false);
    say(&format!(
        "最近会话（输入 /continue <编号> 选择）：\n{lines}\n"
    ))?;
    app.sessions.remember_list(Some(
        sessions.iter().map(|session| session.id.clone()).collect(),
    ));
    if let Some(console) = crate::app::console::active() {
        let choices = sessions
            .iter()
            .zip(lines.lines())
            .map(|(session, line)| crate::app::console::Choice {
                value: format!("/continue {}", session.id),
                label: line.trim().to_owned(),
                description: if session.unfinished() {
                    "恢复并继续未完成任务".to_owned()
                } else {
                    "恢复会话上下文".to_owned()
                },
            })
            .collect();
        console.choose("继续哪个会话？", choices);
    }
    Ok(())
}

const CONTINUE_WORDS: [&str; 6] = ["继续", "继续吧", "接着做", "continue", "go on", "resume"];

/// Same request text, ignoring case and spacing.
fn same_request(a: &str, b: &str) -> bool {
    let normal = |text: &str| {
        text.split_whitespace()
            .collect::<Vec<_>>()
            .join(" ")
            .to_lowercase()
    };
    normal(a) == normal(b)
}

/// Resolves `#`-less numbers against the last list (else this project's list) and IDs
/// or unique ID prefixes against every session.
fn pick(app: &Application, reference: &str) -> Result<String> {
    if let Ok(number) = reference.parse::<usize>()
        && reference.len() < 6
    {
        let ids: Vec<String> = app
            .sessions
            .last_list
            .lock()
            .ok()
            .and_then(|list| list.clone())
            .unwrap_or_else(|| {
                project_sessions(app, false)
                    .into_iter()
                    .map(|session| session.id)
                    .collect()
            });
        return number
            .checked_sub(1)
            .and_then(|index| ids.get(index).cloned())
            .ok_or_else(|| {
                Error::Session(if ids.is_empty() {
                    "这里还没有可恢复的会话；/resume all 查看其他工作区的会话".to_owned()
                } else {
                    format!(
                        "没有编号为 {number} 的会话（列表共 {} 个，编号 1..{}）；输入 /resume 重新查看",
                        ids.len(),
                        ids.len()
                    )
                })
            });
    }
    if reference.len() < 4 || !reference.bytes().all(|b| b.is_ascii_hexdigit()) {
        return Err(Error::Session(
            "请使用 /resume 列表中的编号，或会话 ID（至少 4 位十六进制前缀）".into(),
        ));
    }
    let matches: Vec<String> = app
        .store
        .list_lenient()
        .0
        .into_iter()
        .map(|session| session.id)
        .filter(|id| id.starts_with(&reference.to_ascii_lowercase()))
        .collect();
    match matches.as_slice() {
        [one] => Ok(one.clone()),
        [] => Err(Error::Session(
            "没有这个 ID 的会话；输入 /resume all 查看所有会话".into(),
        )),
        many => Err(Error::Session(format!(
            "会话 ID 前缀 {reference} 匹配到多个会话（{}）；请多输入几位",
            many.iter()
                .map(|id| &id[..id.len().min(8)])
                .collect::<Vec<_>>()
                .join("、")
        ))),
    }
}

/// Switches to session `id`; `Ok(true)` when the program restarts to do so.
async fn resume(app: &mut Application, id: &str, show_continue_hint: bool) -> Result<bool> {
    if id == app.session.id {
        say("已经在使用这个会话。\n")?;
        return Ok(false);
    }
    let lease = app.store.acquire(id)?;
    let session = app.store.load(id)?;
    if session.cwd != app.settings.cwd {
        drop(lease);
        return resume_elsewhere(app, &session);
    }
    crate::app::validate_identity(&session, &app.settings)?;
    app.close_session().await?;
    app.store.save(&app.session)?;
    app.workspace
        .activate(session.current_checkpoint.as_deref())?;
    app.session = session;
    app.lease = lease;
    app.sessions.ledger.set_session(&app.session.id);
    say(&format!(
        "[session] {} {}\n",
        app.session.id,
        app.session.display_title()
    ))?;
    show_conversation(&app.session)?;
    if show_continue_hint && matches!(app.session.state, State::Interrupted(_)) {
        say("这个会话有被中断的任务：输入 /continue 恢复，或直接发送新消息。\n")?;
    }
    Ok(false)
}

/// A session of another workspace resumes in a restart there, the way `/cd` switches:
/// the tools, checkpoints and permissions of a workspace belong to its process.
fn resume_elsewhere(app: &mut Application, session: &Session) -> Result<bool> {
    let cwd = session.cwd.display().to_string();
    let command = format!("cove --cwd \"{cwd}\" --resume {}", session.id);
    if app.settings.one_shot {
        return Err(Error::Session(format!(
            "该会话属于另一个工作区 {cwd}；单次运行不能切换目录，请运行：{command}"
        )));
    }
    if !session.cwd.is_dir() {
        return Err(Error::Session(format!(
            "该会话的工作区 {cwd} 已不存在，无法在那里恢复；/resume detail {} 可查看它的内容",
            &session.id[..session.id.len().min(8)]
        )));
    }
    if app.store.root().starts_with(&session.cwd) {
        return Err(Error::Session(format!(
            "该会话的工作区 {cwd} 包含数据目录，不能在这里切换过去；请退出后运行：{command}"
        )));
    }
    app.store.save(&app.session)?;
    let original: Vec<_> = std::env::args_os().skip(1).collect();
    crate::app::launch::request_restart(crate::app::launch::restart_args(
        &original,
        true,
        &[
            "--cwd".into(),
            session.cwd.clone().into_os_string(),
            "--resume".into(),
            session.id.clone().into(),
        ],
    ));
    print_text(
        &format!(
            "[resume] 注意：会话 {} 属于另一个工作区 {cwd}；正在切换到该目录重启并恢复它。当前会话 {} 已保存，回来用 /cd {}。
",
            &session.id[..session.id.len().min(8)],
            &app.session.id[..app.session.id.len().min(8)],
            app.settings.cwd.display()
        ),
        true,
    )?;
    Ok(true)
}

/// The last turns of a resumed session, so the conversation is on screen again.
pub(crate) fn show_conversation(session: &Session) -> Result<()> {
    const TURNS: usize = 3;
    let text = session.transcript(TURNS);
    if text.trim().is_empty() {
        return Ok(());
    }
    let turns = session.user_turns();
    let shown = if turns > TURNS {
        format!("共 {turns} 轮，显示最后 {TURNS} 轮")
    } else {
        self::turns(turns)
    };
    print_text(
        &format!(
            "── 之前的对话（{shown}；/resume detail {} 查看更多）──{text}── 之前的对话到此结束；模型保有完整上下文 ──\n",
            &session.id[..session.id.len().min(8)]
        ),
        true,
    )?;
    Ok(())
}

/// `Ok(true)` when resuming restarts the program in another workspace.
async fn history(app: &mut Application, line: &str, rest: &[&str]) -> Result<bool> {
    match rest {
        [] | ["list"] | ["all"] => {
            let all = rest == ["all"];
            let sessions = project_sessions(app, all);
            let hidden = if all {
                0
            } else {
                app.store.list_lenient().0.len() - sessions.len()
            };
            let mut text = history_lines(&sessions, Some(&app.session.id), all);
            let empty = app
                .store
                .list_lenient()
                .0
                .iter()
                .filter(|s| (all || s.cwd == app.settings.cwd) && !s.has_conversation())
                .count();
            let hidden = hidden.saturating_sub(empty);
            if empty > 0 {
                text.push_str(&format!("{empty} 个空会话未列出。\n"));
            }
            if hidden > 0 {
                text.push_str(&format!(
                    "另有 {hidden} 个其他工作区的会话未显示；/resume all 可列出。\n"
                ));
            }
            if sessions.is_empty() {
                text.push_str("这里还没有会话记录；直接输入请求即可开始。\n");
            } else {
                text.push_str("输入编号即可恢复，或用 /resume detail <n> 查看详情。\n");
            }
            say(&text)?;
            offer_picker(app, &sessions, all);
            app.sessions
                .remember_list(Some(sessions.into_iter().map(|s| s.id).collect()));
        }
        ["search", words @ ..] if !words.is_empty() => {
            let query = words.join(" ");
            let sessions = project_sessions(app, true);
            let documents: Vec<(String, u64)> = sessions
                .iter()
                .map(|session| (search_text(session), session.updated))
                .collect();
            let found: Vec<Session> =
                crate::memory::recall::rank(&documents, &query, crate::session::now())
                    .into_iter()
                    .take(20)
                    .map(|(index, _)| sessions[index].clone())
                    .collect();
            let mut text = history_lines(&found, Some(&app.session.id), true);
            if found.is_empty() {
                text.push_str("没有找到匹配的会话；换个关键词再试，或用 /resume all 列出全部。\n");
            } else {
                text.push_str("输入编号即可恢复。\n");
            }
            say(&text)?;
            offer_picker(app, &found, true);
            app.sessions
                .remember_list(Some(found.into_iter().map(|s| s.id).collect()));
        }
        ["detail", reference] => {
            let id = pick(app, reference)?;
            let session = if id == app.session.id {
                app.session.clone()
            } else {
                app.store.load(&id)?
            };
            say(&detail(&session))?;
        }
        ["delete", reference] => {
            let id = pick(app, reference)?;
            if id == app.session.id {
                return Err(Error::Session(
                    "不能删除当前会话；请先 /new 开新会话，再删除它".into(),
                ));
            }
            let lease = app.store.acquire(&id)?;
            let session = app.store.load(&id)?;
            authorize_store(app).await?;
            app.store.remove(&session)?;
            drop(lease);
            say(&format!(
                "已删除会话 {}（{}）。\n",
                &id[..8],
                session.display_title()
            ))?;
        }
        ["clear", flags @ ..] => {
            let all = flags.contains(&"all");
            let confirm = flags.contains(&"confirm");
            if flags.iter().any(|flag| !matches!(*flag, "all" | "confirm")) {
                return Err(Error::Session("用法：/resume clear [all] [confirm]".into()));
            }
            // Empty sessions are not listed, but clearing removes them too.
            let targets: Vec<Session> = app
                .store
                .list_lenient()
                .0
                .into_iter()
                .filter(|session| all || session.cwd == app.settings.cwd)
                .filter(|session| session.id != app.session.id)
                .collect();
            if !confirm {
                say(&format!(
                    "将删除 {} 个会话（{}），保留当前会话。确认请运行 /resume clear{} confirm。\n",
                    targets.len(),
                    if all {
                        "所有工作区"
                    } else {
                        "本工作区"
                    },
                    if all { " all" } else { "" }
                ))?;
                return Ok(false);
            }
            authorize_store(app).await?;
            let (mut removed, mut busy) = (0, 0);
            for session in targets {
                match app.store.acquire(&session.id) {
                    Ok(_lease) => {
                        app.store.remove(&session)?;
                        removed += 1;
                    }
                    Err(_) => busy += 1,
                }
            }
            say(&format!(
                "已删除 {removed} 个会话；{busy} 个正被其他进程使用，已保留。\n"
            ))?;
        }
        ["clean"] => {
            authorize_store(app).await?;
            let report = app.store.clean(&app.session.id)?;
            let mut text = format!(
                "历史已整理：扫描 {} 个，修复标题 {} 个，移走不可读的 {} 个，跳过使用中的 {} 个。\n",
                report.scanned,
                report.titles_fixed,
                report.quarantined.len(),
                report.in_use
            );
            for item in &report.quarantined {
                text.push_str(&format!("  已移走：{}\n", clip(item, 300)));
            }
            if let Some(backup) = report.backup {
                text.push_str(&format!("备份：{}\n", backup.display()));
            }
            say(&text)?;
        }
        ["export", ..] => {
            let file = line
                .trim()
                .strip_prefix("/resume")
                .and_then(|rest| rest.trim_start().strip_prefix("export"))
                .map(|rest| rest.trim().trim_matches('"'))
                .filter(|rest| !rest.is_empty());
            let path = match file {
                Some(file) => {
                    let path = PathBuf::from(file);
                    if path.is_absolute() {
                        path
                    } else {
                        app.settings.cwd.join(path)
                    }
                }
                None => app
                    .store
                    .root()
                    .join("exports")
                    .join(format!("{}.md", app.session.id)),
            };
            app.authorizer
                .authorize(Effect::Write(path.clone()), CancellationToken::new())
                .await?;
            if let Some(parent) = path.parent() {
                std::fs::create_dir_all(parent)?;
            }
            std::fs::write(&path, markdown(&app.session))?;
            say(&format!("对话记录已导出到 {}\n", path.display()))?;
        }
        [reference] => {
            let id = pick(app, reference)?;
            return resume(app, &id, true).await;
        }
        _ => {
            return Err(Error::Session(
                "用法：/resume [list|all|search <words>|<n|id>|detail <n|id>|delete <n|id>|clear [all] [confirm]|clean|export [file]]".into(),
            ));
        }
    }
    Ok(false)
}

/// Title and user requests (bounded) of a session, for `/resume search`.
fn search_text(session: &Session) -> String {
    let mut text = session.display_title();
    for message in &session.messages {
        if message.role == Role::User
            && let Some(Block::Text(request)) = message.blocks.first()
        {
            text.push('\n');
            text.push_str(&clip(request, 1024));
            if text.len() > 8 * 1024 {
                break;
            }
        }
    }
    text
}

async fn authorize_store(app: &Application) -> Result<()> {
    app.authorizer
        .authorize(
            Effect::Write(app.store.root().join("sessions")),
            CancellationToken::new(),
        )
        .await
}

fn detail(session: &Session) -> String {
    let mut out = format!(
        "{}\nid={} state={:?}\nworkspace={}\nmodel={} ({})\ncreated={} updated={}\nmessages={} user_turns={} compactions={} model_calls={} input_tokens={} output_tokens={} estimated_cost={}\n",
        session.display_title(),
        session.id,
        session.state,
        session.cwd.display(),
        session.model,
        session.protocol,
        timestamp(session.created),
        timestamp(session.updated),
        session.messages.len(),
        session.user_turns(),
        session.compactions,
        session.accounting.model_calls,
        session.accounting.usage.input,
        session.accounting.usage.output,
        session
            .accounting
            .cost
            .map_or("unknown".into(), |cost| format!("${cost:.4}"))
    );
    let requests: Vec<String> = session
        .messages
        .iter()
        .filter(|message| message.role == Role::User)
        .filter_map(|message| match message.blocks.first() {
            Some(Block::Text(text)) => crate::session::title_from(text),
            _ => None,
        })
        .collect();
    if !requests.is_empty() {
        out.push_str("最近的请求：\n");
        for request in requests.iter().rev().take(5).rev() {
            out.push_str(&format!("  - {request}\n"));
        }
    }
    out
}

/// Markdown transcript including tool calls and (bounded) results; no secrets are
/// stored in sessions, so none can be exported.
pub fn markdown(session: &Session) -> String {
    let mut out = format!(
        "# {}\n\n- 会话：`{}`\n- 工作区：`{}`\n- 模型：{}（{}）\n- 创建：{} UTC，更新：{} UTC\n- 模型调用：{} 次，token 输入/输出：{}/{}，预估花费：{}\n\n---\n\n",
        session.display_title(),
        session.id,
        session.cwd.display(),
        session.model,
        session.protocol,
        timestamp(session.created),
        timestamp(session.updated),
        session.accounting.model_calls,
        session.accounting.usage.input,
        session.accounting.usage.output,
        session
            .accounting
            .cost
            .map_or("unknown".into(), |cost| format!("${cost:.4}"))
    );
    let fence = |text: &str, lang: &str| {
        let ticks = if text.contains("```") { "````" } else { "```" };
        format!("{ticks}{lang}\n{}\n{ticks}\n\n", text.trim_end())
    };
    for message in &session.messages {
        out.push_str(match message.role {
            Role::User => "## 用户\n\n",
            Role::Assistant => "## 助手\n\n",
        });
        for block in &message.blocks {
            match block {
                Block::Text(text) => {
                    out.push_str(text.trim());
                    out.push_str("\n\n");
                }
                Block::ToolUse { name, input, .. } => {
                    out.push_str(&format!("**工具调用 `{name}`**\n\n"));
                    out.push_str(&fence(
                        &serde_json::to_string_pretty(input).unwrap_or_default(),
                        "json",
                    ));
                }
                Block::ToolResult {
                    content, is_error, ..
                } => {
                    out.push_str(if *is_error {
                        "**工具结果（失败）**\n\n"
                    } else {
                        "**工具结果**\n\n"
                    });
                    out.push_str(&fence(&clip(content, 8 * 1024), "text"));
                }
                Block::Image { media_type, .. } => {
                    out.push_str(&format!("*[图片附件：{media_type}]*\n\n"));
                }
                Block::Reasoning(_) | Block::Thinking(_) => {}
            }
        }
    }
    for call in &session.pending {
        out.push_str(&format!(
            "> 待处理的工具调用 `{}`（{:?}）——不属于已完成的历史。\n\n",
            call.name, call.state
        ));
    }
    out
}

// ---------------------------------------------------------------- cost

async fn cost(app: &Application, rest: &[&str]) -> Result<()> {
    let (period_arg, export) = match rest {
        [] => (None, None),
        ["export", file @ ..] => (None, Some(file.first().copied())),
        [period] => (Some(*period), None),
        [period, "export", file @ ..] => (Some(*period), Some(file.first().copied())),
        _ => {
            return Err(Error::Tool(
                "用法：/status cost [month|YYYY-MM|30d|24h|today|all] [export [file]]".into(),
            ));
        }
    };
    let period = parse_period(period_arg, crate::session::now())?;
    let entries = app.sessions.ledger.load(period.from, period.to)?;
    let (rows, total) = aggregate(&entries);
    let mut text = render(&period, &rows, &total);
    if let Some(file) = export {
        let path = match file {
            Some(file) if PathBuf::from(file).is_absolute() => PathBuf::from(file),
            Some(file) => app.settings.cwd.join(file),
            None => app.sessions.ledger.dir().join(format!(
                "export-{}.csv",
                period.label.replace([' ', '(', ')'], "")
            )),
        };
        app.authorizer
            .authorize(Effect::Write(path.clone()), CancellationToken::new())
            .await?;
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent)?;
        }
        std::fs::write(&path, csv(&entries))?;
        text.push_str(&format!(
            "已导出 {} 次调用到 {}\n",
            entries.len(),
            path.display()
        ));
    }
    text.push_str(&format!(
        "账本：{}（每次调用一行 JSON，保留 13 个月）\n",
        app.sessions.ledger.dir().display()
    ));
    say(&text)
}

// ---------------------------------------------------------------- undo / diff

fn snapshots(app: &Application) -> Result<Arc<Snapshots>> {
    app.sessions.snapshots.clone().ok_or_else(|| {
        Error::Workspace(format!(
            "Git 快照不可用（{}）；只能用 /undo 预览、/undo confirm 回滚文件工具的编辑",
            app.sessions.snapshot_error.as_deref().unwrap_or("未启动")
        ))
    })
}

async fn undo_overview(app: &Application) -> Result<()> {
    let file_tools = app.workspace.undo_preview();
    let list = match &app.sessions.snapshots {
        Some(snapshots) => snapshots.list(5).await.unwrap_or_default(),
        None => Vec::new(),
    };
    if list.is_empty()
        && let Err(err) = &file_tools
    {
        return Err(Error::Workspace(format!("{err}；也没有可恢复的 Git 快照")));
    }
    let mut text =
        String::from("文件工具回滚（/undo confirm；只覆盖 write/edit，不含 Shell 的影响）：\n");
    match file_tools {
        Ok(preview) => text.push_str(&preview),
        Err(err) => text.push_str(&format!("  不可用：{err}\n")),
    }
    if !list.is_empty() {
        text.push_str(
            "\nGit 快照（整个工作区，包含 Shell 的影响；不含 .gitignore 忽略的文件和构建目录）：\n",
        );
        for (index, info) in list.iter().enumerate() {
            text.push_str(&format!(
                "  #{} {} {}  {}\n",
                index + 1,
                info.short(),
                timestamp(info.time),
                info.label
            ));
        }
        text.push_str("/undo #1 预览恢复到最新快照；/undo list 查看全部。\n");
    }
    say(&text)
}

async fn undo_list(app: &Application) -> Result<()> {
    let snapshots = snapshots(app)?;
    let list = snapshots.list(crate::session::snapshot::KEEP).await?;
    let mut text = format!(
        "本工作区的 Git 快照（最新在前，保留 {} 个；恢复包含 Shell 的影响）：\n",
        crate::session::snapshot::KEEP
    );
    for (index, info) in list.iter().enumerate() {
        text.push_str(&format!(
            "  #{:<2} {} {}  {}\n",
            index + 1,
            info.short(),
            timestamp(info.time),
            info.label
        ));
    }
    if list.is_empty() {
        text.push_str("  还没有快照（每次会修改文件的工具调用前自动创建）\n");
    }
    text.push_str(
        "/undo <#n|hash> 预览整个工作区的恢复；/undo files <#n|hash> <file…> 只恢复指定文件。\n",
    );
    say(&text)
}

async fn undo_preview(app: &Application, target: &str, paths: Vec<String>) -> Result<()> {
    let snapshots = snapshots(app)?;
    let target = snapshots.resolve(target).await?;
    let paths: Vec<String> = paths
        .into_iter()
        .map(|path| path.replace('\\', "/").trim_start_matches("./").to_owned())
        .collect();
    let (planned, token) = snapshots.plan(&target.hash, &paths).await?;
    let mut text = format!(
        "从快照 {} 恢复{}（{}，{}）：\n",
        target.short(),
        if paths.is_empty() {
            "整个工作区（文件和 Shell 的影响）"
        } else {
            "所选文件"
        },
        timestamp(target.time),
        target.label
    );
    for item in planned.iter().take(200) {
        text.push_str(&format!("  {:<8} {}\n", item.action, item.path));
    }
    if planned.len() > 200 {
        text.push_str(&format!("  ……另有 {} 项\n", planned.len() - 200));
    }
    if planned.is_empty() {
        text.push_str("  没有差异，无需恢复。\n");
        say(&text)?;
        return Ok(());
    }
    text.push_str(&format!(
        "恢复前会先为当前状态创建快照，所以这次恢复本身也能撤销。\n运行 /undo confirm {token} 执行（在此之前工作区若有变化会被拒绝）。\n"
    ));
    crate::session::atomic_json(
        &pending_path(app),
        &PendingRestore {
            token,
            target,
            paths,
        },
    )?;
    say(&text)
}

fn pending_path(app: &Application) -> PathBuf {
    app.store
        .root()
        .join("checkpoints")
        .join(format!("undo-pending-{}.json", app.session.id))
}

async fn undo_apply(app: &mut Application, token: &str) -> Result<()> {
    let snapshots = snapshots(app)?;
    let path = pending_path(app);
    let pending = crate::session::read_json::<PendingRestore>(&path)
        .ok()
        .filter(|pending| pending.token == token)
        .ok_or_else(|| {
            Error::Workspace(
                "确认码未知或已过期；请用 /undo <#n|hash> 重新预览（/undo list 查看快照），再按提示确认".into(),
            )
        })?;
    if !app.session.pending.is_empty() {
        return Err(Error::Session(
            "有待处理的工具调用；请先 /continue 完成，或用 /resolve <call-id> executed|not-run 确认，再恢复".into(),
        ));
    }
    let (_, current) = snapshots.plan(&pending.target.hash, &pending.paths).await?;
    if current != pending.token {
        return Err(Error::Workspace(format!(
            "预览之后工作区发生了变化；请重新预览：{}",
            if pending.paths.is_empty() {
                format!("/undo {}", pending.target.short())
            } else {
                format!(
                    "/undo files {} {}",
                    pending.target.short(),
                    pending.paths.join(" ")
                )
            }
        )));
    }
    app.authorizer
        .authorize(
            Effect::Write(app.settings.cwd.clone()),
            CancellationToken::new(),
        )
        .await?;
    app.session.messages.push(Message::user(format!(
        "Operator requested a Git snapshot restore to {}. Inspect files before further editing.",
        pending.target.short()
    )));
    app.store.save(&app.session)?;
    let _ = std::fs::remove_file(&path);
    let backup = snapshots.restore(&pending.target, &pending.paths).await?;
    let scope = if pending.paths.is_empty() {
        "the whole work tree (file and Shell effects)".to_owned()
    } else {
        format!("{} files", pending.paths.len())
    };
    app.session.messages.push(Message::user(format!(
        "Workspace restored: {scope} from snapshot {}; prior file and verification evidence is stale. The pre-restore state is snapshot {}.",
        pending.target.short(),
        &backup[..10]
    )));
    app.session.state = State::Completed;
    app.store.save(&app.session)?;
    say(&format!(
        "已从快照 {} 恢复{}。之前的验证结果已失效。\n撤销这次恢复：/undo {}（恢复前的快照）。\n",
        pending.target.short(),
        if pending.paths.is_empty() {
            "整个工作区（文件和 Shell 的影响）".to_owned()
        } else {
            format!(" {} 个文件", pending.paths.len())
        },
        &backup[..10]
    ))
}

async fn task_diff(app: &Application) -> Result<Option<String>> {
    let Some(snapshots) = &app.sessions.snapshots else {
        return Ok(None);
    };
    let Some(first) = snapshots.task_first() else {
        return Ok(None);
    };
    let diff = snapshots.diff(&first).await?;
    Ok(Some(format!(
        "自本任务第一个快照 {} 以来的改动（整个工作区，包含 Shell 的影响）：\n{}",
        &first[..10],
        if diff.trim().is_empty() {
            "没有改动\n".into()
        } else {
            diff
        }
    )))
}

// ---------------------------------------------------------------- memory

fn scope_of(word: &str) -> Option<Scope> {
    match word {
        "project" => Some(Scope::Project),
        "global" => Some(Scope::Global),
        _ => None,
    }
}

async fn blocking<T: Send + 'static>(
    job: impl FnOnce() -> Result<T> + Send + 'static,
) -> Result<T> {
    tokio::task::spawn_blocking(job)
        .await
        .map_err(|err| Error::Tool(format!("记忆操作失败：{err}")))?
}

async fn memory(app: &mut Application, line: &str, args: &[&str]) -> Result<()> {
    let memory = app.sessions.memory.clone();
    let text_after = |word: &str| -> String {
        line.split_once(word)
            .map(|(_, rest)| rest.trim().to_owned())
            .unwrap_or_default()
    };
    match args {
        ["list", rest @ ..] => {
            let scopes: Vec<Scope> = match rest.first().and_then(|word| scope_of(word)) {
                Some(scope) => vec![scope],
                None => vec![Scope::Project, Scope::Global],
            };
            let entries = blocking(move || {
                let mut all = Vec::new();
                for scope in scopes {
                    all.extend(memory.entries(scope)?.into_iter().map(|e| (scope, e)));
                }
                Ok(all)
            })
            .await?;
            let mut text = String::new();
            for (scope, entry) in &entries {
                text.push_str(&format!(
                    "{} [{}] {}  ({} bytes, {})\n",
                    &entry.id[..8],
                    scope.name(),
                    entry.title,
                    entry.content.len(),
                    timestamp(entry.updated)
                ));
            }
            if entries.is_empty() {
                text.push_str("还没有记忆；/config memory add <text> 可添加。\n");
            }
            say(&text)
        }
        ["add", rest @ ..] if !rest.is_empty() => {
            let (scope, content) = match scope_of(rest[0]) {
                Some(scope) => (scope, text_after(rest[0])),
                None => (Scope::Project, text_after("add")),
            };
            if content.is_empty() {
                return Err(Error::Tool("用法：/config memory add [global] <text>".into()));
            }
            memory
                .authorize_write(scope, CancellationToken::new())
                .await?;
            let title: String = content
                .lines()
                .next()
                .unwrap_or_default()
                .chars()
                .take(60)
                .collect();
            let session = app.session.id.clone();
            let saved = blocking(move || {
                memory.save(
                    scope,
                    None,
                    title,
                    content,
                    "operator via /config memory add".into(),
                    ProvenanceSource {
                        kind: "operator".into(),
                        session,
                        ..ProvenanceSource::default()
                    },
                )
            })
            .await?;
            say(&format!(
                "已保存记忆 {} [{}]\n",
                saved["saved"].as_str().unwrap_or_default(),
                scope.name()
            ))
        }
        ["remove" | "delete", id] => {
            let reference = id.to_string();
            let lookup = memory.clone();
            let (scope, entry) = blocking(move || lookup.find(&reference)).await?;
            memory
                .authorize_write(scope, CancellationToken::new())
                .await?;
            let id = entry.id.clone();
            blocking(move || memory.remove(scope, &id)).await?;
            say(&format!(
                "已删除记忆 {} [{}] {}\n",
                &entry.id[..8],
                scope.name(),
                entry.title
            ))
        }
        ["search", ..] => {
            let query = text_after("search");
            if query.is_empty() {
                return Err(Error::Tool("用法：/config memory search <query>".into()));
            }
            let hits = blocking(move || memory.search(&[Scope::Project, Scope::Global], &query, 10))
                .await?;
            let mut text = String::new();
            for (scope, entry, score) in &hits {
                text.push_str(&format!(
                    "{score:>6.3}  {} [{}] {}\n        {}\n",
                    &entry.id[..8],
                    scope.name(),
                    entry.title,
                    clip(&entry.content.replace('\n', " "), 160)
                ));
            }
            if hits.is_empty() {
                text.push_str("没有匹配的记忆。\n");
            }
            say(&text)
        }
        ["stats"] => {
            let entries = blocking(move || memory.all()).await?;
            let bytes = |scope: Scope| -> (usize, usize) {
                let list: Vec<_> = entries.iter().filter(|(s, _)| *s == scope).collect();
                (list.len(), list.iter().map(|(_, e)| e.content.len()).sum())
            };
            let (project, project_bytes) = bytes(Scope::Project);
            let (global, global_bytes) = bytes(Scope::Global);
            let total = project_bytes + global_bytes;
            let mut text = format!(
                "项目：{project} 条，{project_bytes} 字节（{}）\n全局：{global} 条，{global_bytes} 字节（{}）\n限制：每条 25 KiB，每个范围 300 KiB\n注入方式：{}\n",
                app.sessions.memory.directory(Scope::Project).display(),
                app.sessions.memory.directory(Scope::Global).display(),
                if total <= INLINE_LIMIT {
                    "全部记忆直接注入（<= 24 KiB）".to_owned()
                } else {
                    format!("索引 + 每个任务最相关的 {} 条", crate::memory::recall::TOP_K)
                }
            );
            text.push_str(&format!(
                "向量检索：{}\n",
                if app.sessions.recall.embedder.is_some() {
                    "已配置 memory_embedding（与 BM25 按 0.5 混合）"
                } else {
                    "关闭（仅 BM25）"
                }
            ));
            text.push_str(&app.sessions.extractor.status());
            text.push_str(&format!(
                "会话笔记：{}\n",
                app.sessions.notes.path().display()
            ));
            say(&text)
        }
        ["source", id] => {
            let reference = id.to_string();
            let (scope, entry) = blocking(move || memory.find(&reference)).await?;
            let mut text = format!(
                "{} [{}] {}\n来源：{}\n创建 {}，更新 {}\n",
                entry.id,
                scope.name(),
                entry.title,
                entry.source,
                timestamp(entry.created),
                timestamp(entry.updated)
            );
            if entry.provenance.is_empty() {
                text.push_str("没有出处记录（旧条目或外部写入的条目）\n");
            }
            let current = crate::memory::store::content_hash(&entry.content);
            for source in &entry.provenance {
                text.push_str(&format!(
                    "- {} 于 {}{}{}{}{}\n",
                    source.kind,
                    timestamp(source.at),
                    if source.session.is_empty() {
                        String::new()
                    } else {
                        format!("，会话 {}", &source.session[..source.session.len().min(8)])
                    },
                    source
                        .messages
                        .map_or(String::new(), |(a, b)| format!("，消息 {a}-{b}")),
                    if source.transcript_hash.is_empty() {
                        String::new()
                    } else {
                        format!("，对话记录 {}", &source.transcript_hash[..12.min(source.transcript_hash.len())])
                    },
                    if source.content_hash == current {
                        "，与当前内容一致"
                    } else {
                        "，内容此后已改变"
                    }
                ));
                if !source.evidence.is_empty() {
                    for line in source.evidence.lines().take(12) {
                        text.push_str(&format!("    {line}\n"));
                    }
                }
            }
            text.push_str("出处只记录输入来源，不代表内容属实；依赖前请先核实。\n");
            say(&text)
        }
        ["dream"] | ["dream", "status"] => say(&app.sessions.dreamer.status()),
        ["dream", "run"] => {
            let dreamer = app.sessions.dreamer.clone();
            if app.settings.one_shot {
                let summary = dreamer.run("manual").await?;
                say(&format!("{summary}\n"))
            } else {
                tokio::spawn(async move {
                    let outcome = dreamer.run("manual").await;
                    let _ = print_text(
                        &match outcome {
                            Ok(_) => "\n[dream] 记忆整理完成；/config memory dream status 查看摘要。\n".into(),
                            Err(err) => format!("\n[dream] {err}\n"),
                        },
                        true,
                    );
                });
                say("已在后台开始整理记忆（只使用记忆工具，最长 5 分钟）；/config memory dream status 查看进度。\n")
            }
        }
        _ => Err(Error::Tool(
            "用法：/config memory [list [project|global]|add [global] <text>|remove <id>|search <query>|stats|source <id>|dream [status|run]] 或 /config memory <tool JSON>".into(),
        )),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn markdown_export_includes_tool_calls_and_results() {
        let mut session = Session::new("/w".into(), "Openai".into(), "m".into(), None);
        session.messages.push(Message::user("Fix the bug"));
        session.messages.push(Message {
            role: Role::Assistant,
            blocks: vec![
                Block::Text("Looking.".into()),
                Block::ToolUse {
                    id: "1".into(),
                    name: "read".into(),
                    input: serde_json::json!({"path":"a.rs"}),
                    extra: None,
                },
            ],
        });
        session.messages.push(Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: "1".into(),
                content: "fn main() {} ```".into(),
                is_error: true,
            }],
        });
        let text = markdown(&session);
        assert!(text.starts_with("# Fix the bug\n"));
        assert!(text.contains("**工具调用 `read`**\n\n```json\n{\n  \"path\": \"a.rs\"\n}\n```"));
        assert!(text.contains("**工具结果（失败）**\n\n````text\nfn main() {} ```\n````"));
        let lines = history_lines(&[session.clone()], Some(&session.id), false);
        assert!(
            lines.contains("  1. ") && lines.contains("Fix the bug") && lines.contains("(current)")
        );
    }
}
