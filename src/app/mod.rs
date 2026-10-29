//! Assembly root and terminal front end: builds the Application, runs the REPL and one-shot tasks, setup, launch/restart, UI, diagnostics.

pub mod blocks;
pub mod completion;
pub mod console;
pub mod diagnostics;
pub mod launch;
pub mod onboarding;
pub mod paste;
pub mod render;
pub mod setup;
pub mod ui;

use std::{future::Future, io::IsTerminal, sync::Arc, time::Duration};

use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    agents::subagent::{AllowedTools, READ_TOOLS, WithSubAgent},
    app::ui::{ConsoleInput, ReplInput, consume_events, print_error, print_report, print_text},
    config::Settings,
    domain::{Error, Message, Progress, Provider, Result, UserInput},
    engine::agent::{Agent, Ledger, Limits},
    extensions::hooks::{HookState, Hooks, Lifecycle},
    extensions::mcp::{Mcp, McpArgs},
    extensions::{Catalog, HookEvent, Registry},
    memory::store::Memory,
    safety::permission::{Authorizer, Effect, LocalAuthorizer, Mode, Permissions},
    safety::verify::{Outcome, Verifier},
    session::workspace::Workspace,
    session::{CallState, Lease, Session, SessionPersistence, State, Store},
    tools::ToolExecutor,
    tools::extension_tools::ExtensionTools,
    tools::web::Web,
    tools::{LocalTools, ToolOutput},
};

