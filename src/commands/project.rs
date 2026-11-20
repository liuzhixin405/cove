//! Project commands: /init, /diff git, /commit, /cd, /restart, /expand, /clear,
//! /doctor, and the grouped /help with suggestions for unknown commands.

use std::{
    io::{IsTerminal, Write},
    path::Path,
    time::Duration,
};

use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    app::Application,
    app::onboarding,
    app::ui::print_text,
    domain::{Error, Result},
    safety::permission::Effect,
    session::{SessionPersistence, State},
    workflows::gitops::{self, Git, Scope},
};

pub(crate) const HELP: &str = "\
/init [apply|discard]  根据仓库起草 CLAUDE.md（或 AGENTS.md），以 diff 展示；apply 写入，从不覆盖已有文件
/diff git [--stat]  显示 git diff --cached 和 git diff；--stat 列出已暂存、未暂存和未跟踪的文件
/commit [--preview] [--no-review] [--all | --only <paths>... --] [message]  先验证、再由 AI 按项目规则审查，然后提交已暂存内容（或所选文件）；--no-review 跳过审查；从不推送
/rules [add <规则>|remove <n>|local]  项目规则（.cove/rules.md）：你纠正过的做法和反复出现的问题，每个任务都遵守、提交前按它审查；local 让它只留在本机（不提交）
/cd <path>  保存会话并在另一个项目目录中重启
/restart  保存并重启程序，继续当前会话
/expand [n] [all]  工具结果 #n 的完整输出（默认最新一条；不加 all 时最多 200 行）
/clear  清空屏幕和回滚缓冲（仅终端）
/doctor [quick|errors|trace [N]|codes|report [export <file>|analyze confirm <hash>]]  诊断与本地报告";

/// Model call bound for drafting the instruction file.
const INIT_TIMEOUT: Duration = Duration::from_secs(180);

/// Built-in commands of `app.rs`, with their `/help` group.
const BUILTIN: &[(&str, &str)] = &[
    (
        "通用",
        "/help [command]  按组列出命令；/help <命令> 只看一条",
    ),
    ("通用", "/exit  保存并退出"),
    ("会话", "/status  会话、模型、上下文和花费"),
    ("会话", "/new  开始新会话"),
    ("会话", "/resume [id]  列出本目录的会话，或切换到其中一个"),
    ("会话", "/continue  继续被中断的任务"),
    ("会话", "/rename <名字>  给当前会话改名"),
    ("会话", "/compact  总结较早的上下文（一次计费的模型调用）"),
    ("改动", "/diff  当前任务通过文件工具做的改动"),
    (
        "改动",
        "/undo [#n|files #n <文件…>|confirm]  列出回滚点、预览并恢复（每个任务一个，包含 Shell 的影响）",
    ),
    (
        "改动",
        "/resolve <call-id> executed|not-run  确认一次状态不明的工具调用",
    ),
    (
        "扩展",
        "/config [skills|mcp|hooks|plugins|memory]  扩展和记忆",
    ),
];

/// Every `/help` line, for input completion.
pub(super) fn usage_lines() -> Vec<&'static str> {
    help_lines().into_iter().map(|(_, _, line)| line).collect()
}

/// All help lines as (group, command name, line).
fn help_lines() -> Vec<(&'static str, String, &'static str)> {
    let name = |line: &str| {
        line.split_whitespace()
            .next()
            .unwrap_or_default()
            .to_owned()
    };
    let mut lines: Vec<_> = BUILTIN
        .iter()
        .map(|(group, line)| (*group, name(line), *line))
        .collect();
    for (group, help) in crate::commands::help_sections() {
        for line in help.lines().filter(|line| line.starts_with('/')) {
            lines.push((group, name(line), line));
        }
    }
    lines
}

fn help(topic: Option<&str>) -> String {
    let lines = help_lines();
    if let Some(topic) = topic {
        let topic = format!("/{}", topic.trim_start_matches('/'));
        let matching: Vec<_> = lines.iter().filter(|line| line.1 == topic).collect();
        if matching.is_empty() {
            return format!("{}\n", unknown_command(&topic));
        }
        return matching
            .iter()
            .map(|(_, _, line)| format!("{line}\n"))
            .collect();
    }
    let mut groups: Vec<&str> = Vec::new();
    for (group, _, _) in &lines {
        if !groups.contains(group) {
            groups.push(group);
        }
    }
    let mut out = String::new();
    for group in groups {
        out.push_str(&format!("{group}\n"));
        for (_, _, line) in lines.iter().filter(|line| line.0 == group) {
            out.push_str(&format!("  {line}\n"));
        }
    }
    out.push_str("技能和插件命令以 /<名称> 运行。输入 / 或 @路径 会列出补全（上下键选择，Tab 补全）。前台任务用 Ctrl+C 停止；命令在任务之间执行。未知的斜杠命令不会发给模型。\n");
    out
}

