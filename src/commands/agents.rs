//! Sub-agent activity and lifecycle (`/status agents [verb id]`), plus the glue the REPL
//! calls between tasks: background notices, queued approvals and background spend.

use std::sync::Arc;

use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::{
    agents::background::MAIN,
    agents::lifecycle::Kind,
    agents::subagent::{
        AgentHub, HubParts, ModelCatalog, SerialAuthorizer, SubAgentSettings, ToolScope,
        TypeRegistry,
    },
    app::ui::print_text,
    app::{Application, fresh, validate_identity},
    config::Settings,
    domain::{Error, Progress, Provider, Result, UserInput},
    engine::agent::{Ledger, add_spend},
    model::routing::{ModelPolicy, ModelRouter},
    safety::permission::{Authorizer, LocalAuthorizer, Mode},
    session::workspace::Workspace,
    session::{SessionPersistence, State, Store},
    tools::extension_tools::ExtensionTools,
    tools::web::Web,
    tools::{LocalTools, ToolExecutor},
};

pub(crate) const HELP: &str = "/status agents [show|stop|park|resume|send|merge|discard <id|main> [confirm] [text]]  子代理和主任务：列出、查看完整报告、中断、挂起、唤醒、发消息、合并或丢弃后台代理的分支；/status agents repair 恢复回归检查崩溃时留在基线状态的文件";

/// The router's models for sub-agents: live main and fast model, prices by model, and
/// each model's own window (learned from overflow errors, else the built-in table).
struct RouterModels {
    router: Arc<ModelRouter>,
    cache: std::path::PathBuf,
    provider: String,
    base_url: String,
    learned: serde_json::Value,
}

impl RouterModels {
    /// The window of `model` as `Settings::load` finds the main model's: learned from an
    /// overflow error (cache file, then `model_context_windows`), else the table.
    fn window(&self, model: &str) -> usize {
        let valid = |window: &u64| (8192..=10_000_000).contains(window);
        crate::persist::read_json::<serde_json::Value>(&self.cache)
            .ok()
            .and_then(|cache| {
                cache
                    .get(crate::config::model_context_cache_key(
                        &self.provider,
                        &self.base_url,
                        model,
                    ))?
                    .as_u64()
            })
            .filter(valid)
            .or_else(|| {
                self.learned
                    .get(model.to_ascii_lowercase())?
                    .as_u64()
                    .filter(valid)
            })
            .map_or_else(
                || crate::model::models::context_window(model),
                |window| window as usize,
            )
    }

    fn budget_of(&self, model: &str) -> (usize, u32) {
        let window = self.window(model);
        let output = crate::model::models::max_output_tokens(model, window);
        (
            crate::model::models::context_budget(window, output).min(10_000_000),
            output,
        )
    }
}

impl ModelCatalog for RouterModels {
    fn main_model(&self) -> String {
        self.router.main_model()
    }

    fn fast_model(&self) -> Option<String> {
        let main = self.router.main_model();
        self.router.fast_model().filter(|fast| *fast != main)
    }

    fn task_model(&self) -> String {
        self.router.current_model()
    }

    fn prices(&self, model: &str) -> Option<crate::model::models::Prices> {
        ModelPolicy::prices(&*self.router, model)
    }

    fn budget(&self, model: &str) -> (usize, u32) {
        self.budget_of(model)
    }
}

/// The sub-agents' view of `router`.
pub(crate) fn catalog(settings: &Settings, router: Arc<ModelRouter>) -> Arc<dyn ModelCatalog> {
    Arc::new(RouterModels {
        router,
        cache: settings
            .data_dir
            .join(crate::config::MODEL_CONTEXT_WINDOWS_CACHE),
        provider: settings.provider.clone(),
        base_url: settings.base_url.clone(),
        learned: settings
            .file
            .get("model_context_windows")
            .cloned()
            .unwrap_or_default(),
    })
}