pub async fn run(settings: Settings) -> anyhow::Result<()> {
    let input: Option<Arc<dyn UserInput>> = if settings.interactive
        && std::io::stdin().is_terminal()
        && std::io::stderr().is_terminal()
    {
        Some(Arc::new(ConsoleInput))
    } else {
        None
    };
    let store = Store::open(&settings.data_dir)?;
    // Started in the home directory (or with the data directory inside the project):
    // the data directory stays out of every tool's reach.
    if store.root().starts_with(settings.cwd.canonicalize()?) {
        crate::safety::protected::protect(store.root());
    }
    if let Some(what) = crate::safety::protected::too_wide(&settings.cwd) {
        let what = match what {
            "a filesystem root" => "文件系统根目录",
            "your home directory" => "用户主目录",
            other => other,
        };
        print_text(
            &format!(
                "[warning] 当前工作区是{what}（{}）：文件工具能访问其下的所有内容，自动快照已关闭。日常使用请在项目目录里启动 cove（或加 --cwd <目录>）。\n",
                settings.cwd.display()
            ),
            true,
        )?;
    }
    let workspace_lease = store.acquire_workspace(&settings.cwd)?;
    // Before any tool or prompt text names the shell.
    if let Some(value) = settings
        .file
        .get("shell")
        .and_then(serde_json::Value::as_str)
    {
        crate::process::Shell::install(crate::process::Shell::from_setting(value)?);
    }
    let workspace = Arc::new(Workspace::new(settings.cwd.clone(), store.clone())?);
    let remote = Arc::new(crate::workflows::remote::Hub::default());
    let permissions = Arc::new(
        Permissions::new(
            settings.permission,
            settings.cwd.clone(),
            vec![
                settings.cwd.clone(),
                store.root().to_path_buf(),
                settings.data_dir.clone(),
            ],
            input.clone(),
        )
        .with_policies(store.root().join("policies.json"))?,
    );
    let authorizer: Arc<dyn Authorizer> = Arc::new(crate::workflows::remote::RemoteAuthorizer {
        inner: permissions.clone(),
        hub: remote.clone(),
    });
    let diagnostics = Arc::new(
        crate::app::diagnostics::Diagnostics::new(store.root()).with_secret(&settings.api_key),
    );
    diagnostics.trace.set_echo(settings.debug);
    crate::app::blocks::VERBOSE.store(settings.verbose, std::sync::atomic::Ordering::Relaxed);
    let router = crate::commands::model::new_router(&settings)?;
    crate::app::ui::set_show_reasoning(settings.show_reasoning);
    let traced = |inner: Arc<dyn Provider>| -> Arc<dyn Provider> {
        Arc::new(crate::app::diagnostics::Traced {
            inner,
            model: settings.model.clone(),
            diagnostics: diagnostics.clone(),
        })
    };
    // Every model call is metered into the usage ledger at this one boundary.
    let usage = Arc::new(crate::session::usage::UsageLedger::new(store.root()));
    let metered = |inner: Arc<dyn Provider>, source| -> Arc<dyn crate::domain::Provider> {
        Arc::new(crate::session::usage::Metered {
            inner,
            ledger: usage.clone(),
            provider: settings.provider.clone(),
            model: settings.model.clone(),
            prices: settings.prices,
            source,
        })
    };
    let provider = metered(traced(router.clone()), "session");
    // Cheap background calls (memory extraction, dream): the `model_fast` model when set.
    let background = metered(traced(router.background()), "background");
    let progress = Arc::new(Progress::default());
    let local = Arc::new(
        LocalTools::new(settings.cwd.clone(), authorizer.clone())?
            .with_user_input(input.clone())
            .with_workspace(workspace.clone())
            .with_progress(progress.clone()),
    );
    let registry = Arc::new(Registry::open(store.root())?);
    let extensions = crate::commands::extensions::assemble(&settings, store.root())?;
    let catalog = Catalog::load(extensions.config, &settings.extension_base, &registry)?;
    let (catalog, skills, plugins) = crate::commands::extensions::load(
        &settings,
        store.root(),
        catalog,
        extensions.hooks,
        registry.clone(),
    )?;
    let memory = Arc::new(Memory::new(
        store.root(),
        &settings.cwd,
        authorizer.clone(),
    )?);
    let mcp = Arc::new(Mcp::new(
        catalog.servers.clone(),
        &settings.cwd,
        std::env::current_exe()?,
        settings.trust_extensions,
        registry.clone(),
        authorizer.clone(),
        workspace.clone(),
        store.root().join("mcp-oauth"),
    ));
    let hooks = Arc::new(Hooks::new(
        catalog.hooks.clone(),
        registry.clone(),
        authorizer.clone(),
        workspace.clone(),
        settings.cwd.clone(),
        settings.trust_extensions,
    ));
    let tools = Arc::new(ExtensionTools {
        local: local.clone(),
        skills: skills.clone(),
        memory: memory.clone(),
        mcp,
        web: Arc::new(Web::new(settings.web_search.clone(), authorizer.clone())),
        authorizer: authorizer.clone(),
    });
    let project_context = {
        let cwd = settings.cwd.clone();
        let index = skills.index()?;
        tokio::task::spawn_blocking(move || crate::engine::prompt::system_context(&cwd, &index))
            .await
            .map_err(|err| Error::Tool(format!("project context failed: {err}")))?
    };
    let ledger = Arc::new(Ledger::default());
    let output_policy = Arc::new(crate::engine::tool_output::OutputPolicy::new(
        settings.context_window,
        Some(store.root().join("tool-outputs")),
    ));
    {
        let policy = output_policy.clone();
        tokio::task::spawn_blocking(move || policy.prune());
    }
    let readonly: Arc<dyn ToolExecutor> = Arc::new(AllowedTools {
        inner: Arc::new(LocalTools::new(
            settings.cwd.clone(),
            Arc::new(LocalAuthorizer {
                mode: Mode::Readonly,
                input: None,
            }),
        )?),
        allowed: READ_TOOLS,
    });
    let gates = crate::safety::gates::Assembly::new(
        &settings,
        crate::safety::gates::Parts {
            permissions: permissions.clone(),
            input: input.clone(),
            workspace: workspace.clone(),
            authorizer: authorizer.clone(),
            progress: progress.clone(),
            tools: tools.clone(),
            provider: provider.clone(),
            readonly,
            ledger: ledger.clone(),
            project_context: Some(project_context.clone()),
            trust: Arc::new(crate::config::trust::StoreTrust),
        },
    );
    let workflow_tools = Arc::new(crate::tools::workflow_tools::WorkflowTools {
        inner: gates.tools.clone(),
        root: settings.cwd.clone(),
        workspace: workspace.clone(),
        authorizer: authorizer.clone(),
        web: tools.web.clone(),
        browser_path: crate::commands::workflows::browser_path(&settings),
    });
    let agents = crate::commands::agents::hub(
        &settings,
        provider.clone(),
        tools.clone(),
        authorizer.clone(),
        input.is_some(),
        ledger.clone(),
        progress.clone(),
        workspace.clone(),
        store.clone(),
        project_context.clone(),
    )?;
    let agent_tools = Arc::new(WithSubAgent {
        inner: workflow_tools,
        hub: agents.clone(),
    });
    let sessions = crate::commands::sessions::Services::start(
        &settings,
        &store,
        memory,
        authorizer.clone(),
        background,
        usage,
    )
    .await;
    let agent_tools = sessions.wrap_tools(agent_tools, settings.permission);
    let mut agent = Agent::new(
        provider.clone(),
        agent_tools,
        Limits {
            max_turns: settings.max_turns,
            max_output_tokens: settings.max_output_tokens,
            budget: settings.budget,
            prices: settings.prices,
        },
    )?
    .with_context_tokens(settings.context_tokens)?
    .with_instructions(settings.instructions.clone())
    .with_verifier(
        gates
            .verifier
            .clone()
            .map(|verifier| -> Arc<dyn Verifier> { verifier }),
    )
    .with_ledger(ledger)
    .with_project_context(Some(project_context))
    .with_turn_note(Some(Arc::new(crate::engine::prompt::GitNote::new(
        settings.cwd.clone(),
    ))))
    .with_user_input(input.clone())
    .with_model_policy(router.clone())
    .with_output_policy(output_policy)
    .with_turn_hook(Arc::new(crate::engine::subdir_hints::SubdirHints::new(
        settings.cwd.clone(),
    )))
    .with_hooks(hooks.clone())
    .with_turn_hook(Arc::new(crate::app::diagnostics::TraceHook::new(
        diagnostics.clone(),
    )))
    .with_turn_hook(Arc::new(crate::workflows::remote::SteerHook(
        remote.clone(),
    )))
    .with_turn_hook(Arc::new(crate::app::console::SteerHook))
    .with_turn_hook(Arc::new(crate::agents::background::NoticeHook {
        hub: agents.clone(),
    }));
    for hook in sessions.hooks() {
        agent = agent.with_turn_hook(hook);
    }
    for hook in gates.hooks {
        agent = agent.with_turn_hook(hook);
    }
    // Skill review uses the cheap background model when one is configured.
    let agent =
        crate::commands::extensions::turn_hooks(agent, &settings, &skills, router.background());
    if settings.dump_system_prompt {
        print_text(&format!("{}\n", agent.system_prompt()), false)?;
        return Ok(());
    }
    let (session, lease) = match &settings.resume {
        Some(id) => {
            let lease = store.acquire(id)?;
            (store.load(id)?, lease)
        }
        None => {
            let session = fresh(&settings);
            let lease = store.acquire(&session.id)?;
            (session, lease)
        }
    };
    let mut session = session;
    adopt(&mut session, &settings)?;
    store.save(&session)?;
    workspace.activate(session.current_checkpoint.as_deref())?;
    // The interactive console names the session in its banner instead.
    if settings.one_shot || !crate::app::console::supported() {
        print_text(&format!("[session] {}\n", session.id), true)?;
    }
    let mut app = Application {
        tools,
        registry,
        catalog,
        plugins,
        hooks,
        settings,
        agent,
        store,
        workspace,
        authorizer,
        permissions,
        workflow: gates.workflow,
        input,
        session,
        lease,
        _workspace_lease: workspace_lease,
        progress,
        provider,
        diagnostics,
        remote,
        remote_server: None,
        sessions,
        models: crate::commands::model::ModelState {
            router,
            attachments: Vec::new(),
        },
        agents,
    };
    app.sessions.maintain(&app.store, &app.session.id);
    let outcome = app.interact().await;
    if let Err(err) = &outcome {
        app.record_failure("run", err);
    }
    if let Some(server) = app.remote_server.take() {
        server.stop().await;
    }
    app.sessions.shutdown().await;
    crate::commands::agents::shutdown(&mut app).await;
    let cleanup = app.close_session().await;
    match (outcome, cleanup) {
        (Ok(()), Ok(())) => Ok(()),
        (Err(err), Ok(())) | (Ok(()), Err(err)) => Err(err.into()),
        (Err(err), Err(cleanup)) => {
            Err(Error::Session(format!("{err}; shutdown: {cleanup}")).into())
        }
    }
}