fn distance(a: &str, b: &str) -> usize {
    let b: Vec<char> = b.chars().collect();
    let mut row: Vec<usize> = (0..=b.len()).collect();
    for (i, ca) in a.chars().enumerate() {
        let mut previous = row[0];
        row[0] = i + 1;
        for (j, cb) in b.iter().enumerate() {
            let current = row[j + 1];
            row[j + 1] = (previous + usize::from(ca != *cb))
                .min(row[j] + 1)
                .min(current + 1);
            previous = current;
        }
    }
    row[b.len()]
}

/// The error for a line no command handled: usage when the command exists,
/// otherwise the closest command names.
pub(crate) fn unknown_command(line: &str) -> Error {
    let name = line.split_whitespace().next().unwrap_or(line).to_owned();
    let lines = help_lines();
    let usage: Vec<&str> = lines
        .iter()
        .filter(|entry| entry.1 == name)
        .map(|entry| entry.2)
        .collect();
    if !usage.is_empty() {
        return Error::Session(format!(
            "{name} 的参数不对；用法：\n  {}",
            usage.join("\n  ")
        ));
    }
    let mut names: Vec<(usize, &str)> = lines
        .iter()
        .map(|entry| entry.1.as_str())
        .filter(|candidate| candidate.len() > 1)
        .map(|candidate| {
            let prefix = candidate.starts_with(&name) || name.starts_with(candidate);
            (
                if prefix {
                    0
                } else {
                    distance(&name, candidate)
                },
                candidate,
            )
        })
        .filter(|(score, _)| *score <= 2)
        .collect();
    names.sort();
    names.dedup_by(|a, b| a.1 == b.1);
    let suggestion = match names
        .iter()
        .take(3)
        .map(|entry| entry.1)
        .collect::<Vec<_>>()
    {
        list if list.is_empty() => String::new(),
        list => format!("；你是不是想输入 {}？", list.join("、")),
    };
    Error::Session(format!(
        "未知命令 {name}{suggestion}（输入 /help 查看全部命令；未知命令不会发给模型）"
    ))
}

pub(crate) async fn handle(
    app: &mut Application,
    line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    match args {
        ["/help"] => print_text(&help(None), false)?,
        ["/help", topic] => print_text(&help(Some(topic)), false)?,
        ["/init"] => init(app).await?,
        ["/init", "apply"] => init_apply(app).await?,
        ["/init", "discard"] => {
            let path = onboarding::draft_path(app.store.root(), &app.settings.cwd);
            match std::fs::remove_file(&path) {
                Ok(()) => print_text("草稿已丢弃。\n", false)?,
                Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
                    print_text("没有可丢弃的草稿；/init 可以起草一份。\n", false)?
                }
                Err(err) => return Err(err.into()),
            }
        }
        ["/diff", "git"] => print_text(&gitops::diff(&app.settings.cwd, false).await?, false)?,
        ["/diff", "git", "--stat"] => {
            print_text(&gitops::diff(&app.settings.cwd, true).await?, false)?
        }
        ["/commit", rest @ ..] => commit(app, rest).await?,
        ["/rules", rest @ ..] => rules(app, line, rest).await?,
        ["/cd"] => print_text(
            &format!("{}\n", crate::paths::display(&app.settings.cwd)),
            false,
        )?,
        ["/cd", ..] => {
            let path = line.trim_start()["/cd".len()..].trim().trim_matches('"');
            return change_directory(app, path).map(Some);
        }
        ["/restart"] => return restart(app).map(Some),
        ["/expand", rest @ ..] => {
            let (id, all) = crate::app::blocks::parse(rest).ok_or_else(|| {
                Error::Session("用法：/expand [n] [all]，例如 /expand 3 all".into())
            })?;
            print_text(&crate::app::blocks::BLOCKS.expand(id, all), false)?;
        }
        ["/clear"] => clear()?,
        ["/doctor", rest @ ..] => doctor(app, rest).await?,
        _ => return Ok(None),
    }
    Ok(Some(false))
}

