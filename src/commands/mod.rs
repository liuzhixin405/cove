//! Slash commands by feature area. Each area owns one module with
//! `HELP` (one or more help lines) and `handle`, which returns `Ok(None)` when
//! the line is not one of its commands, `Ok(Some(exit))` when it handled it.
//! Areas are tried before the built-in commands in `app.rs`, in this order.

pub(crate) mod agents;
pub(crate) mod extensions;
pub(crate) mod model;
mod project;
mod safety;
pub(crate) mod sessions;
pub(crate) mod workflows;

use crate::{app::Application, domain::Result};

pub(crate) use project::unknown_command;

pub(crate) async fn dispatch(app: &mut Application, line: &str) -> Result<Option<bool>> {
    let args: Vec<&str> = line.split_whitespace().collect();
    macro_rules! try_area {
        ($area:ident) => {
            if let Some(exit) = $area::handle(app, line, &args).await? {
                return Ok(Some(exit));
            }
        };
    }
    try_area!(model);
    try_area!(safety);
    try_area!(sessions);
    try_area!(extensions);
    try_area!(agents);
    try_area!(project);
    try_area!(workflows);
    Ok(None)
}

/// `/config` areas that also answer to their own name.
const CONFIG_SHORTCUTS: [&str; 4] = ["mcp", "plugins", "hooks", "memory"];

/// `/mcp list` as `/config mcp list`, for the areas in `CONFIG_SHORTCUTS`.
pub(crate) fn config_shortcut(line: &str) -> Option<String> {
    let rest = line.trim().strip_prefix('/')?;
    let name = rest.split_whitespace().next()?;
    CONFIG_SHORTCUTS
        .contains(&name)
        .then(|| format!("/config {rest}"))
}

/// Help lines of each area with its `/help` group title.
pub(crate) fn help_sections() -> [(&'static str, &'static str); 7] {
    [
        ("模型", model::HELP),
        ("安全", safety::HELP),
        ("会话", sessions::HELP),
        ("扩展", extensions::HELP),
        ("子代理", agents::HELP),
        ("项目", project::HELP),
        ("工作流", workflows::HELP),
    ]
}

/// Help lines contributed by the areas, appended to the built-in `/help`.
pub(crate) fn help() -> String {
    [
        model::HELP,
        safety::HELP,
        sessions::HELP,
        extensions::HELP,
        agents::HELP,
        project::HELP,
        workflows::HELP,
    ]
    .iter()
    .filter(|help| !help.is_empty())
    .map(|help| format!("{help}\n"))
    .collect()
}

/// REPL completion over the current commands, skills and plugin commands.
pub(crate) fn completer(app: &Application) -> crate::app::completion::Completer {
    // Skill directories are looked at again, so a new SKILL.md completes at once.
    if let Err(err) = app.tools.skills.refresh() {
        let _ = crate::app::ui::print_text(
            &format!(
                "[warning] 技能未重新加载：{err}
"
            ),
            true,
        );
    }
    let lines = project::usage_lines();
    let plugins = app
        .plugins
        .commands(&app.registry)
        .into_iter()
        .map(|(name, command)| (name, command.description))
        .collect();
    crate::app::completion::Completer::new(
        &lines,
        app.tools.skills.index().unwrap_or_default(),
        plugins,
        app.settings.cwd.clone(),
    )
}