impl Application {
    async fn interact(&mut self) -> Result<()> {
        if self.settings.one_shot {
            if self.settings.continue_task {
                self.execute(None).await?;
            } else {
                let prompt = self.settings.prompt.take().ok_or_else(|| {
                    Error::Session("缺少单次运行的提示词：用 cove -p \"提示词\" 运行".into())
                })?;
                if prompt.starts_with('/') {
                    self.command(&prompt).await?;
                } else {
                    self.execute(Some(prompt)).await?;
                }
            }
        } else {
            if self.input.is_some() && crate::app::console::supported() {
                let history = self.settings.data_dir.join("input_history.jsonl");
                crate::app::console::start(
                    Some(history),
                    self.settings.cwd.clone(),
                    self.settings.data_dir.join("clipboard"),
                )?;
            }
            let result = self.repl().await;
            crate::app::console::stop();
            result?;
            self.store.flush(&self.session)?;
        }
        Ok(())
    }

    async fn repl(&mut self) -> Result<()> {
        let mut reader = ReplInput::open();
        let panel = crate::app::console::active().map(|console| {
            let hub = self.agents.clone();
            tokio::spawn(async move {
                loop {
                    tokio::select! {
                        () = console.panel_opened() => {}
                        () = tokio::time::sleep(std::time::Duration::from_secs(2)) => {}
                    }
                    if console.panel_open() {
                        let text = hub.status_text(None).await;
                        console.set_panel(text.lines().map(str::to_owned).collect());
                    }
                }
            })
        });
        let result = self.repl_loop(&mut reader).await;
        if let Some(panel) = panel {
            panel.abort();
        }
        result
    }

    async fn repl_loop(&mut self, reader: &mut ReplInput) -> Result<()> {
        // The banner carries its own styles; print_text would show them as text.
        crate::app::console::write(true, &self.banner())?;
        if self.session.has_conversation() {
            crate::commands::sessions::show_conversation(&self.session)?;
        } else if let Some(notice) = crate::commands::sessions::startup_notice(self) {
            print_text(&notice, true)?;
        }
        let result = self.repl_lines(reader).await;
        if self.session.has_conversation() {
            print_text(
                &format!(
                    "会话已保存。下次在这里输入“继续”，或用 cove --resume {} 启动。\n",
                    &self.session.id[..8]
                ),
                true,
            )?;
        }
        result
    }