fn clear() -> Result<()> {
    use crossterm::{
        cursor::MoveTo,
        execute,
        terminal::{Clear, ClearType},
    };
    if let Some(console) = crate::app::console::active() {
        console.clear_screen();
        return Ok(());
    }
    let mut stdout = std::io::stdout();
    if !stdout.is_terminal() {
        return Err(Error::Session(
            "/clear 需要终端，当前输出已被重定向；无需清屏".into(),
        ));
    }
    execute!(
        stdout,
        Clear(ClearType::All),
        Clear(ClearType::Purge),
        MoveTo(0, 0)
    )?;
    Ok(())
}

fn unfinished(app: &Application) -> bool {
    !app.session.pending.is_empty()
        || matches!(app.session.state, State::Running | State::Interrupted(_))
}

async fn init(app: &mut Application) -> Result<()> {
    let root = app.settings.cwd.clone();
    let target = onboarding::target(&root);
    if let Some(existing) = &target.existing {
        print_text(
            &format!(
                "{} 已存在；/init 从不覆盖它，请直接编辑该文件。\n",
                crate::paths::display(existing)
            ),
            false,
        )?;
        return Ok(());
    }
    let digest = {
        let root = root.clone();
        tokio::task::spawn_blocking(move || onboarding::digest(&root))
            .await
            .map_err(|err| Error::Tool(format!("生成仓库摘要失败：{err}；请重试 /init")))?
    };
    print_text(
        &format!(
            "正在起草 {}（一次计费的模型调用，最多 3 分钟；按 Ctrl+C 改用模板）...\n",
            target.name
        ),
        true,
    )?;
    let (draft, source) = match draft_with_model(app, target.name, &digest).await {
        Ok(draft) => (draft, "由模型根据仓库摘要起草".to_owned()),
        Err(reason) => (
            onboarding::template(&root, target.name),
            format!("模板（{reason}）"),
        ),
    };
    let path = onboarding::draft_path(app.store.root(), &root);
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    crate::session::workspace::replace_file(
        &path,
        std::fs::read_to_string(&path).ok().as_deref(),
        Some(&draft),
    )?;
    let diff = similar::TextDiff::from_lines("", draft.as_str())
        .unified_diff()
        .header("/dev/null", target.name)
        .to_string();
    print_text(
        &format!(
            "{} 草稿，{source}：\n{diff}\n/init apply 写入；/init discard 丢弃。草稿位置：{}\n",
            target.name,
            crate::paths::display(&path)
        ),
        false,
    )?;
    Ok(())
}

/// One tool-free model call, metered like any other.
async fn draft_with_model(
    app: &mut Application,
    target: &str,
    digest: &str,
) -> std::result::Result<String, String> {
    let request = crate::model::oneshot::text_request(
        onboarding::INIT_SYSTEM,
        onboarding::prompt(target, digest),
        app.settings.max_output_tokens.min(4096),
    );
    let cancel = CancellationToken::new();
    let provider = app.background_provider();
    app.session.accounting.model_calls += 1;
    app.store
        .save(&app.session)
        .map_err(|err| err.to_string())?;
    let call = crate::model::oneshot::call(&*provider, request, INIT_TIMEOUT, cancel.clone());
    let result = tokio::select! {
        result = call => result.map_err(|err| err.to_string()),
        _ = crate::app::console::interrupted() => { cancel.cancel(); Err("已取消".into()) }
    };
    if let Ok(response) = &result {
        // Billed like a task: the model that answered, at its prices, cache-aware.
        let model = response.model.clone().unwrap_or_default();
        let prices = crate::model::routing::ModelPolicy::prices(&*app.models.router, &model);
        let cost = crate::model::models::charge(response, prices).or(Some(0.0));
        app.session.accounting.add_response(response, cost);
    }
    app.store
        .save(&app.session)
        .map_err(|err| err.to_string())?;
    let response = result?;
    let text = crate::model::oneshot::require_text(&response).map_err(|why| why.to_string())?;
    onboarding::clean(&text).ok_or_else(|| "模型回答为空".into())
}

