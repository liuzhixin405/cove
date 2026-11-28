//! Race, changes, remote supervision and browser verification.
//!
//! Three top-level commands:
//! - `/race <request>` produces two candidate solutions;
//! - `/changes` owns every reviewable patch (race candidates, the current task)
//!   and browser verification;
//! - `/remote` owns the supervision server.

use std::{
    future::Future,
    path::{Path, PathBuf},
    time::Duration,
};

use tokio_util::sync::CancellationToken;

use crate::{
    app::Application,
    app::ui::print_text,
    config::Settings,
    domain::{Error, Message, Result},
    safety::impact,
    safety::permission::Effect,
    session::SessionPersistence,
    workflows::browser_verify,
    workflows::changes::{self, Changeset, Decision, Kind},
    workflows::race,
};

pub(crate) const HELP: &str = "/race [<需求>]  一个需求两个方案：主模型和快速模型各在独立的 worktree 里实现，用同一组命令验证，选一个应用；不带参数查看最近一次\n\
/changes [list|show <id>|diff <id>|export <id> <out>|apply <id> [confirm]|review <id> accepted|rejected|impact [turn|<git 区间>]|verify-browser <workflow.json> [--allow-local]|verify-browser results]  可审查的补丁：竞速方案和当前任务；impact 列出改动符号在其他文件的使用和有没有测试引用\n\
/remote start [--bind 127.0.0.1:0] [--token-file <path>] [--allow-lan --tls-cert <pem> --tls-key <pem>] | status | stop  远程监督服务";

/// `/changes impact` prints at most this many lines of the list (30 with its title
/// and timing).
const ASKED_LINES: usize = 28;

/// `browser_path` from the effective config.
pub(crate) fn browser_path(settings: &Settings) -> Option<PathBuf> {
    settings
        .file
        .get("browser_path")
        .and_then(serde_json::Value::as_str)
        .filter(|path| !path.trim().is_empty())
        .map(PathBuf::from)
}

/// Runs `future` in the foreground: Ctrl+C or a remote cancel stops it.
async fn foreground<T>(
    app: &Application,
    cancel: CancellationToken,
    future: impl Future<Output = Result<T>>,
) -> Result<T> {
    app.remote.set_scope(&app.session.id, &app.settings.cwd);
    app.remote.task_started(&cancel);
    tokio::pin!(future);
    let result = tokio::select! {
        result = &mut future => result,
        signal = crate::app::console::interrupted() => {
            signal?;
            cancel.cancel();
            match tokio::time::timeout(Duration::from_secs(30), &mut future).await {
                Ok(result) => result,
                Err(_) => Err(Error::Limit("清理超过 30 秒；请用 git worktree list 检查残留的工作树".into())),
            }
        }
    };
    app.remote.task_ended();
    result
}

fn turn_changeset(app: &Application) -> Result<Option<Changeset>> {
    if app.workspace.fingerprint()?.is_none() {
        return Ok(None);
    }
    let diff = app.workspace.diff()?;
    Ok(Some(Changeset {
        id: "turn".into(),
        kind: Kind::Turn,
        title: "当前任务通过文件工具做的改动".into(),
        baseline: None,
        patch_sha256: changes::sha256_hex(diff.as_bytes()),
        patch_bytes: diff.len(),
        checks: Vec::new(),
        review: Decision::Pending,
        not_applicable: Some("已在工作区中；只读，撤销请用 /undo".into()),
        created: app.session.updated,
        patch_path: None,
    }))
}

fn all_changesets(app: &Application) -> Result<Vec<Changeset>> {
    let data = app.store.root();
    let mut sets = Vec::new();
    sets.extend(turn_changeset(app)?);
    sets.extend(race::changesets(data, &app.settings.cwd)?);
    Ok(sets)
}

fn find(app: &Application, id: &str) -> Result<Changeset> {
    all_changesets(app)?
        .into_iter()
        .find(|set| set.id == id)
        .ok_or_else(|| Error::Tool(format!("未知的改动集 {id}；用 /changes 查看可用 ID")))
}