    /// Who answers and where: model, provider, permission mode, directory and branch.
    fn banner(&self) -> String {
        let branch = crate::engine::prompt::git(&self.settings.cwd, &["branch", "--show-current"])
            .map(|branch| branch.trim().to_owned())
            .filter(|branch| !branch.is_empty())
            .map_or(String::new(), |branch| format!(" ({branch})"));
        let cwd = self.settings.cwd.display().to_string();
        let cwd = cwd.strip_prefix(r"\\?\").unwrap_or(&cwd).to_owned();
        if crate::app::console::rich() {
            return format!(
                "\x1b[1;36mcove\x1b[0m \x1b[2m{}\x1b[0m  {} \x1b[2m({}) · {} 模式\x1b[0m\n\x1b[2m{cwd}{branch} · 会话 {}\x1b[0m\n\x1b[2m/help 命令 · / 或 @ 补全 · 两次 Esc 中断 · /exit 退出\x1b[0m\n",
                env!("CARGO_PKG_VERSION"),
                self.settings.model,
                self.settings.provider,
                self.permissions.mode().name(),
                &self.session.id[..8],
            );
        }
        format!(
            "cove {} · {}（{}）· 模式 {} · {cwd}{branch}\n/help 查看命令，输入 / 或 @ 可补全；按两次 Esc 或 Ctrl+C 停止正在运行的任务；/exit 保存并退出。\n",
            env!("CARGO_PKG_VERSION"),
            self.settings.model,
            self.settings.provider,
            self.permissions.mode().name(),
        )
    }

    async fn repl_lines(&mut self, reader: &mut ReplInput) -> Result<()> {
        loop {
            crate::commands::agents::between_tasks(self).await?;
            let console = crate::app::console::active();
            // Steering the last task ended before taking runs as the next task.
            let line = if let Some(text) = console.and_then(|console| console.take_steer()) {
                print_text("[steer] 任务已先结束，这条内容作为新消息发送\n", true)?;
                format!("[sent while the previous task was running]\n{text}")
            } else {
                let read = {
                    let agents = self.agents.clone();
                    let read = reader.read(crate::commands::completer(self));
                    tokio::pin!(read);
                    loop {
                        tokio::select! {
                            line = &mut read => break line,
                            _ = agents.background.notify.notified() => {
                                crate::commands::agents::print_idle(&agents);
                            }
                        }
                    }
                };
                match read {
                    Ok(Some(line)) => line,
                    Ok(None) | Err(Error::Cancelled | Error::InputClosed) => break,
                    Err(err) => return Err(err),
                }
            };
            let line = line.trim();
            if line.is_empty() {
                continue;
            }
            self.models.attachments.append(&mut reader.attachments);
            if let Some(console) = console {
                console.set_busy(true);
            }
            let result = if line == "/" {
                // A bare `/` asks for the command list.
                self.command("/help").await
            } else if line.starts_with('/') {
                self.command(line).await
            } else if let Some(result) = crate::commands::sessions::intercept(self, line).await {
                result
            } else {
                self.execute(Some(line.into())).await.map(|_| false)
            };
            if let Some(console) = console {
                console.set_busy(false);
            }
            match result {
                Ok(true) => break,
                Ok(false) => {}
                Err(err) => {
                    self.record_failure("command", &err);
                    print_error(&err.to_string())?
                }
            }
        }
        Ok(())
    }
}

pub(crate) fn fresh(settings: &Settings) -> Session {
    Session::new(
        settings.cwd.clone(),
        format!("{:?}", settings.protocol),
        settings.model.clone(),
        settings.prices,
    )
}

/// A session belongs to its workspace; it may continue on another model or provider
/// (each request is accounted to the model that served it).
pub(crate) fn validate_identity(session: &Session, settings: &Settings) -> Result<()> {
    if session.cwd != settings.cwd {
        return Err(Error::Session(format!(
            "该会话属于另一个工作区（{}）；请在那个目录启动 cove 再恢复它，或用 /new 在当前目录开新会话",
            session.cwd.display()
        )));
    }
    Ok(())
}

/// `validate_identity`, then records the provider and model now in use.
pub(crate) fn adopt(session: &mut Session, settings: &Settings) -> Result<()> {
    validate_identity(session, settings)?;
    session.protocol = format!("{:?}", settings.protocol);
    session.model = settings.model.clone();
    Ok(())
}

pub(crate) struct Application {
    pub(crate) tools: Arc<ExtensionTools>,
    pub(crate) registry: Arc<Registry>,
    pub(crate) catalog: Catalog,
    /// Plugins in `<data_dir>/plugins` (commands, skills).
    pub(crate) plugins: crate::extensions::plugins::Plugins,
    pub(crate) hooks: Arc<Hooks>,
    pub(crate) settings: Settings,
    pub(crate) agent: Agent,
    pub(crate) store: Store,
    pub(crate) workspace: Arc<Workspace>,
    pub(crate) authorizer: Arc<dyn Authorizer>,
    /// The live mode and rules behind `authorizer` (`/mode`).
    pub(crate) permissions: Arc<Permissions>,
    /// Review/once workflow state (`/mode review|once|direct`).
    pub(crate) workflow: Arc<crate::safety::gates::plan::Workflow>,
    pub(crate) input: Option<Arc<dyn UserInput>>,
    pub(crate) session: Session,
    pub(crate) lease: Lease,
    pub(crate) _workspace_lease: Lease,
    pub(crate) progress: Arc<Progress>,
    /// Main model provider (trace-decorated).
    #[allow(dead_code)] // For commands that call the main model directly.
    pub(crate) provider: Arc<dyn Provider>,
    pub(crate) diagnostics: Arc<crate::app::diagnostics::Diagnostics>,
    /// Remote supervision state, shared with the authorizer and the steer hook.
    pub(crate) remote: Arc<crate::workflows::remote::Hub>,
    pub(crate) remote_server: Option<crate::workflows::remote::Server>,
    pub(crate) sessions: Arc<crate::commands::sessions::Services>,
    pub(crate) agents: Arc<crate::agents::subagent::AgentHub>,
    pub(crate) models: crate::commands::model::ModelState,
}

impl Application {
    /// Provider for cheap background calls (summaries, memory extraction, reviews):
    /// the `model_fast` model when configured, else the main model.
    #[allow(dead_code)] // Shared contract for background callers.
    pub(crate) fn background_provider(&self) -> Arc<dyn Provider> {
        self.models.router.background()
    }

    /// Feeds `errors.log`. Model and task failures are logged where they happen
    /// (trace provider and hook); a mistyped command is not a failure.
    pub(crate) fn record_failure(&self, source: &str, err: &Error) {
        let logged = matches!(err, Error::Provider(_) | Error::Limit(_) | Error::Cancelled)
            || matches!(err, Error::Session(text)
                if text.starts_with("未知命令") || text.contains("的参数不对")
                    || text.starts_with("unknown command") || text.starts_with("invalid arguments"));
        if !logged {
            self.diagnostics.error(source, &err.to_string());
        }
    }

    pub(crate) async fn close_session(&mut self) -> Result<()> {
        let hook = tokio::time::timeout(
            Duration::from_secs(10),
            self.hooks.run(
                HookEvent::SessionEnd,
                &mut self.session,
                &self.store,
                serde_json::json!({}),
                CancellationToken::new(),
            ),
        )
        .await
        .map_err(|_| {
            Error::Session(
                "SessionEnd 钩子 10 秒内未结束；会话已保存，可用 /config hooks 查看运行记录".into(),
            )
        })
        .and_then(|r| r);
        let mcp = self.tools.mcp.close(None, None).await;
        let save = self.store.flush(&self.session);
        let errors = [hook, mcp, save]
            .into_iter()
            .filter_map(|result| result.err().map(|err| err.to_string()))
            .collect::<Vec<_>>();
        if !errors.is_empty() {
            return Err(Error::Session(errors.join("; ")));
        }
        Ok(())
    }