/// Builds the sub-agent hub; tool factories re-root the file and shell tools per scope.
#[allow(clippy::too_many_arguments)]
pub(crate) fn hub(
    settings: &Settings,
    catalog: Arc<dyn ModelCatalog>,
    provider: Arc<dyn Provider>,
    tools: Arc<ExtensionTools>,
    authorizer: Arc<dyn Authorizer>,
    input: Option<Arc<dyn UserInput>>,
    ledger: Arc<Ledger>,
    progress: Arc<Progress>,
    workspace: Arc<Workspace>,
    store: Store,
    project_context: String,
) -> Result<Arc<AgentHub>> {
    let file = &settings.file;
    let types = TypeRegistry::from_config(file)?;
    let experimental = match file.get("experimental_tools") {
        None => false,
        Some(value) => value.as_bool().ok_or_else(|| {
            Error::Session("experimental_tools 必须是 true 或 false；请修改配置文件".into())
        })?,
    };
    let max_background = match file.get("max_background_agents") {
        None => 4,
        Some(value) => value
            .as_u64()
            .filter(|count| (1..=16).contains(count))
            .ok_or_else(|| {
                Error::Session("max_background_agents 必须在 1..16 之间；请修改配置文件".into())
            })? as usize,
    };
    let regression_command = match file.get("regression_command") {
        None => None,
        Some(value) => Some(
            value
                .as_str()
                .filter(|command| command.contains("{filter}") && command.len() < 4096)
                .ok_or_else(|| {
                    Error::Session(
                        "regression_command 必须是包含 {filter} 的命令字符串；请修改配置文件"
                            .into(),
                    )
                })?
                .to_owned(),
        ),
    };
    let overlay_check = match file.get("overlay_check") {
        None | Some(Value::Null) => None,
        Some(value) => match value.as_str().map(str::trim) {
            Some("") => None,
            Some(command) if command.len() < 4096 => Some(command.to_owned()),
            _ => {
                return Err(Error::Session(
                    "overlay_check 必须是命令字符串（例如 \"cargo check\"）；请修改配置文件".into(),
                ));
            }
        },
    };
    let mergiraf = match file.get("mergiraf_path") {
        None | Some(Value::Null) => which::which("mergiraf").ok(),
        Some(value) => {
            let path = value
                .as_str()
                .map(std::path::PathBuf::from)
                .filter(|path| path.is_absolute())
                .ok_or_else(|| {
                    Error::Session(
                        "mergiraf_path 必须是 mergiraf 程序的绝对路径；请修改配置文件".into(),
                    )
                })?;
            Some(path)
        }
    };
    let web_search = settings.web_search.clone();
    let factory_tools = tools.clone();
    let job_dir = store.root().join("tool-outputs").join("shell");
    let factory = Arc::new(move |scope: ToolScope| -> Result<Arc<dyn ToolExecutor>> {
        let local_authorizer: Arc<dyn Authorizer> = if scope.readonly {
            Arc::new(LocalAuthorizer {
                mode: Mode::Readonly,
                input: None,
            })
        } else {
            scope.authorizer.clone()
        };
        let mut local = LocalTools::new(scope.root.clone(), local_authorizer)?
            .with_progress(scope.progress)
            .with_job_dir(job_dir.clone());
        if let Some(workspace) = scope.workspace {
            local = local.with_workspace(workspace);
        }
        if let Some(stack) = scope.overlay {
            local = local.with_overlay(stack)?;
        }
        Ok(Arc::new(ExtensionTools {
            local: Arc::new(local),
            skills: factory_tools.skills.clone(),
            memory: factory_tools.memory.clone(),
            mcp: factory_tools.mcp.clone(),
            web: Arc::new(Web::new(web_search.clone(), scope.authorizer.clone())),
            authorizer: scope.authorizer,
        }))
    });
    AgentHub::new(HubParts {
        provider,
        factory,
        types,
        settings: SubAgentSettings {
            max_turns: settings.subagent_max_turns,
            stall_rounds: settings.stall_rounds,
            max_output_tokens: settings.max_output_tokens,
            prices: settings.prices,
            context_tokens: settings.context_tokens,
            project_context: Some(project_context),
            model: settings.model.clone(),
            protocol: format!("{:?}", settings.protocol),
            max_background,
            experimental,
            regression_command,
            catalog,
            explore_fast: settings.explore_fast,
            overlay_check,
            mergiraf,
        },
        ledger,
        progress,
        root: settings.cwd.canonicalize()?,
        workspace,
        store,
        authorizer: Arc::new(SerialAuthorizer {
            inner: authorizer,
            lock: tokio::sync::Mutex::new(()),
        }),
        permission: settings.permission,
        interactive: input.is_some(),
        input,
    })
}