async fn init_apply(app: &mut Application) -> Result<()> {
    let root = app.settings.cwd.clone();
    let draft_path = onboarding::draft_path(app.store.root(), &root);
    let draft = match std::fs::read_to_string(&draft_path) {
        Ok(draft) => draft,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {
            print_text("还没有草稿；请先运行 /init。\n", false)?;
            return Ok(());
        }
        Err(err) => return Err(err.into()),
    };
    let target = onboarding::target(&root);
    if let Some(existing) = target.existing {
        return Err(Error::Workspace(format!(
            "{} 现在已存在，未覆盖（草稿保留在 {}）；可对照草稿手动编辑，或用 /init discard 丢弃草稿",
            crate::paths::display(&existing),
            crate::paths::display(&draft_path)
        )));
    }
    if unfinished(app) {
        return Err(Error::Session(
            "有未完成的任务；请先 /continue 完成它或 /new 开新会话，再运行 /init apply".into(),
        ));
    }
    let path = root.join(target.name);
    app.authorizer
        .authorize(Effect::Write(path.clone()), CancellationToken::new())
        .await?;
    // A restore point first, and through a checkpoint like a task's file edits, so
    // /diff and /undo cover it.
    if let Some(snapshots) = &app.sessions.snapshots {
        snapshots.snapshot("/init apply").await?;
    }
    let checkpoint = app.workspace.begin()?;
    app.session.checkpoints.push(checkpoint.clone());
    app.session.current_checkpoint = Some(checkpoint);
    app.store.save(&app.session)?;
    app.workspace.replace(&path, None, &draft)?;
    std::fs::remove_file(&draft_path)?;
    print_text(
        &format!(
            "已写入 {}（/undo 可撤销）。/restart 后或在新会话中，系统提示会读取它。\n",
            crate::paths::display(&path)
        ),
        false,
    )?;
    Ok(())
}

async fn commit(app: &mut Application, args: &[&str]) -> Result<()> {
    let options = gitops::parse_commit(args).map_err(Error::Session)?;
    let cwd = app.settings.cwd.clone();
    let mut git = Git::open(&cwd).await?;
    if git.run(&["status", "--porcelain"]).await?.trim().is_empty() {
        print_text("没有可提交的内容。\n", false)?;
        return Ok(());
    }
    let _index = match &options.scope {
        Scope::Staged => None,
        Scope::All => Some(git.select(None).await?),
        Scope::Only(paths) => {
            let paths = git.repo_paths(&cwd, paths)?;
            Some(git.select(Some(&paths)).await?)
        }
    };
    let stat = git.run(&["diff", "--cached", "--stat"]).await?;
    if stat.trim().is_empty() {
        return Err(Error::Session(
            "没有已暂存的内容；请先 git add，或用 /commit --all [message] 提交全部改动，或 /commit --only <paths>... -- [message] 提交指定文件"
                .into(),
        ));
    }
    let files = git.staged_files().await?;
    if options.preview {
        print_text(&format!("将会提交（未创建提交）：\n{stat}"), false)?;
        return Ok(());
    }
    print_text(&format!("正在提交：\n{stat}"), false)?;
    git.run(&["diff", "--cached", "--check"]).await?;
    app.authorizer
        .authorize(
            Effect::Shell(format!(
                "git commit -m {:?} ({} files, no push) in {}",
                options.message,
                files.len(),
                crate::paths::display(&git.root)
            )),
            CancellationToken::new(),
        )
        .await?;
    let tree = git.run(&["write-tree"]).await?;
    let commands = app.settings.verify_commands.clone();
    if !commands.is_empty() {
        // Verification runs on the working tree; it must equal what is committed.
        let differs = git.unstaged_among(&files).await?;
        if !differs.is_empty() {
            return Err(Error::Session(format!(
                "所选内容与工作区不一致，验证测不到要提交的内容；请重新 git add 这些文件，或改用 /commit --all 或 --only：\n{differs}"
            )));
        }
        verify(app, &commands).await?;
        let differs = git.unstaged_among(&files).await?;
        if !differs.is_empty() {
            return Err(Error::Session(format!(
                "验证期间文件发生了变化，未提交；检查改动后重新运行 /commit：\n{differs}"
            )));
        }
    }
    if options.review
        && let Some(review) = app.commit_review.clone()
    {
        review_staged(app, &git, &review).await?;
    }
    if git.run(&["write-tree"]).await? != tree {
        return Err(Error::Session(
            "验证期间暂存区发生了变化，未提交；检查 git status 后重新运行 /commit".into(),
        ));
    }
    let output = git
        .run(&["commit", "-q", "-m", &options.message])
        .await
        .map_err(|err| {
            Error::Tool(format!(
                "提交失败（暂存区保持不变）：{err}；修正后重新运行 /commit"
            ))
        })?;
    if git.uses_temporary_index() {
        git.sync_index().await.map_err(|err| {
            Error::Tool(format!(
                "提交已创建，但同步暂存区失败；不要重试提交，用 git status 检查暂存区：{err}"
            ))
        })?;
    }
    let head = git.run(&["log", "-1", "--format=%h %s"]).await?;
    print_text(
        &format!("{output}已提交 {}（未推送）。\n", head.trim()),
        false,
    )?;
    Ok(())
}