    async fn install_plugin(&mut self, path: &std::path::Path) -> Result<()> {
        let path = path.canonicalize()?;
        let manifest = Catalog::validate_plugin(&path)?;
        if self.catalog.plugins.contains_key(&manifest.name)
            || self
                .registry
                .snapshot()?
                .installed
                .contains_key(&manifest.name)
        {
            return Err(Error::Tool(format!(
                "插件 {0} 已注册；如需重新安装，先执行 /config plugins uninstall {0}",
                manifest.name
            )));
        }
        self.authorizer
            .authorize(
                Effect::Write(self.registry.path().into()),
                CancellationToken::new(),
            )
            .await?;
        self.registry.change(|state| {
            if state.installed.len() >= 32 {
                return Err(Error::Tool(
                    "最多安装 32 个插件；先用 /config plugins uninstall <name> 卸载不用的".into(),
                ));
            }
            state.disabled.remove(&format!("plugin:{}", manifest.name));
            state.installed.insert(manifest.name, path);
            Ok(())
        })?;
        print_text(
            "本地插件清单已登记，/restart 后加载；可执行扩展仍需 --trust-extensions 并在运行时授权。源文件没有被复制或执行。\n",
            false,
        )?;
        Ok(())
    }

    async fn configure(&mut self, args: &[&str]) -> Result<()> {
        let value = match args {
            [] => serde_json::json!({"commands":[
                "/config skills", "/config mcp", "/config hooks", "/config plugins",
                "/config <skills|mcp|hooks|plugins> enable|disable <id>",
                "/config hooks resolve <run-id>",
                "/config plugins install <local-manifest-path>", "/config plugins uninstall <name>",
                "/config memory <JSON matching the memory tool schema>"
            ],"trust_extensions":self.settings.trust_extensions}),
            ["skills"] => self
                .tools
                .skills
                .execute(crate::extensions::skills::SkillArgs::List)?,
            ["mcp"] => {
                let result = self
                    .tools
                    .mcp
                    .execute(McpArgs::Servers, CancellationToken::new())
                    .await?;
                print_text(&format!("{}\n", result.content), false)?;
                return Ok(());
            }
            ["hooks"] => {
                let mut list = Vec::new();
                for hook in &self.catalog.hooks {
                    list.push(serde_json::json!({"id":hook.id,"event":hook.value.event,"plugin":hook.plugin,
                        "enabled":self.registry.active("hook",&hook.id,hook.plugin.as_deref())?}));
                }
                serde_json::json!({"configured":list,"recent_runs":self.session.hook_runs})
            }
            ["plugins"] => {
                let mut plugins = self.catalog.plugins.clone();
                plugins.extend(self.registry.snapshot()?.installed);
                let mut list = Vec::new();
                for (id, path) in plugins {
                    list.push(serde_json::json!({"id":id,"source":path,"loaded":self.catalog.plugins.contains_key(&id),
                        "enabled":self.registry.active("plugin",&id,None)?}));
                }
                serde_json::json!(list)
            }
            ["hooks", "resolve", id] => {
                self.authorizer
                    .authorize(
                        Effect::Write(
                            self.store
                                .root()
                                .join("sessions")
                                .join(format!("{}.json", self.session.id)),
                        ),
                        CancellationToken::new(),
                    )
                    .await?;
                let run = self
                    .session
                    .hook_runs
                    .iter_mut()
                    .find(|run| run.id == *id)
                    .ok_or_else(|| {
                        Error::Session(format!(
                            "找不到钩子运行记录 {id}；/config hooks 列出最近的运行及其 ID"
                        ))
                    })?;
                if !matches!(run.state, HookState::Running | HookState::Uncertain) {
                    return Err(Error::Session(
                        "这次钩子运行已有结果，无需处理；/config hooks 查看状态".into(),
                    ));
                }
                run.state = HookState::Acknowledged;
                run.outcome.push_str("\nOperator acknowledged effects; not evidence of success. Hook will not be retried.");
                self.store.save(&self.session)?;
                serde_json::json!({"acknowledged":id,"warning":"不会重放或验证该钩子；如有结果不确定的工具调用，请另行用 /resolve 处理。"})
            }
            ["plugins", "uninstall", id] => {
                if !self.registry.snapshot()?.installed.contains_key(*id) {
                    return Err(Error::Tool(format!(
                        "插件 {id} 不是用 /config plugins install 安装的；可用 /config plugins disable {id} 停用，或从配置文件的扩展设置里删掉它"
                    )));
                }
                self.authorizer
                    .authorize(
                        Effect::Write(self.registry.path().into()),
                        CancellationToken::new(),
                    )
                    .await?;
                self.registry.change(|state| {
                    state.installed.remove(*id);
                    state.disabled.insert(format!("plugin:{id}"));
                    Ok(())
                })?;
                self.tools.mcp.close(Some(id), None).await?;
                serde_json::json!({"uninstalled":id,"source_files_preserved":true})
            }
            [kind, action, id] if matches!(*action, "enable" | "disable") => {
                let category = match *kind {
                    "skills" if self.tools.skills.contains(id) => "skill",
                    "mcp" if self.catalog.servers.iter().any(|s| s.id == *id) => "mcp",
                    "hooks" if self.catalog.hooks.iter().any(|h| h.id == *id) => "hook",
                    "plugins"
                        if self.catalog.plugins.contains_key(*id)
                            || self.registry.snapshot()?.installed.contains_key(*id) =>
                    {
                        "plugin"
                    }
                    _ => {
                        return Err(Error::Tool(format!(
                            "没有这个扩展：{kind} {id}；可用 /config skills|mcp|hooks|plugins 列出可用的 ID"
                        )));
                    }
                };
                self.authorizer
                    .authorize(
                        Effect::Write(self.registry.path().into()),
                        CancellationToken::new(),
                    )
                    .await?;
                self.registry.change(|state| {
                    let key = format!("{category}:{id}");
                    if *action == "disable" {
                        state.disabled.insert(key);
                    } else {
                        state.disabled.remove(&key);
                    }
                    Ok(())
                })?;
                if *action == "disable" {
                    if category == "mcp" {
                        self.tools.mcp.close(None, Some(id)).await?;
                    }
                    if category == "plugin" {
                        self.tools.mcp.close(Some(id), None).await?;
                    }
                }
                serde_json::json!({"id":id,"action":action,"note":"已加载的条目立即生效；新登记的插件需 /restart 后生效。"})
            }
            _ => {
                return Err(Error::Tool(
                    "无法识别的 /config 用法；输入 /config 查看全部子命令".into(),
                ));
            }
        };
        print_text(&format!("{}\n", crate::tools::json_output(value)?), false)?;
        Ok(())
    }