fn record(app: &Application, set: &Changeset, decision: Decision) -> Result<()> {
    match set.kind {
        Kind::Turn => Err(Error::Tool(
            "当前任务的改动集是只读的；撤销请用 /undo".into(),
        )),
        Kind::Race => {
            let mut parts = set.id.split(':').skip(1);
            let (Some(run), Some(candidate)) = (parts.next(), parts.next()) else {
                return Err(Error::Tool(
                    "竞赛改动集 ID 无效；用 /changes 查看可用 ID".into(),
                ));
            };
            race::record(app.store.root(), run, candidate, decision)
        }
    }
}

async fn changes_command(app: &mut Application, args: &[&str]) -> Result<()> {
    let data = app.store.root().to_path_buf();
    match args {
        [] | ["list"] => {
            let sets = all_changesets(app)?;
            if sets.is_empty() {
                print_text(
                    "没有改动集。可用 /race <需求> 或执行一个任务来产生。\n",
                    false,
                )?;
            }
            for set in sets {
                print_text(&format!("{}\n", set.summary()), false)?;
            }
        }
        ["show", id] => {
            let set = find(app, id)?;
            print_text(&format!("{}\n", serde_json::to_string_pretty(&set)?), false)?;
        }
        ["diff", id] => {
            let set = find(app, id)?;
            let text = if set.kind == Kind::Turn {
                app.workspace.diff()?
            } else {
                String::from_utf8_lossy(&set.load_patch()?).into_owned()
            };
            print_text(&format!("{}\n", changes::tail(&text, 256 * 1024)), false)?;
        }
        ["export", id, out] => {
            let set = find(app, id)?;
            let bytes = if set.kind == Kind::Turn {
                app.workspace.diff()?.into_bytes()
            } else {
                set.load_patch()?
            };
            let target =
                changes::new_outside_file(Path::new(out.trim_matches('"')), &app.settings.cwd)?;
            changes::write_new(&target, &bytes)?;
            print_text(
                &format!(
                    "已将 {} 字节写入 {}\n",
                    bytes.len(),
                    crate::paths::display(&target)
                ),
                false,
            )?;
        }
        ["apply", id] | ["apply", id, "confirm"] => {
            let confirm = args.len() == 3;
            let set = find(app, id)?;
            if set.kind == Kind::Turn {
                return Err(Error::Tool(
                    "当前任务的改动集已在工作区中；撤销请用 /undo".into(),
                ));
            }
            if !app.session.pending.is_empty() {
                return Err(Error::Session(
                    "有待处理的工具调用；请先 /continue 完成，或用 /resolve <call-id> executed|not-run 确认，再应用".into(),
                ));
            }
            let cancel = CancellationToken::new();
            if !confirm {
                let patch = set.preflight(&app.settings.cwd, &cancel).await?;
                print_text(
                    &format!(
                        "{} 通过了应用检查（基线、干净工作区、最近检查、哈希；{} 字节）。\n运行 /changes apply {id} confirm 写入 {}。\n",
                        set.id,
                        patch.len(),
                        crate::paths::plain(&app.settings.cwd).to_string_lossy()
                    ),
                    false,
                )?;
                return Ok(());
            }
            app.authorizer
                .authorize(Effect::Write(app.settings.cwd.clone()), cancel.clone())
                .await?;
            let _lock =
                changes::project_lock(&data, &app.settings.cwd, Duration::from_secs(10)).await?;
            let patch = set.preflight(&app.settings.cwd, &cancel).await?;
            let baseline = set.baseline.clone().ok_or_else(|| {
                Error::Tool(
                    "改动集没有基线，无法应用；可用 /changes export <id> <out> 导出补丁后手动应用"
                        .into(),
                )
            })?;
            baseline.apply(&patch, &set.patch_sha256, &cancel).await?;
            record(app, &set, Decision::Applied)?;
            app.session.messages.push(Message::notice(format!(
                "Operator applied changeset {} ({} bytes) to the workspace outside the file tools; earlier file and verification evidence may be stale.",
                set.id,
                patch.len()
            )));
            app.store.save(&app.session)?;
            print_text(
                &format!("已应用 {}。没有创建提交；用 git diff 审查。\n", set.id),
                false,
            )?;
        }
        ["review", id, decision] => {
            let set = find(app, id)?;
            record(app, &set, Decision::parse(decision)?)?;
            print_text(
                &format!("已为 {id} 记录 {decision}；没有应用任何改动。\n"),
                false,
            )?;
        }
        ["impact"] | ["impact", _] => {
            let target = args.get(1).copied().unwrap_or("turn");
            let cancel = CancellationToken::new();
            let root = app.workspace.root().to_path_buf();
            let (changes, shell, what) = if target == "turn" {
                (
                    crate::safety::gates::impact::task_changes(&app.workspace),
                    app.workspace.shell_effects(),
                    "当前任务通过文件工具做的改动".to_owned(),
                )
            } else {
                let changes = foreground(
                    app,
                    cancel.clone(),
                    impact::git_changes(&root, target, &cancel),
                )
                .await?;
                (
                    changes,
                    false,
                    format!("{target}（调用方按当前工作区查找）"),
                )
            };
            if changes.is_empty() {
                print_text(&format!("{what}：没有改动的文件。\n"), false)?;
                return Ok(());
            }
            let started = std::time::Instant::now();
            let mut found = foreground(
                app,
                cancel.clone(),
                impact::run(
                    root,
                    changes,
                    impact::Limits::ASKED,
                    impact::ASKED_BUDGET,
                    &cancel,
                ),
            )
            .await?;
            found.skipped.shell = shell;
            let text = if found.symbols.is_empty() {
                "[影响] 没有找到改动的符号（新增的符号、测试代码、不支持的语言不算）".to_owned()
            } else {
                found.render(ASKED_LINES)
            };
            print_text(
                &format!(
                    "{what}\n{text}\n用时 {:.1} 秒\n",
                    started.elapsed().as_secs_f64()
                ),
                false,
            )?;
        }
        ["verify-browser", "results"] => {
            let reports = browser_verify::list(&data, &app.settings.cwd);
            if reports.is_empty() {
                print_text(
                    "本项目还没有浏览器验证记录；用 /changes verify-browser <workflow.json> 运行一次。\n",
                    false,
                )?;
            }
            for report in reports {
                print_text(&browser_verify::render(&report), false)?;
            }
        }
        ["verify-browser", rest @ ..] if !rest.is_empty() => {
            let allow_local = rest.contains(&"--allow-local");
            let path: Vec<&str> = rest
                .iter()
                .copied()
                .filter(|arg| *arg != "--allow-local")
                .collect();
            let [path] = path.as_slice() else {
                return Err(Error::Tool(
                    "用法：/changes verify-browser <workflow.json> [--allow-local]".into(),
                ));
            };
            let cancel = CancellationToken::new();
            if allow_local {
                app.authorizer
                    .authorize(
                        Effect::Network("browser verification with loopback access".into()),
                        cancel.clone(),
                    )
                    .await?;
            }
            let cwd = app.settings.cwd.clone();
            let browser = browser_path(&app.settings);
            let report = foreground(
                app,
                cancel.clone(),
                browser_verify::run(
                    &data,
                    &cwd,
                    Path::new(path.trim_matches('"')),
                    allow_local,
                    browser.as_deref(),
                    &cancel,
                ),
            )
            .await?;
            print_text(&browser_verify::render(&report), false)?;
            print_text(
                &format!(
                    "报告：{}\n",
                    crate::paths::display(
                        &browser_verify::directory(&data)
                            .join(&report.id)
                            .join("report.json")
                    )
                ),
                false,
            )?;
        }
        _ => {
            return Err(Error::Tool(format!(
                "/changes 命令无效；用法：\n{}",
                HELP.lines().next().unwrap_or("")
            )));
        }
    }
    Ok(())
}

