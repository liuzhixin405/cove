//! Permission modes, plan/review workflow and acceptance (`/mode`, `/status acceptance`).

use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::{
    app::Application,
    app::ui::print_text,
    domain::{Error, Result},
    safety::gates::{acceptance, plan::WorkflowMode},
    safety::permission::{Effect, Mode},
};

pub(crate) const HELP: &str = "/mode [default|plan|auto|bypass|review|once|direct] [save]  权限模式或工作流，立即生效；加 save 写入配置\n/status acceptance  上一个任务的检查、验证和 git 状态";

pub(crate) async fn handle(
    app: &mut Application,
    _line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    match args {
        ["/mode"] => print_text(&status(app), false)?,
        ["/mode", value, rest @ ..] => {
            let save = match rest {
                [] => false,
                ["save"] => true,
                _ => {
                    return Err(Error::Session(
                        "用法：/mode <模式> [save]，例如 /mode auto save".into(),
                    ));
                }
            };
            let (key, stored) = if let Some(mode) = Mode::parse(value) {
                app.permissions.set_mode(mode);
                let warning = if mode == Mode::Bypass {
                    "\n警告：bypass 会放行所有写入、Shell 命令和网络请求，只拦截 deny 规则和灾难性命令。改回请用 /mode default。"
                } else {
                    ""
                };
                print_text(&format!("权限模式：{}{warning}\n", mode.name()), false)?;
                ("permission_mode", mode.name())
            } else if let Some(workflow) = WorkflowMode::parse(value) {
                if workflow != WorkflowMode::Direct && !app.permissions.interactive() {
                    return Err(Error::Session(
                        "review/once 工作流需要交互式终端来批准计划；请在交互式终端中运行，或改用 /mode direct"
                            .into(),
                    ));
                }
                app.workflow.set_mode(workflow);
                print_text(
                    &format!(
                        "工作流：{}{}\n",
                        workflow.name(),
                        match workflow {
                            WorkflowMode::Review => "（每个任务先进入 plan 模式，计划需要你批准）",
                            WorkflowMode::Once => "（下一个任务先进入 plan 模式）",
                            WorkflowMode::Direct => "",
                        }
                    ),
                    false,
                )?;
                ("workflow", workflow.name())
            } else {
                return Err(Error::Session(
                    "未知模式；可用 default、plan、auto、bypass、review、once 或 direct，例如 /mode auto".into(),
                ));
            };
            if save {
                let path = save_config(app, key, stored).await?;
                print_text(
                    &format!("已将 {key}={stored} 保存到 {}\n", path.display()),
                    false,
                )?;
            }
        }
        ["/status", "acceptance"] => {
            let text = match acceptance::load(&app.settings.data_dir, &app.session.id) {
                Some(report) => acceptance::summary(&report),
                None => "本会话还没有验收报告；每个任务结束后会自动保存一份。\n".into(),
            };
            print_text(&text, false)?;
        }
        _ => return Ok(None),
    }
    Ok(Some(false))
}

fn status(app: &Application) -> String {
    let permissions = &app.permissions;
    let origin = match permissions.plan_origin() {
        Some(origin) if permissions.mode() == Mode::Plan => {
            format!("（由 {} 进入）", format!("{origin:?}").to_lowercase())
        }
        _ => String::new(),
    };
    let rules = permissions.rules();
    let mut text = format!(
        "权限模式：{}{origin}\n工作流：{}\n策略：{} 条规则，位于 {}\n",
        permissions.mode().name(),
        app.workflow.mode().name(),
        rules.len(),
        permissions
            .policy_path()
            .map_or("-".into(), |path| path.display().to_string())
    );
    for rule in rules.iter().take(20) {
        let target = [&rule.command_prefix, &rule.command_group]
            .into_iter()
            .find(|s| !s.is_empty())
            .cloned()
            .unwrap_or_default();
        text.push_str(&format!(
            "  {:?} {} {}{}\n",
            rule.action(),
            if rule.tool_pattern.is_empty() {
                "shell"
            } else {
                &rule.tool_pattern
            },
            target,
            if rule.scope.is_empty() {
                String::new()
            } else {
                format!(" [范围 {}]", rule.scope)
            }
        ));
    }
    if let Some(plan) = app.workflow.last_plan() {
        text.push_str(&format!(
            "上一个计划：{}（{}）\n",
            plan.summary.lines().next().unwrap_or_default(),
            plan.decision
        ));
    }
    text.push_str("模式：default（逐项询问）、plan（只读）、auto（构建/测试、项目内编辑、联网）、bypass（除 deny 规则外全部放行）\n工作流：direct、review（每个任务先做计划）、once（下一个任务先做计划）\n切换：/mode <模式> [save]\n");
    text
}

/// Writes `key` into the config file the session loaded (or `<data_dir>/config.json`).
async fn save_config(app: &Application, key: &str, value: &str) -> Result<std::path::PathBuf> {
    let path = app
        .settings
        .config_path
        .clone()
        .unwrap_or_else(|| app.settings.data_dir.join("config.json"));
    app.authorizer
        .authorize(Effect::Write(path.clone()), CancellationToken::new())
        .await?;
    let mut config: Value = if path.is_file() {
        let data = std::fs::read(&path)?;
        serde_json::from_slice(data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data))?
    } else {
        Value::Object(Default::default())
    };
    let object = config
        .as_object_mut()
        .ok_or_else(|| Error::Session("配置文件不是 JSON 对象；请修正后重试".into()))?;
    // A profile that sets the key would override the root value.
    let target = match &app.settings.profile {
        Some(profile)
            if object
                .get("profiles")
                .and_then(|p| p.get(profile))
                .and_then(|p| p.get(key))
                .is_some() =>
        {
            object
                .get_mut("profiles")
                .and_then(|p| p.get_mut(profile))
                .and_then(Value::as_object_mut)
        }
        _ => Some(object),
    };
    if let Some(target) = target {
        if key == "permission_mode" {
            target.remove("mode");
        }
        target.insert(key.into(), Value::String(value.into()));
    }
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let parent = path.parent().unwrap_or(std::path::Path::new("."));
    let mut file = tempfile::NamedTempFile::new_in(parent)?;
    std::io::Write::write_all(&mut file, &serde_json::to_vec_pretty(&config)?)?;
    crate::session::replace_with(file, &path)?;
    Ok(path)
}