/// Background spend recorded since the last tool round goes into the session.
pub(crate) fn absorb(app: &mut Application) -> Result<()> {
    if let Some(spent) = app.agents.ledger.take_background() {
        add_spend(&mut app.session.accounting, &spent);
        app.store.save(&app.session)?;
    }
    Ok(())
}

pub(crate) fn before_task(app: &mut Application) -> Result<()> {
    absorb(app)?;
    app.agents.plan.seed(&app.session.messages);
    Ok(())
}

fn print_notices(hub: &AgentHub) {
    for notice in hub.notices_for_user() {
        let _ = print_text(&format!("\n{notice}\n"), true);
    }
}

/// While the REPL waits for a line: show notices, and point at waiting approvals.
pub(crate) fn print_idle(hub: &AgentHub) {
    print_notices(hub);
    let approvals = hub.approvals.descriptions();
    if !approvals.is_empty() {
        let _ = print_text(
            &format!(
                "\n[approval waiting] {}；按回车键回答\n",
                approvals.join("; ")
            ),
            true,
        );
    }
}

/// Between tasks: absorb background spend, show notices, ask queued approvals.
pub(crate) async fn between_tasks(app: &mut Application) -> Result<()> {
    absorb(app)?;
    print_notices(&app.agents);
    for approval in app.agents.approvals.take_all() {
        let approved = match &app.input {
            Some(input) => input
                .answer(
                    format!("[{}] 批准 {}？[y/N] ", approval.agent, approval.description),
                    CancellationToken::new(),
                )
                .await
                .is_ok_and(|answer| answer.trim().eq_ignore_ascii_case("y")),
            None => false,
        };
        approval.answer(approved);
    }
    if let Some(files) = crate::agents::regression::pending_files(&app.workspace) {
        print_text(
            &format!("[warning] 中断后 {files} 仍是回归基线内容；/status agents repair 可恢复\n"),
            true,
        )?;
    }
    Ok(())
}

/// One-shot runs wait for their background agents; the REPL interrupts them (resumable).
pub(crate) async fn shutdown(app: &mut Application) {
    let hub = app.agents.clone();
    if hub.running_count() > 0 {
        if app.settings.one_shot {
            let _ = print_text(
                &format!(
                    "[agents] 正在等待 {} 个后台代理，最多 {} 秒\n",
                    hub.running_count(),
                    app.settings.timeout.as_secs()
                ),
                true,
            );
            hub.wait_all(app.settings.timeout, |notice| {
                let _ = print_text(&format!("\n{notice}\n"), true);
            })
            .await;
        } else {
            hub.stop_all().await;
            let _ = print_text(
                "[agents] 运行中的后台代理已中断；/status agents resume <id> 可继续\n",
                true,
            );
        }
    }
    print_notices(&hub);
    let _ = absorb(app);
}