/// `/rules`: list, add, remove, or keep the rules file out of commits.
async fn rules(app: &Application, line: &str, args: &[&str]) -> Result<()> {
    let Some(rules) = app.sessions.rules.clone() else {
        return Err(Error::Session(
            "当前工作区不是一个项目目录（比如家目录或盘符根），没有项目规则；/cd 到项目里再用"
                .into(),
        ));
    };
    let shown = crate::paths::display(rules.path());
    match args {
        [] => {
            let listed = rules.list()?;
            if listed.is_empty() {
                print_text(
                    &format!(
                        "还没有项目规则。cove 会从你的纠正和反复出现的问题里记下规则，写在 {shown}；也可以 /rules add <规则>。\n"
                    ),
                    false,
                )?;
            } else {
                let mut text = format!("项目规则（{shown}，每个任务都遵守，提交前按它审查）：\n");
                for (number, rule) in listed.iter().enumerate() {
                    text.push_str(&format!(
                        "  {}. {}\n",
                        number + 1,
                        crate::text::visible_row(rule)
                    ));
                }
                text.push_str("/rules remove <n> 删除一条；也可以直接编辑这个文件。\n");
                print_text(&text, false)?;
            }
        }
        ["add", ..] => {
            let rule = line.trim_start()["/rules".len()..]
                .trim_start()
                .strip_prefix("add")
                .unwrap_or_default()
                .trim();
            if rule.is_empty() {
                return Err(Error::Session("用法：/rules add <规则>".into()));
            }
            let text = if rules.add_by_user(rule)? {
                format!("已添加到 {shown}；本会话接下来的任务和提交前审查都会用上。\n")
            } else {
                "已有相同的规则。\n".to_owned()
            };
            print_text(&text, false)?;
        }
        ["remove", number] => {
            let number = number
                .trim_start_matches('#')
                .parse()
                .map_err(|_| Error::Session("用法：/rules remove <n>（/rules 列出编号）".into()))?;
            let removed = rules.remove(number)?;
            print_text(
                &format!("已删除：{}\n", crate::text::visible_row(&removed)),
                false,
            )?;
        }
        ["local"] => print_text(&keep_rules_local(rules.path()).await?, false)?,
        _ => {
            return Err(Error::Session(
                "用法：/rules [add <规则>|remove <n>|local]".into(),
            ));
        }
    }
    Ok(())
}

/// Lists the rules file in the repository's `info/exclude`, so it stays on this machine.
async fn keep_rules_local(path: &std::path::Path) -> Result<String> {
    let Some(root) = path.parent().and_then(std::path::Path::parent) else {
        return Err(Error::Session("规则文件的位置不对".into()));
    };
    let cancel = CancellationToken::new();
    let Some(exclude) = crate::process::git::Git::new(root)
        .args(["rev-parse", "--git-path", "info/exclude"])
        .query(&cancel)
        .await
    else {
        return Ok("这个目录不在 git 仓库里，规则文件本来就只在本机。\n".into());
    };
    let exclude = root.join(exclude.trim());
    let entry = format!("/{}", crate::memory::rules::FILE);
    let existing = std::fs::read_to_string(&exclude).unwrap_or_default();
    if existing.lines().any(|line| line.trim() == entry) {
        return Ok(format!(
            "{entry} 已经在 {} 里。\n",
            crate::paths::display(&exclude)
        ));
    }
    if let Some(dir) = exclude.parent() {
        std::fs::create_dir_all(dir)?;
    }
    let separator = if existing.is_empty() || existing.ends_with('\n') {
        ""
    } else {
        "\n"
    };
    crate::persist::write_atomic(
        &exclude,
        format!("{existing}{separator}{entry}\n").as_bytes(),
        false,
    )?;
    Ok(format!(
        "已把 {entry} 加进 {}：规则只留在本机，不会被提交。已经提交过的，需要再 git rm --cached {}。\n",
        crate::paths::display(&exclude),
        crate::memory::rules::FILE
    ))
}