/// `/race <request>`: two solutions in their own worktrees, verified alike.
async fn race_command(app: &mut Application, line: &str) -> Result<()> {
    let request = line
        .trim()
        .strip_prefix("/race")
        .map(str::trim)
        .unwrap_or_default()
        .to_owned();
    let data = app.store.root().to_path_buf();
    let cwd = app.settings.cwd.clone();
    if request.is_empty() {
        return match race::latest(&data, &cwd)? {
            Some(report) => print_text(&race::render(&report), false).map_err(Into::into),
            None => {
                print_text("本项目还没有竞速记录。用法：/race <需求>\n", false).map_err(Into::into)
            }
        };
    }
    if !app.session.pending.is_empty() {
        return Err(Error::Session(
            "有待处理的工具调用；请先 /continue 完成，或用 /resolve <call-id> executed|not-run 确认".into(),
        ));
    }
    let lanes = race::lanes(
        &request,
        &app.settings.model,
        app.settings.model_fast.as_deref(),
    );
    let verify = race::verification(&cwd, &app.settings.verify_commands);
    print_text(
        &format!(
            "正在两个独立 worktree 里各做一个方案：A {} {}，B {} {}；{}。Ctrl+C 取消。\n",
            lanes[0].label,
            lanes[0].model,
            lanes[1].label,
            lanes[1].model,
            if verify.is_empty() {
                "推断不出验证命令，方案将标为未验证".to_owned()
            } else {
                format!(
                    "完成后用 {} 验证",
                    verify
                        .iter()
                        .map(|argv| argv.join(" "))
                        .collect::<Vec<_>>()
                        .join("、")
                )
            }
        ),
        true,
    )?;
    let hub = app.agents.clone();
    let access = race::Access {
        mode: app.permissions.mode(),
        input: app.input.clone(),
    };
    let cancel = CancellationToken::new();
    let result = foreground(
        app,
        cancel.clone(),
        race::run(
            &hub,
            &data,
            &cwd,
            &request,
            &lanes,
            verify,
            race::TIMEOUT,
            &access,
            &cancel,
        ),
    )
    .await;
    // The lanes' spend reaches the session whether or not the race finished.
    crate::commands::agents::absorb(app)?;
    let report = result?;
    app.session.messages.push(Message::notice(format!(
        "Operator ran a race ({}) for: {}. Nothing was applied to the workspace.",
        report.id,
        crate::text::short(&request, 200)
    )));
    app.store.save(&app.session)?;
    print_text(&race::render(&report), false)?;
    Ok(())
}