fn main_line(app: &Application) -> String {
    format!(
        "main     main       {:<11} session={} pending={} cost={} task=\"{}\"",
        match &app.session.state {
            State::Running => "running".to_owned(),
            State::Interrupted(_) => "interrupted".to_owned(),
            State::Completed => "finished".to_owned(),
            State::Idle => "idle".to_owned(),
        },
        &app.session.id[..8],
        app.session.pending.len(),
        app.session
            .accounting
            .cost
            .map_or("unknown".into(), |cost| format!("${cost:.4}")),
        crate::text::short(&app.session.current_request, 60)
    )
}

fn is_main(app: &Application, id: &str) -> bool {
    id == MAIN || (id.len() >= 4 && app.session.id.starts_with(id))
}

async fn park_main(app: &mut Application) -> Result<String> {
    if app.session.messages.is_empty() {
        return Err(Error::Session("主会话没有可挂起的任务".into()));
    }
    let record = app.agents.park_main(&app.session)?;
    app.close_session().await?;
    app.store.save(&app.session)?;
    let session = fresh(&app.settings);
    let lease = app.store.acquire(&session.id)?;
    app.store.save(&session)?;
    app.workspace.activate(None)?;
    app.session = session;
    app.lease = lease;
    Ok(format!(
        "主任务已挂起为 {}（会话、待处理调用和检查点均保留；文件不会回退）。新会话 {}。\
         /status agents resume {} 可唤醒它。",
        record.short(),
        app.session.id,
        record.short()
    ))
}

/// Continues the foreground session, optionally with a message.
async fn run_main(app: &mut Application, message: Option<String>) -> Result<()> {
    match app.session.state {
        State::Interrupted(_) | State::Running => {
            if let Some(message) = message {
                app.agents.inbox.push(MAIN, message);
            }
            app.execute(None).await
        }
        State::Completed | State::Idle => match message {
            Some(message) => app.execute(Some(message)).await,
            None => {
                print_text("已唤醒；该任务已经完成，请输入下一个请求。\n", false)?;
                Ok(())
            }
        },
    }
}

async fn wake_main(app: &mut Application, id: &str, message: Option<String>) -> Result<()> {
    let record = app.agents.find_record(id)?;
    if record.session_id != app.session.id {
        if !app.session.pending.is_empty()
            || matches!(app.session.state, State::Interrupted(_) | State::Running)
        {
            return Err(Error::Session(
                "当前任务尚未完成；请先 /status agents park main 挂起它，或 /continue 完成它"
                    .into(),
            ));
        }
        let lease = app.store.acquire(&record.session_id)?;
        let session = app.store.load(&record.session_id)?;
        validate_identity(&session, &app.settings)?;
        app.close_session().await?;
        app.store.save(&app.session)?;
        app.workspace
            .activate(session.current_checkpoint.as_deref())?;
        app.session = session;
        app.lease = lease;
    }
    app.agents.wake_main(&record.id)?;
    print_text(
        &format!("[session] {}（已从挂起中唤醒）\n", app.session.id),
        false,
    )?;
    run_main(app, message).await
}