/// The AI review of what is about to be committed; findings stop the commit.
async fn review_staged(
    app: &Application,
    git: &Git,
    review: &crate::safety::gates::review::CommitReview,
) -> Result<()> {
    use crate::safety::gates::review::Verdict;
    let diff = match git
        .run(&["diff", "--cached", "--no-color", "--no-ext-diff"])
        .await
    {
        Ok(diff) => diff,
        Err(err) => {
            print_text(
                &format!("[提交前审查] 读不到暂存的改动，已跳过：{err}\n"),
                true,
            )?;
            return Ok(());
        }
    };
    if !review.wanted(&diff) {
        return Ok(());
    }
    print_text("[提交前审查] 正在按项目规则审查暂存的改动…\n", true)?;
    let cancel = CancellationToken::new();
    let watcher = {
        let cancel = cancel.clone();
        tokio::spawn(async move {
            if crate::app::console::interrupted().await.is_ok() {
                cancel.cancel();
            }
        })
    };
    let verdict = review
        .review(&app.session.current_request, &diff, cancel)
        .await;
    watcher.abort();
    match verdict {
        Verdict::Passed => {
            print_text("[提交前审查] 通过\n", true)?;
            Ok(())
        }
        Verdict::Skipped => {
            print_text("[提交前审查] 没有完成，已跳过\n", true)?;
            Ok(())
        }
        Verdict::Issues(findings) => Err(Error::Session(format!(
            "提交前审查发现问题，未提交：\n{findings}\n\n修改后重新运行 /commit；确认这些问题不成立时，用 /commit --no-review 跳过审查。"
        ))),
    }
}

/// The configured verification commands, each authorized like any shell command.
async fn verify(app: &Application, commands: &[String]) -> Result<()> {
    let cancel = CancellationToken::new();
    let watcher = {
        let cancel = cancel.clone();
        tokio::spawn(async move {
            if crate::app::console::interrupted().await.is_ok() {
                cancel.cancel();
            }
        })
    };
    let result = async {
        for command in commands {
            app.authorizer
                .authorize(
                    Effect::Shell(format!("verification: {command}")),
                    cancel.clone(),
                )
                .await?;
            print_text(&format!("[verification] $ {command}\n"), true)?;
            let timeout = app
                .settings
                .verify_timeout
                .unwrap_or(Duration::from_secs(300));
            let output =
                crate::process::shell(&app.settings.cwd, command, timeout, cancel.clone()).await?;
            if output.code != Some(0) || output.timed_out {
                let text = format!("{}\n{}", output.stdout, output.stderr);
                return Err(Error::Tool(format!(
                    "提交前验证失败：{command}（{}）；修复后重新运行 /commit\n{}",
                    if output.timed_out {
                        "超时".to_owned()
                    } else {
                        format!("exit_code={:?}", output.code)
                    },
                    crate::text::clip(text.trim(), 4000, "…")
                )));
            }
            print_text(&format!("通过   {command}\n"), true)?;
        }
        Ok(())
    }
    .await;
    watcher.abort();
    result
}

fn change_directory(app: &mut Application, path: &str) -> Result<bool> {
    if app.settings.one_shot {
        return Err(Error::Session(
            "/cd 只能在交互模式中使用；单次运行请用 --cwd <目录>".into(),
        ));
    }
    let target = app.settings.cwd.join(Path::new(path));
    let target = target
        .canonicalize()
        .map_err(|err| Error::Session(format!("无法打开 {path}：{err}；请检查路径")))?;
    if !target.is_dir() {
        return Err(Error::Session(format!("{path} 不是目录；/cd 需要一个目录")));
    }
    if target == app.settings.cwd {
        print_text("已经在该目录中。\n", false)?;
        return Ok(false);
    }
    if app.store.root().starts_with(&target) {
        return Err(Error::Session(
            "数据目录位于该目录之内，不能切换过去；请选择数据目录之外的项目目录".into(),
        ));
    }
    app.store.save(&app.session)?;
    let original: Vec<_> = std::env::args_os().skip(1).collect();
    crate::app::launch::request_restart(crate::app::launch::restart_args(
        &original,
        true,
        &["--cwd".into(), target.clone().into_os_string()],
    ));
    print_text(
        &format!(
            "[cd] 正在 {} 中重启；会话 {} 已为 {} 保存（回到那里用 --resume {}）。\n",
            crate::paths::display(&target),
            app.session.id,
            crate::paths::display(&app.settings.cwd),
            app.session.id
        ),
        true,
    )?;
    Ok(true)
}