    /// Interrupted calls that need no operator: side-effect-free tools (reads,
    /// searches, questions, the plan) run again, and a file edit counts as done or
    /// not done when the file holds the intended or the earlier content.
    fn settle_interrupted(&mut self) -> Result<()> {
        if !matches!(self.session.state, State::Interrupted(_)) {
            return Ok(());
        }
        let mut changed = false;
        for index in 0..self.session.pending.len() {
            let call = &self.session.pending[index];
            if !matches!(call.state, CallState::Executing | CallState::Uncertain) {
                continue;
            }
            let replayable = matches!(call.name.as_str(), "question" | "todo")
                || ToolExecutor::concurrent(self.tools.as_ref(), &call.name);
            if replayable {
                self.session.pending[index].state = CallState::Planned;
                changed = true;
                continue;
            }
            if !matches!(call.name.as_str(), "write" | "edit") {
                continue;
            }
            let Some(path) = ["path", "file_path", "filePath"]
                .iter()
                .find_map(|key| call.input.get(*key))
                .and_then(serde_json::Value::as_str)
                .map(std::path::PathBuf::from)
            else {
                continue;
            };
            match self.workspace.settle_edit(&path) {
                Some(true) => {
                    let call = &mut self.session.pending[index];
                    call.state = CallState::Finished;
                    call.result = Some(ToolOutput {
                        content: "The previous process stopped right after this edit; the file holds the intended content.".into(),
                        failed: false,
                        images: Vec::new(),
                        refused: false,
                    });
                    changed = true;
                }
                Some(false) => {
                    self.session.pending[index].state = CallState::Planned;
                    changed = true;
                }
                None => {}
            }
        }
        if changed {
            self.store.save(&self.session)?;
        }
        Ok(())
    }

    /// The user moved on from an unfinished task: its open calls are closed with what
    /// is known about them, so the conversation goes on instead of refusing the
    /// message. Nothing is replayed.
    fn close_unfinished(&mut self) -> Result<()> {
        let mut unknown = Vec::new();
        for call in &mut self.session.pending {
            let text = match call.state {
                CallState::Finished => continue,
                CallState::Planned => "Not run: the user sent a new message instead.".to_owned(),
                CallState::Executing | CallState::Uncertain => {
                    unknown.push(call.name.clone());
                    "Outcome unknown: the previous process stopped while this ran. Inspect its effects before relying on them; do not repeat it blindly.".to_owned()
                }
            };
            call.state = CallState::Finished;
            call.result = Some(ToolOutput {
                content: text,
                failed: true,
                images: Vec::new(),
                refused: false,
            });
        }
        self.session.finish_batch()?;
        self.session.state = State::Idle;
        self.store.save(&self.session)?;
        if !unknown.is_empty() {
            print_text(
                &format!(
                    "[note] 上一个任务在执行 {} 时中断，其效果未知；已如实告知模型。\n",
                    unknown.join(", ")
                ),
                true,
            )?;
        }
        Ok(())
    }

    pub(crate) async fn execute(&mut self, prompt: Option<String>) -> Result<()> {
        let (prompt, attachments) = match prompt {
            Some(prompt) => {
                let (text, blocks) = self.take_attachments(&prompt)?;
                (Some(text), blocks)
            }
            None => (None, Vec::new()),
        };
        crate::commands::agents::before_task(self)?;
        // Interrupted calls whose outcome can be told from the workspace (or that have
        // no effects) are settled; a new message closes what is left of the old batch.
        self.settle_interrupted()?;
        if prompt.is_some() && self.session.unfinished() {
            self.close_unfinished()?;
        }
        if let Some(prompt) = &prompt {
            for secret in crate::safety::permission::safety::secrets(prompt) {
                print_text(
                    &format!(
                        "[safety] 警告：你的消息里似乎有密钥（{secret}），它会发送给模型服务商并保存在会话中。\n"
                    ),
                    true,
                )?;
            }
        }
        if prompt.is_some() {
            if !self.session.pending.is_empty()
                || matches!(self.session.state, State::Running | State::Interrupted(_))
            {
                return Err(Error::Session(
                    "上一个任务还没结束：输入 /continue 继续它，或用 /new 开新会话".into(),
                ));
            }
            if self.session.title.is_none() {
                self.session.title = prompt.as_deref().and_then(crate::session::title_from);
            }
            let checkpoint = self.workspace.begin()?;
            self.session.checkpoints.push(checkpoint.clone());
            self.session.current_checkpoint = Some(checkpoint);
            self.store.save(&self.session)?;
        }
        let cancel = CancellationToken::new();
        self.remote.set_scope(&self.session.id, &self.settings.cwd);
        self.remote.task_started(&cancel);
        let (events, receiver) = mpsc::channel(64);
        let consumer = tokio::spawn(consume_events(receiver, cancel.clone()));
        self.progress.attach(Some(events.clone()));
        let result = drive(
            self.agent.run_with(
                &mut self.session,
                prompt,
                attachments,
                &self.store,
                events,
                cancel.clone(),
            ),
            cancel,
            self.settings.timeout,
        )
        .await;
        // The consumer ends only when every sender, including this one, is gone.
        self.progress.attach(None);
        self.remote.task_ended();
        consumer
            .await
            .map_err(|err| Error::Session(format!("输出线程异常退出：{err}")))??;
        let (added, removed) = self.workspace.take_line_changes();
        let accounting = &mut self.session.accounting;
        accounting.lines_added = accounting.lines_added.saturating_add(added);
        accounting.lines_removed = accounting.lines_removed.saturating_add(removed);
        // The task's journal folds into the snapshot: files at rest are complete.
        let flushed = self.store.flush(&self.session);
        let report = result?;
        flushed?;
        print_report(&report)?;
        if report.verification == Some(Outcome::Failed) {
            return Err(Error::Tool(
                "自动验证仍未通过，任务尚未完成；可让模型继续修复，或用 /diff 查看改动".into(),
            ));
        }
        if report.tools_failed > 0 {
            return Err(Error::Tool(format!(
                "任务已结束，但有 {} 个工具调用失败；请查看上面的 [failed] 行（/expand <n> 看完整输出），需要时让模型重试",
                report.tools_failed
            )));
        }
        Ok(())
    }