pub(crate) async fn handle(
    app: &mut Application,
    line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    if args.first() == Some(&"/resolve") {
        // A crashed regression check may have left files on their baseline: restore first.
        if let Some(text) = crate::agents::regression::repair(&app.workspace)? {
            print_text(&format!("{text}\n"), true)?;
        }
        return Ok(None);
    }
    if args.get(..2) != Some(&["/status", "agents"][..]) {
        return Ok(None);
    }
    let hub = app.agents.clone();
    let cancel = CancellationToken::new();
    // Free text after "<verb> <id> [confirm]", with its original spacing.
    let rest = |skip: usize| -> Option<String> {
        let mut text = line.trim_start();
        for _ in 0..skip {
            text = text
                .trim_start()
                .split_once(char::is_whitespace)
                .map_or("", |(_, rest)| rest);
        }
        let text = text.trim();
        (!text.is_empty()).then(|| text.to_owned())
    };
    let output = match &args[2..] {
        [] => hub.status_text(Some(main_line(app))).await,
        ["repair"] => crate::agents::regression::repair(&app.workspace)?
            .unwrap_or_else(|| "没有待恢复的回归基线文件".into()),
        [verb, id, ..] => {
            let main = is_main(app, id);
            match (*verb, main) {
                ("show", true) => {
                    return Err(Error::Session(
                        "主任务没有后台报告；它的过程在当前会话里".into(),
                    ));
                }
                ("show", false) => hub.report_text(id)?,
                ("stop", true) => "前台任务用 Ctrl+C 停止；任务之间它不会运行".into(),
                ("park", true) => park_main(app).await?,
                ("resume" | "send", true) => {
                    run_main(app, rest(4)).await?;
                    return Ok(Some(false));
                }
                ("merge" | "discard", true) => {
                    return Err(Error::Session(
                        "主任务直接在你的工作区中修改；查看用 /diff，撤销用 /undo".into(),
                    ));
                }
                ("stop", false) => hub.stop_agent(id, false).await?,
                ("park", false) => hub.park_agent(id, &cancel).await?,
                ("resume", false) => {
                    let confirm = args.get(4) == Some(&"confirm");
                    let message = rest(if confirm { 5 } else { 4 });
                    if hub.find_record(id)?.kind == Kind::Main {
                        wake_main(app, id, message).await?;
                        return Ok(Some(false));
                    }
                    hub.resume_agent(id, message, confirm).await?
                }
                ("send", false) => {
                    let Some(message) = rest(4) else {
                        return Err(Error::Session(
                            "send 需要消息内容；用法：/status agents send <id|main> <消息>".into(),
                        ));
                    };
                    if hub.find_record(id)?.kind == Kind::Main {
                        wake_main(app, id, Some(message)).await?;
                        return Ok(Some(false));
                    }
                    hub.send_agent(id, message).await?
                }
                ("merge", false) => hub.merge_agent(id, &cancel).await?,
                ("discard", false) => hub.discard_agent(id, &cancel).await?,
                _ => {
                    return Err(Error::Session(format!(
                        "未知的 /status agents 操作；用法：\n  {HELP}"
                    )));
                }
            }
        }
        _ => return Err(Error::Session(format!("用法：\n  {HELP}"))),
    };
    print_text(&format!("{}\n", output.trim_end()), false)?;

    Ok(Some(false))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_sub_agent_model_gets_its_own_window() {
        let dir = tempfile::tempdir().unwrap();
        let provider = Arc::new(
            crate::model::provider::HttpProvider::new(
                crate::config::Protocol::Openai,
                "http://127.0.0.1:9/v1",
                "claude-opus-5-5".into(),
                "test-only-not-a-real-key",
            )
            .unwrap(),
        );
        let router = Arc::new(ModelRouter::new(
            provider,
            Some("claude-haiku-4-5".into()),
            None,
        ));
        let cache = dir.path().join("windows.json");
        let models = RouterModels {
            router,
            cache: cache.clone(),
            provider: "openai".into(),
            base_url: "http://127.0.0.1:9/v1".into(),
            learned: serde_json::json!({"learned-model": 50_000}),
        };
        assert_eq!(models.fast_model().as_deref(), Some("claude-haiku-4-5"));
        // The fast model's window is Haiku's 200K, not the main model's 1M.
        let (haiku, _) = models.budget("claude-haiku-4-5");
        let (opus, _) = models.budget("claude-opus-5-5");
        assert!(haiku <= 200_000 * 85 / 100, "{haiku}");
        assert!(opus > 500_000, "{opus}");
        assert_eq!(models.window("learned-model"), 50_000);
        let key = crate::config::model_context_cache_key(
            "openai",
            "http://127.0.0.1:9/v1",
            "claude-haiku-4-5",
        );
        std::fs::write(&cache, serde_json::json!({ key: 120_000 }).to_string()).unwrap();
        assert_eq!(models.window("claude-haiku-4-5"), 120_000);
    }
}