fn restart(app: &mut Application) -> Result<bool> {
    if app.settings.one_shot {
        return Err(Error::Session(
            "/restart 只能在交互模式中使用；单次运行直接重新执行命令即可".into(),
        ));
    }
    app.store.save(&app.session)?;
    let original: Vec<_> = std::env::args_os().skip(1).collect();
    crate::app::launch::request_restart(crate::app::launch::restart_args(
        &original,
        false,
        &["--resume".into(), app.session.id.clone().into()],
    ));
    print_text(
        &format!("[restart] 正在继续会话 {}\n", app.session.id),
        true,
    )?;
    Ok(true)
}

async fn doctor(app: &mut Application, args: &[&str]) -> Result<()> {
    use crate::app::diagnostics;
    let text = match args {
        [] | ["quick"] => {
            let checks = diagnostics::doctor(diagnostics::DoctorInput {
                data_dir: app.store.root(),
                config_path: app.settings.config_path.clone(),
                settings: Some(&app.settings),
                load_error: None,
                extensions: Ok(diagnostics::catalog_summary(
                    &app.catalog,
                    app.settings.trust_extensions,
                )),
                quick: !args.is_empty(),
            })
            .await;
            diagnostics::format_checks(&checks, Some(&app.diagnostics))
        }
        ["errors"] => diagnostics::errors_report(&app.diagnostics),
        ["codes"] => diagnostics::codes_report(),
        ["report"] => {
            let report = diagnostics::support_report(&app.diagnostics);
            let model = app
                .models
                .router
                .fast_model()
                .unwrap_or_else(|| app.models.router.main_model());
            let hash = report_confirmation(
                &report,
                &app.settings.provider,
                &app.settings.base_url,
                &model,
            );
            let endpoint = url::Url::parse(&app.settings.base_url)
                .ok()
                .and_then(|url| {
                    url.host_str().map(|host| {
                        format!(
                            "{}://{}{}",
                            url.scheme(),
                            host,
                            url.port().map_or(String::new(), |port| format!(":{port}"))
                        )
                    })
                })
                .unwrap_or_else(|| "未知地址".into());
            format!(
                "{report}\n确认 SHA-256: {hash}\n可选模型分析将发送以上报告至 {} / {model} / {endpoint}，会产生一次模型调用和可能的费用；核对后运行 /doctor report analyze confirm {hash}\n",
                app.settings.provider
            )
        }
        ["report", "export", file] => {
            let path = app.settings.cwd.join(file);
            app.authorizer
                .authorize(Effect::Write(path.clone()), CancellationToken::new())
                .await?;
            let mut options = std::fs::OpenOptions::new();
            options.write(true).create_new(true);
            #[cfg(unix)]
            {
                use std::os::unix::fs::OpenOptionsExt;
                options.mode(0o600);
            }
            let mut output = options.open(&path)?;
            output.write_all(diagnostics::support_report(&app.diagnostics).as_bytes())?;
            format!(
                "诊断报告已导出：{}；请检查并按需编辑后再分享。\n",
                crate::paths::display(&path)
            )
        }
        ["report", "analyze", "confirm", expected] => {
            let report = diagnostics::support_report(&app.diagnostics);
            let model = app
                .models
                .router
                .fast_model()
                .unwrap_or_else(|| app.models.router.main_model());
            if report_confirmation(
                &report,
                &app.settings.provider,
                &app.settings.base_url,
                &model,
            ) != *expected
            {
                return Err(Error::Session(
                    "报告已变化或哈希不匹配；请重新运行 /doctor report 预览".into(),
                ));
            }
            app.authorizer
                .authorize(
                    Effect::Network("向已配置的模型供应商发送诊断报告".into()),
                    CancellationToken::new(),
                )
                .await?;
            print_text(
                "正在发送已确认的报告进行模型分析（一次计费调用；建议不自动执行）。\n",
                true,
            )?;
            let (events, mut receiver) = mpsc::channel(64);
            let drain = tokio::spawn(async move { while receiver.recv().await.is_some() {} });
            let cancel = CancellationToken::new();
            let result = tokio::time::timeout(
                app.settings.timeout.min(Duration::from_secs(120)),
                app.agent.analyze_diagnostics(
                    &mut app.session,
                    &app.store,
                    report,
                    events,
                    cancel.clone(),
                ),
            )
            .await;
            if result.is_err() {
                cancel.cancel();
            }
            let _ = drain.await;
            let analysis =
                result.map_err(|_| Error::Limit("诊断分析超时；模型费用可能未知".into()))??;
            format!("模型分析（未验证，不自动执行）：\n{analysis}\n")
        }
        ["trace"] => diagnostics::trace_report(&app.diagnostics, 30),
        ["trace", n] => {
            let n = n
                .parse::<usize>()
                .ok()
                .filter(|n| (1..=10_000).contains(n))
                .ok_or_else(|| {
                    Error::Session(
                        "用法：/doctor trace [N]（N 为 1..10000），例如 /doctor trace 50".into(),
                    )
                })?;
            diagnostics::trace_report(&app.diagnostics, n)
        }
        _ => {
            return Err(Error::Session(
                "用法：/doctor [quick|errors|trace [N]|codes|report [export <file>|analyze confirm <hash>]]".into(),
            ));
        }
    };
    print_text(&text, false)?;
    Ok(())
}