    pub(crate) async fn command(&mut self, line: &str) -> Result<bool> {
        if let Some(exit) = crate::commands::dispatch(self, line).await? {
            return Ok(exit);
        }
        if let Some(json) = line.strip_prefix("/config memory ") {
            let output = self
                .tools
                .execute(
                    "memory".into(),
                    serde_json::from_str(json)?,
                    CancellationToken::new(),
                )
                .await?;
            if output.failed {
                return Err(Error::Tool(output.content));
            }
            print_text(&format!("{}\n", output.content), false)?;
            return Ok(false);
        }
        if let Some(path) = line.strip_prefix("/config plugins install ") {
            self.install_plugin(std::path::Path::new(path.trim().trim_matches('"')))
                .await?;
            return Ok(false);
        }
        let args: Vec<_> = line.split_whitespace().collect();
        match args.as_slice() {
            ["/help"] => print_text(
                &format!(
                    "/help  /status  /new  /resume [id]  /continue  /compact  /diff  /undo [confirm]  /resolve <call-id> executed|not-run  /config [skills|mcp|hooks|plugins|memory]  /exit\n{}前台任务用 Ctrl+C 停止；命令在任务之间执行。未知的斜杠命令不会发给模型。\n",
                    crate::commands::help()
                ),
                false,
            )?,
            ["/exit"] => {
                self.store.save(&self.session)?;
                return Ok(true);
            }
            ["/status"] => {
                print_text(
                    &format!(
                        "会话 {}  状态 {:?}\n工作区 {}\n供应商 {}  协议 {:?}  模型 {}  base_url {}  profile {}  配置 {}  代理 {}\n消息 {} 条  上下文 ~{}/{} tokens（窗口 {}）  压缩 {} 次\n模型调用 {} 次  输入 {} tokens  输出 {} tokens  预估费用 {}\n待处理调用 {}\n",
                        self.session.id,
                        self.session.state,
                        self.session.cwd.display(),
                        self.settings.provider,
                        self.settings.protocol,
                        self.settings.model,
                        self.settings.base_url,
                        self.settings.profile.as_deref().unwrap_or("-"),
                        self.settings
                            .config_path
                            .as_ref()
                            .map_or("-".into(), |path| path.display().to_string()),
                        crate::model::net::describe(),
                        self.session.messages.len(),
                        crate::engine::context::tokens(&self.session.messages),
                        self.settings.context_tokens * 3 / 4,
                        self.settings.context_window,
                        self.session.compactions,
                        self.session.accounting.model_calls,
                        self.session.accounting.usage.input,
                        self.session.accounting.usage.output,
                        self.session
                            .accounting
                            .cost
                            .map_or("未知".into(), |cost| format!("${cost:.4}")),
                        if self.session.pending.is_empty() {
                            "无".to_owned()
                        } else {
                            self.session
                                .pending
                                .iter()
                                .map(|call| format!("{}:{}:{:?}", call.id, call.name, call.state))
                                .collect::<Vec<_>>()
                                .join(", ")
                        }
                    ),
                    false,
                )?;
            }
            ["/new"] => {
                self.close_session().await?;
                self.store.save(&self.session)?;
                let session = fresh(&self.settings);
                let lease = self.store.acquire(&session.id)?;
                self.store.save(&session)?;
                self.workspace.activate(None)?;
                self.session = session;
                self.lease = lease;
                print_text(&format!("[session] {}\n", self.session.id), false)?;
            }
            ["/resume"] => {
                for session in self.store.list()? {
                    if session.cwd == self.settings.cwd {
                        print_text(
                            &format!("{} {:?} {}\n", session.id, session.state, session.model),
                            false,
                        )?;
                    }
                }
            }
            ["/resume", id] => {
                if *id == self.session.id {
                    print_text("已经在使用这个会话。\n", false)?;
                    return Ok(false);
                }
                let lease = self.store.acquire(id)?;
                let mut session = self.store.load(id)?;
                adopt(&mut session, &self.settings)?;
                self.close_session().await?;
                self.store.save(&self.session)?;
                self.workspace
                    .activate(session.current_checkpoint.as_deref())?;
                self.session = session;
                self.lease = lease;
                print_text(&format!("[session] {}\n", self.session.id), false)?;
            }
            ["/continue"] => {
                // In a fresh session, /continue goes back to the last task like 继续.
                if !self.session.unfinished()
                    && let Some(result) =
                        crate::commands::sessions::intercept(self, "continue").await
                {
                    return result;
                }
                self.execute(None).await?
            }
            ["/config", rest @ ..] => self.configure(rest).await?,
            ["/compact"] => {
                let cancel = CancellationToken::new();
                let (events, receiver) = mpsc::channel(64);
                let consumer = tokio::spawn(consume_events(receiver, cancel.clone()));
                let result = drive(
                    self.agent
                        .compact(&mut self.session, &self.store, events, cancel.clone()),
                    cancel,
                    self.settings.timeout,
                )
                .await;
                consumer
                    .await
                    .map_err(|err| Error::Session(format!("输出线程异常退出：{err}")))??;
                result?;
            }
            ["/diff"] => print_text(&self.workspace.diff()?, false)?,
            ["/undo"] => print_text(&self.workspace.undo_preview()?, false)?,
            ["/undo", "confirm"] => {
                if !self.session.pending.is_empty() {
                    return Err(Error::Session(
                        "还有未完成的工具调用，暂不能回滚；先 /continue 完成任务，或用 /resolve <call-id> executed|not-run 处理（/status 列出待处理调用）".into(),
                    ));
                }
                print_text(&self.workspace.undo_preview()?, false)?;
                self.authorizer
                    .authorize(
                        Effect::Write(self.settings.cwd.clone()),
                        CancellationToken::new(),
                    )
                    .await?;
                self.session.messages.push(Message::user("Operator requested a file-tool checkpoint rollback. Checkpoint journal is authoritative; inspect files before further editing."));
                self.store.save(&self.session)?;
                self.workspace.undo()?;
                self.session.checkpoints.pop();
                self.session.current_checkpoint = self.session.checkpoints.last().cloned();
                self.session.messages.push(Message::user("File-tool rollback completed; prior file/verification evidence is stale. Shell effects were not rolled back."));
                self.session.state = State::Completed;
                self.store.save(&self.session)?;
                self.workspace
                    .activate(self.session.current_checkpoint.as_deref())?;
                print_text(
                    "已回滚文件工具的改动。之前的验证结果已失效；shell 命令的效果没有还原。\n",
                    false,
                )?;
            }
            ["/resolve", id, decision] if matches!(*decision, "executed" | "not-run") => {
                if !matches!(self.session.state, State::Interrupted(_)) {
                    return Err(Error::Session(
                        "当前没有被中断的任务，无需 /resolve；直接发消息或 /continue 即可".into(),
                    ));
                }
                let call = self
                    .session
                    .pending
                    .iter_mut()
                    .find(|call| call.id == *id)
                    .ok_or_else(|| {
                        Error::Session(format!(
                            "没有待处理调用 {id}；/status 的“待处理调用”一行列出可用的 ID"
                        ))
                    })?;
                if call.state != CallState::Uncertain {
                    return Err(Error::Session(
                        "只有结果不确定（Uncertain）的调用需要 /resolve；其余调用 /continue 会自动处理"
                            .into(),
                    ));
                }
                // A file call without a path touched no file: nothing to settle in the
                // workspace journal.
                if matches!(call.name.as_str(), "write" | "edit")
                    && let Some(path) = ["path", "file_path", "filePath"]
                        .iter()
                        .find_map(|key| call.input.get(*key))
                        .and_then(serde_json::Value::as_str)
                {
                    self.workspace
                        .resolve_edit(std::path::Path::new(path), *decision == "executed")?;
                }
                if *decision == "not-run" {
                    call.state = CallState::Planned;
                } else {
                    call.state = CallState::Finished;
                    call.result = Some(ToolOutput { content: "Operator confirmed execution, but no original output/exit code was recovered. Inspect the effects; this is NOT verification evidence. Do not blindly repeat the operation.".into(), failed: true, images: Vec::new(), refused: false });
                    self.session.task_failures += 1;
                }
                self.store.save(&self.session)?;
                print_text(
                    "已记录处理结果；/continue 继续任务，已确认执行的调用不会重放。\n",
                    false,
                )?;
            }
            _ => {
                // `/mcp`, `/plugins`, `/hooks`, `/memory`: the /config areas by their
                // own names, as other agents name them (a skill of that name wins).
                if let Some(config) = crate::commands::config_shortcut(line) {
                    return Box::pin(self.command(&config)).await;
                }
                // Built-in suggestions first, then skills and plugin commands.
                let extra = crate::commands::extensions::did_you_mean(self, line);
                return Err(match crate::commands::unknown_command(line) {
                    // One suggestion: skills and plugin commands only when no built-in fits.
                    Error::Session(text) if !text.contains("你是不是想输入") => {
                        Error::Session(format!("{text}{extra}"))
                    }
                    err => err,
                });
            }
        }
        Ok(false)
    }
}

async fn drive<T>(
    future: impl Future<Output = Result<T>>,
    cancel: CancellationToken,
    timeout: Duration,
) -> Result<T> {
    tokio::pin!(future);
    let reason = tokio::select! {
        result = &mut future => return result,
        signal = crate::app::console::interrupted() => { signal?; Error::Cancelled },
        _ = tokio::time::sleep(timeout) => Error::Limit(format!(
            "任务超过时限（{}）；/continue 接着做，或调高 --timeout / max_turn_minutes",
            crate::app::diagnostics::duration(timeout.as_millis() as u64)
        )),
    };
    cancel.cancel();
    match tokio::time::timeout(Duration::from_secs(10), &mut future).await {
        Ok(Err(Error::Cancelled)) | Ok(Ok(_)) => Err(reason),
        Ok(Err(err)) => Err(err),
        Err(_) => Err(Error::Limit(
            "停止任务后的清理超过 10 秒；状态已保存，用 /status 查看待处理调用，/continue 继续"
                .into(),
        )),
    }
}