async fn remote_command(app: &mut Application, args: &[&str]) -> Result<()> {
    match args {
        ["start", rest @ ..] => {
            if app.remote_server.is_some() {
                return Err(Error::Tool(
                    "远程监督已在运行；/remote status 查看地址，或先 /remote stop".into(),
                ));
            }
            let options = crate::workflows::remote::Options::parse(rest)?;
            app.remote.set_scope(&app.session.id, &app.settings.cwd);
            let server = crate::workflows::remote::Server::start(
                options,
                app.store.root().join("remote").join("token"),
                app.remote.clone(),
            )
            .await?;
            print_text(
                &format!(
                    "远程监督地址：{}\n接口：GET /v1/status、POST /v1/actions、GET /v1/actions/<id>\nBearer 令牌在 {}（仅所有者可读；不在此显示）。\n",
                    server.url,
                    crate::paths::plain(&server.token_file).to_string_lossy()
                ),
                false,
            )?;
            app.remote_server = Some(server);
        }
        ["status"] => {
            let mut status = app.remote.snapshot();
            status["listening"] =
                serde_json::json!(app.remote_server.as_ref().map(|s| s.url.clone()));
            print_text(
                &format!("{}\n", serde_json::to_string_pretty(&status)?),
                false,
            )?;
        }
        ["stop"] => match app.remote_server.take() {
            Some(server) => {
                server.stop().await;
                print_text("远程监督已停止；令牌文件已删除。\n", false)?;
            }
            None => print_text("远程监督未在运行；/remote start 可启动。\n", false)?,
        },
        _ => {
            return Err(Error::Tool(
                "用法：/remote start [--bind 127.0.0.1:0] [--token-file <path>] [--allow-lan --tls-cert <pem> --tls-key <pem>] | status | stop".into(),
            ));
        }
    }
    Ok(())
}

pub(crate) async fn handle(
    app: &mut Application,
    line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    match args.first().copied() {
        Some("/changes") => changes_command(app, &args[1..]).await?,
        Some("/race") => race_command(app, line).await?,
        Some("/remote") => remote_command(app, &args[1..]).await?,
        _ => return Ok(None),
    }
    Ok(Some(false))
}