fn report_confirmation(report: &str, provider: &str, endpoint: &str, model: &str) -> String {
    use sha2::{Digest, Sha256};
    let mut digest = Sha256::new();
    for field in [report, provider, endpoint, model] {
        digest.update((field.len() as u64).to_be_bytes());
        digest.update(field.as_bytes());
    }
    format!("{:x}", digest.finalize())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn report_confirmation_binds_report_and_model_destination() {
        let original = report_confirmation("report", "provider", "https://one", "model");
        assert_ne!(
            original,
            report_confirmation("other", "provider", "https://one", "model")
        );
        assert_ne!(
            original,
            report_confirmation("report", "other", "https://one", "model")
        );
        assert_ne!(
            original,
            report_confirmation("report", "provider", "https://two", "model")
        );
        assert_ne!(
            original,
            report_confirmation("report", "provider", "https://one", "other")
        );
    }

    #[test]
    fn help_is_grouped_and_answers_single_commands() {
        let text = help(None);
        assert!(text.starts_with("通用\n  /help"));
        assert!(text.contains("项目\n  /init"));
        assert!(help(Some("commit")).starts_with("/commit [--preview]"));
        assert!(help(Some("/diff")).contains("/diff git"));
        assert!(help(Some("nope")).contains("未知命令 /nope"));
    }

    #[test]
    fn unknown_commands_get_suggestions_or_usage() {
        let text = unknown_command("/comit now").to_string();
        assert!(text.contains("你是不是想输入 /commit"), "{text}");
        let text = unknown_command("/undo now").to_string();
        assert!(text.contains("用法：\n  /undo [#n"), "{text}");
        let text = unknown_command("/zzzzzz").to_string();
        assert!(text.contains("未知命令 /zzzzzz") && !text.contains("你是不是"));
        assert_eq!(distance("kitten", "sitting"), 3);
        let text = unknown_command("/mpc").to_string();
        assert!(text.contains("你是不是想输入 /mcp"), "{text}");
        let shortcut = crate::commands::shortcut;
        assert_eq!(
            shortcut("/mcp login atlassian").as_deref(),
            Some("/config mcp login atlassian")
        );
        assert_eq!(shortcut("/usage").as_deref(), Some("/status usage"));
        assert_eq!(shortcut("/cost 30d").as_deref(), Some("/status cost 30d"));
        assert_eq!(shortcut("/mcpx"), None);
        assert_eq!(shortcut("/usagex"), None);
    }

    #[test]
    fn the_help_lines_drive_completion() {
        let completer = crate::app::completion::Completer::new(
            &usage_lines(),
            Vec::new(),
            Vec::new(),
            std::env::temp_dir(),
        );
        let texts = |line: &str| -> Vec<String> {
            completer
                .suggest(line)
                .into_iter()
                .map(|found| found.text)
                .collect()
        };
        assert_eq!(texts("/sta"), ["/status"]);
        assert!(!texts("/").iter().any(|name| name.contains('<')));
        let status = texts("/status ");
        for sub in ["agents", "cost", "limits", "acceptance"] {
            assert!(status.iter().any(|found| found == sub), "{status:?}");
        }
        let config = texts("/config ");
        for sub in ["skills", "mcp", "hooks", "plugins", "memory", "trust"] {
            assert!(config.iter().any(|found| found == sub), "{config:?}");
        }
        assert!(texts("/status agents ").contains(&"merge".to_owned()));
        assert!(texts("/mode ").contains(&"plan".to_owned()));
        assert!(texts("/help sta").contains(&"status".to_owned()));
    }
}
