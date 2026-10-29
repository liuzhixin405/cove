//! Skills, plugin commands, hooks, MCP and project trust (/<skill>, /config trust).

use std::{path::Path, sync::Arc};

use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    app::Application,
    app::ui::print_text,
    config::Settings,
    domain::{Error, Provider, Result},
    engine::agent::Agent,
    extensions::mcp::McpArgs,
    extensions::plugins::{self, Plugins},
    extensions::skills::{self, SkillSources, Skills},
    extensions::{
        Catalog, ExtensionConfig, HookConfig, Registry, Sourced, identifier, top_level_servers,
    },
    safety::permission::Effect,
};

pub(crate) const HELP: &str = "/skills  列出技能（名称、来源、说明）   /skill <name>  查看某个技能的指令\n/<skill> [args]  /<plugin-command> [args]  以任务方式运行技能或插件命令\n/config trust  信任此项目（目录及已加载的 .cove.json；重启后生效）\n/config skills [list|show <name>|install <name|url>|create <name>|marketplace [query]]  管理技能：查看、安装、创建、浏览市场\n/config plugins [list|install <name|git-url|manifest-path>|search [query]|update [name]|enable|disable|uninstall <name>]  管理插件\n/config mcp [list|connect <name>|disconnect <name|all>|login <name>|logout <name>|read <server> <uri>]  管理 MCP 服务器   /config hooks [resolve <run-id>]  查看钩子\n/mcp [list|connect <name>|disconnect <name|all>|login <name>|logout <name>|read <server> <uri>]  同 /config mcp\n/plugins [list|install <name|git-url|manifest-path>|search [query]|update [name]|enable|disable|uninstall <name>]  同 /config plugins\n/hooks [resolve <run-id>]  同 /config hooks\n/memory [list|add [global] <text>|remove <id>|search <query>|stats|source <id>|dream [status|run]]  同 /config memory";

/// Top-level commands of the final command set; they win over skills of the same name.
const BUILTIN: &[&str] = &[
    "help",
    "usage",
    "skill",
    "skills",
    "status",
    "model",
    "mode",
    "config",
    "new",
    "resume",
    "continue",
    "compact",
    "diff",
    "undo",
    "resolve",
    "attach",
    "init",
    "commit",
    "cd",
    "expand",
    "clear",
    "restart",
    "doctor",
    "exit",
    "race",
    "automations",
    "changes",
    "remote",
    "browser",
    "browser-verify",
];

fn warn(text: &str) {
    let _ = print_text(&format!("[extensions] {text}\n"), true);
}

pub(crate) struct Assembled {
    pub config: ExtensionConfig,
    pub hooks: Vec<Sourced<HookConfig>>,
}

/// Config extensions plus top-level `mcp_servers` and `<data_dir>/hooks.json`.
pub(crate) fn assemble(settings: &Settings, data_dir: &Path) -> Result<Assembled> {
    let mut config = settings.extensions.clone();
    for (name, server) in top_level_servers(&settings.file)? {
        if config.mcp.insert(name.clone(), server).is_some() {
            return Err(Error::Tool(format!(
                "MCP 服务器 {name} 同时配置在 extensions.mcp 和 mcp_servers 中；请删除其中一处"
            )));
        }
    }
    let (mut hooks, warnings) = crate::extensions::hooks::load_hooks_json(data_dir)?;
    for warning in warnings {
        warn(&warning);
    }
    if !hooks.is_empty() && !settings.trust_extensions {
        warn(&format!(
            "{} 个 hooks.json 钩子未加载：钩子需要 --trust-extensions 或 /config trust",
            hooks.len()
        ));
        hooks.clear();
    }
    Ok(Assembled { config, hooks })
}

pub(crate) fn load(
    settings: &Settings,
    data_dir: &Path,
    mut catalog: Catalog,
    hooks: Vec<Sourced<HookConfig>>,
    registry: Arc<Registry>,
) -> Result<(Catalog, Arc<Skills>, Plugins)> {
    catalog.hooks.extend(hooks);
    let plugins = Plugins::scan(data_dir);
    for warning in &plugins.warnings {
        warn(warning);
    }
    let disabled = settings
        .file
        .get("disabled_skills")
        .and_then(Value::as_array)
        .map(|names| {
            names
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default();
    let skills = Skills::load(
        SkillSources {
            data_dir: data_dir.into(),
            cwd: settings.cwd.clone(),
            home: skills::home_dir(),
            configured: catalog.skills.clone(),
            plugins: plugins
                .list
                .iter()
                .map(|p| (p.name.clone(), p.dir.clone()))
                .collect(),
            disabled,
            builtin: true,
        },
        registry,
    )?;
    for warning in &skills.warnings {
        warn(warning);
    }
    Ok((catalog, Arc::new(skills), plugins))
}

/// File-type skill hints, and learned skills in interactive sessions (`skill_review`).
pub(crate) fn turn_hooks(
    agent: Agent,
    settings: &Settings,
    skills: &Arc<Skills>,
    background: Arc<dyn Provider>,
) -> Agent {
    let agent = agent.with_turn_hook(Arc::new(skills::SkillNotes::new(
        skills.clone(),
        settings.cwd.clone(),
    )));
    if settings.one_shot || !crate::extensions::skill_review::SkillReview::enabled(&settings.file) {
        return agent;
    }
    agent.with_turn_hook(Arc::new(crate::extensions::skill_review::SkillReview::new(
        background,
        skills.clone(),
        settings.max_output_tokens,
    )))
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

/// " Did you mean /x?" for a mistyped command, or "".
pub(crate) fn did_you_mean(app: &Application, line: &str) -> String {
    let Some(typed) = line
        .split_whitespace()
        .next()
        .and_then(|w| w.strip_prefix('/'))
    else {
        return String::new();
    };
    let mut candidates: Vec<String> = BUILTIN.iter().map(|s| (*s).to_owned()).collect();
    candidates.extend(app.tools.skills.names());
    candidates.extend(app.plugins.commands(&app.registry).into_keys());
    if candidates.iter().any(|c| c == typed) {
        return String::new();
    }
    let best = candidates
        .iter()
        .map(|c| (distance(typed, c), c))
        .filter(|(d, c)| *d <= 2.max(typed.len() / 4) || (typed.len() >= 3 && c.starts_with(typed)))
        .min_by_key(|(d, _)| *d);
    best.map_or(String::new(), |(_, c)| format!("。你是不是想输入 /{c}？"))
}

/// The `mcp_servers` listing (the tool's JSON) as lines for people, with the next
/// step when nothing is configured or a server still needs signing in.
fn mcp_servers(listing: &str) -> String {
    let servers: Vec<Value> = serde_json::from_str(listing).unwrap_or_default();
    if servers.is_empty() {
        return "还没有配置 MCP 服务器。在 ~/.cove/config.json（或项目的 .cove.json）的 \"mcp_servers\" 里添加，例如：\n  \"mcp_servers\": { \"atlassian\": { \"type\": \"http\", \"url\": \"https://mcp.atlassian.com/v2/mcp\" } }\n保存后重启 cove；需要登录的服务器再运行 /mcp login <名称>。\n".into();
    }
    let mut out = format!("MCP 服务器（{} 个）：\n", servers.len());
    let mut login = Vec::new();
    for server in &servers {
        let name = server["server"].as_str().unwrap_or("?");
        let transport = server["transport"].as_str().unwrap_or("stdio");
        let mut state = vec![if server["enabled"] == Value::Bool(false) {
            "已停用"
        } else if server["connected"] == Value::Bool(true) {
            "已连接"
        } else {
            "未连接（首次使用时自动连接）"
        }];
        if server["signed_in"] == Value::Bool(true) {
            state.push("已登录");
        } else if server["signed_in"] == Value::Bool(false) {
            state.push("未登录");
            login.push(name);
        }
        let place = server["url"].as_str().unwrap_or_default();
        let plugin = server["plugin"]
            .as_str()
            .map_or(String::new(), |plugin| format!("，来自插件 {plugin}"));
        out.push_str(&format!(
            "  {name}  {transport} {place}  {}{plugin}\n",
            state.join("，")
        ));
    }
    if !login.is_empty() {
        out.push_str(&format!(
            "需要登录的服务器（如 Jira/Confluence）先运行 /mcp login {}，浏览器里授权后即可使用。\n",
            login[0]
        ));
    }
    out.push_str("/mcp connect <名称> 立即连接并列出工具 · /mcp logout <名称> 退出登录\n");
    out
}

fn show(value: Value) -> Result<()> {
    print_text(&format!("{}\n", crate::tools::json_output(value)?), false)?;
    Ok(())
}

async fn authorize(app: &Application, effect: Effect) -> Result<()> {
    app.authorizer
        .authorize(effect, CancellationToken::new())
        .await
}

pub(crate) async fn handle(
    app: &mut Application,
    line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    let Some(command) = args.first().and_then(|c| c.strip_prefix('/')) else {
        return Ok(None);
    };
    if command == "config" {
        return configure(app, &args[1..]).await;
    }
    // `/skill` and `/skills`: a readable list, or one skill's instructions.
    if command == "skills" || command == "skill" {
        return match &args[1..] {
            [] | ["list"] => {
                print_skill_list(&app.tools.skills.listing()?)?;
                Ok(Some(false))
            }
            [sub, ..] if ["show", "create", "install", "marketplace"].contains(sub) => {
                let mut forwarded = vec!["skills"];
                forwarded.extend_from_slice(&args[1..]);
                configure(app, &forwarded).await
            }
            [name] => configure(app, &["skills", "show", name]).await,
            _ => Err(Error::Session(
                "用法：/skills [list] | /skill <name> | /skill show|create|install|marketplace ..."
                    .into(),
            )),
        };
    }
    if BUILTIN.contains(&command) {
        return Ok(None);
    }
    let rest = line
        .trim_start()
        .split_once(char::is_whitespace)
        .map_or("", |(_, rest)| rest.trim());
    if app.tools.skills.contains(command) {
        let text = app.tools.skills.render(command, Some(rest))?;
        app.execute(Some(text)).await?;
        return Ok(Some(false));
    }
    if let Some(plugin_command) = app.plugins.commands(&app.registry).remove(command) {
        let text = format!(
            "[Plugin command /{command} from plugin {}]\n\n{}",
            plugin_command.plugin,
            plugins::expand(&plugin_command.prompt, rest)
        );
        app.execute(Some(text)).await?;
        return Ok(Some(false));
    }
    Ok(None)
}

/// One line per skill: name, where it comes from, and its description.
fn print_skill_list(listing: &serde_json::Value) -> Result<()> {
    let skills = listing.as_array().cloned().unwrap_or_default();
    if skills.is_empty() {
        return print_text("没有找到技能；/config skills marketplace 可浏览可安装的技能，/config skills create <name> 可新建。\n", false).map_err(Into::into);
    }
    let width = skills
        .iter()
        .filter_map(|skill| skill["name"].as_str())
        .map(|name| name.chars().count())
        .max()
        .unwrap_or(0);
    let mut out = format!(
        "{} 个技能（运行用 /<name> [args]；详情用 /skill <name>）：\n",
        skills.len()
    );
    for skill in &skills {
        let name = skill["name"].as_str().unwrap_or_default();
        let origin = skill["origin"].as_str().unwrap_or_default();
        let disabled = if skill["enabled"].as_bool() == Some(false) {
            "（已禁用）"
        } else {
            ""
        };
        let description: String = skill["description"]
            .as_str()
            .unwrap_or_default()
            .split_whitespace()
            .collect::<Vec<_>>()
            .join(" ")
            .chars()
            .take(100)
            .collect();
        out.push_str(&format!(
            "  {name:<width$}  [{origin}]{disabled}  {description}\n"
        ));
    }
    print_text(&out, false).map_err(Into::into)
}

async fn configure(app: &mut Application, args: &[&str]) -> Result<Option<bool>> {
    match args {
        ["trust"] => trust(app).await?,
        ["skills"] | ["skills", "list"] => show(app.tools.skills.listing()?)?,
        ["skills", "show", name] => show(app.tools.skills.show(name)?)?,
        ["skills", "create", name] => create_skill(app, name).await?,
        ["skills", "install", source] => install_skill(app, source).await?,
        ["skills", "marketplace", query @ ..] => skill_marketplace(app, &query.join(" ")).await?,
        ["plugins"] | ["plugins", "list"] => list_plugins(app)?,
        ["plugins", "search", query @ ..] => search_plugins(app, &query.join(" ")).await?,
        ["plugins", "install", source]
            if plugins::is_git_url(source)
                || (identifier(source) && !Path::new(source).exists()) =>
        {
            install_plugin(app, source).await?;
        }
        ["plugins", "update", name @ ..] => update_plugins(app, name.first().copied()).await?,
        ["plugins", action @ ("enable" | "disable"), name] if app.plugins.get(name).is_some() => {
            authorize(app, Effect::Write(app.registry.path().into())).await?;
            let key = format!("plugin:{name}");
            let disable = *action == "disable";
            app.registry.change(|state| {
                if disable {
                    state.disabled.insert(key);
                } else {
                    state.disabled.remove(&key);
                }
                Ok(())
            })?;
            show(json!({"id":name,"action":action,"note":"命令和技能立即生效。"}))?;
        }
        ["plugins", "uninstall", name] if app.plugins.get(name).is_some() => {
            let dir = app
                .plugins
                .get(name)
                .map(|p| p.dir.clone())
                .unwrap_or_default();
            authorize(app, Effect::Write(dir.clone())).await?;
            std::fs::remove_dir_all(&dir)?;
            app.plugins.list.retain(|p| p.name != *name);
            show(json!({"uninstalled":name,"removed":dir}))?;
        }
        ["mcp"] | ["mcp", "list"] => {
            let result = app
                .tools
                .mcp
                .execute(McpArgs::Servers, CancellationToken::new())
                .await?;
            print_text(&mcp_servers(&result.content), false)?;
        }
        ["mcp", "connect", name] => {
            let tools = app
                .tools
                .mcp
                .connect_now(name, CancellationToken::new())
                .await?;
            show(json!({"connected":name,"tools":tools}))?;
        }
        ["mcp", "login", name] => {
            let say = |text: &str| {
                let _ = print_text(text, true);
            };
            app.tools.mcp.login(name, &say).await?;
            show(json!({"signed_in":name,"note":"令牌保存在数据目录中，并会自动刷新"}))?;
        }
        ["mcp", "logout", name] => {
            let removed = app.tools.mcp.logout(name).await?;
            show(json!({"signed_out":name,"had_sign_in":removed}))?;
        }
        ["mcp", "disconnect", "all"] => {
            app.tools.mcp.close(None, None).await?;
            show(json!({"disconnected":"all"}))?;
        }
        ["mcp", "disconnect", name] => {
            if !app.tools.mcp.ids().iter().any(|id| id == name) {
                return Err(Error::Tool(
                    "未知的 MCP 服务器；/config mcp list 查看已配置的服务器".into(),
                ));
            }
            app.tools.mcp.close(None, Some(name)).await?;
            show(json!({"disconnected":name}))?;
        }
        ["mcp", "read", server, uri] => {
            let result = app
                .tools
                .mcp
                .execute(
                    McpArgs::Read {
                        server: (*server).into(),
                        uri: (*uri).into(),
                    },
                    CancellationToken::new(),
                )
                .await?;
            print_text(&format!("{}\n", result.content), false)?;
        }
        _ => return Ok(None),
    }
    Ok(Some(false))
}

async fn trust(app: &mut Application) -> Result<()> {
    let data = app.store.root().to_path_buf();
    authorize(app, Effect::Write(crate::config::trust::store_path(&data))).await?;
    let config = app.settings.project_config.clone();
    let root = crate::config::trust::trust(&data, &app.settings.cwd, config.as_ref())?;
    let mut text = format!(
        "已信任项目 {}：验证命令和项目扩展可以运行（每个操作仍需授权）。",
        root.display()
    );
    if let Some(config) = config {
        text.push_str(&format!(
            " 已按当前内容信任 {}（sha256 {}）；以后修改它需要再次 /config trust。",
            config.path.display(),
            &config.hash[..12]
        ));
        if !config.ignored.is_empty() {
            text.push_str(&format!(" 重启后生效：{}。", config.ignored.join(", ")));
        }
    }
    text.push_str(" 用 /restart 重启 cove 后，MCP 服务器和钩子会以信任状态加载。\n");
    print_text(&text, false)?;
    Ok(())
}

fn skill_dir(app: &Application, name: &str) -> Result<std::path::PathBuf> {
    if !identifier(name) {
        return Err(Error::Tool(
            "技能名只能使用字母、数字、- 和 _（最多 64 个字符）".into(),
        ));
    }
    Ok(app.store.root().join("skills").join(name))
}

async fn write_skill(app: &Application, name: &str, text: &str) -> Result<String> {
    let dir = skill_dir(app, name)?;
    let path = dir.join("SKILL.md");
    if path.exists() || app.tools.skills.is_curated(name) {
        return Err(Error::Tool(format!(
            "技能 {name} 已存在；请直接编辑或删除它，或换个名称"
        )));
    }
    authorize(app, Effect::Write(path.clone())).await?;
    std::fs::create_dir_all(&dir)?;
    std::fs::write(&path, text)?;
    app.tools.skills.add(&path)?;
    Ok(path.display().to_string())
}

async fn create_skill(app: &Application, name: &str) -> Result<()> {
    let template = format!(
        "---\nname: {name}\ndescription: Describe when this skill applies (shown to the model, max 120 characters)\n# paths: \"*.rs, src/**/*.ts\"   # only suggested when a matching file is touched\n# allowed_tools: [read, grep, edit]\n# steps: [Understand the code, Make the change, Run the tests]\n---\n# {name}\n\nInstructions for the model.\n"
    );
    let path = write_skill(app, name, &template).await?;
    show(json!({"created":name,"path":path,"invoke":format!("/{name} [args]")}))
}

#[derive(serde::Deserialize)]
struct RegistryEntry {
    name: String,
    #[serde(default)]
    description: String,
    #[serde(default)]
    url: String,
}

/// Where the skill marketplace is read from (`skills_registry`).
enum Market {
    /// The marketplace built into the binary.
    Builtin,
    /// A `registry.json` over https; relative entry URLs join onto it.
    Remote(url::Url),
    /// A local `registry.json`; relative entry URLs are files below its directory.
    Local(std::path::PathBuf),
}

/// Most bytes of a registry and of one skill file.
const REGISTRY_LIMIT: usize = 1024 * 1024;
const SKILL_LIMIT: usize = 256 * 1024;

fn market(app: &Application) -> Result<Market> {
    let Some(raw) = app
        .settings
        .file
        .get("skills_registry")
        .and_then(Value::as_str)
        .map(str::trim)
        .filter(|raw| !raw.is_empty())
    else {
        return Ok(Market::Builtin);
    };
    if raw.contains("://") {
        let url = url::Url::parse(raw)
            .map_err(|err| Error::Tool(format!("skills_registry {raw:?}: {err}")))?;
        if url.scheme() != "https" {
            return Err(Error::Tool(
                "skills_registry must be an https URL or a local path".into(),
            ));
        }
        return Ok(Market::Remote(url));
    }
    // A relative path is relative to the config file that names it.
    let path = std::path::PathBuf::from(raw);
    let path = if path.is_absolute() {
        path
    } else {
        app.settings
            .config_path
            .as_deref()
            .and_then(std::path::Path::parent)
            .unwrap_or(&app.settings.data_dir)
            .join(path)
    };
    Ok(Market::Local(if path.is_dir() {
        path.join("registry.json")
    } else {
        path
    }))
}

/// The marketplace's entries; a configured marketplace that cannot be read falls
/// back to the built-in one with a warning.
async fn skill_registry(app: &Application) -> (Market, Vec<RegistryEntry>) {
    let market = match market(app) {
        Ok(market) => market,
        Err(err) => {
            warn(&format!("{err}；改用内置技能市场"));
            Market::Builtin
        }
    };
    let text = match &market {
        Market::Builtin => Ok(skills::MARKETPLACE_REGISTRY.to_owned()),
        Market::Remote(url) => {
            async {
                authorize(app, Effect::Network(format!("fetch {url}"))).await?;
                crate::tools::web::get_public_text(
                    url.as_str(),
                    REGISTRY_LIMIT,
                    &CancellationToken::new(),
                )
                .await
            }
            .await
        }
        Market::Local(path) => skills::read_text(path, REGISTRY_LIMIT),
    };
    let entries = text.and_then(|text| {
        serde_json::from_str::<Vec<RegistryEntry>>(&text)
            .map_err(|err| Error::Tool(format!("技能注册表不是条目列表：{err}")))
    });
    let (market, entries) = match entries {
        Ok(entries) => (market, entries),
        Err(err) => {
            warn(&format!("技能市场不可用（{err}）；改用内置市场"));
            let builtin = serde_json::from_str(skills::MARKETPLACE_REGISTRY).unwrap_or_default();
            (Market::Builtin, builtin)
        }
    };
    let entries = entries
        .into_iter()
        .filter(|entry| identifier(&entry.name))
        .collect();
    (market, entries)
}

/// The text of a marketplace entry's skill and where it came from.
async fn marketplace_skill(
    app: &Application,
    market: &Market,
    url: &str,
) -> Result<(String, String)> {
    if url.starts_with("https://") {
        return download_skill(app, url).await;
    }
    let relative = url.trim_start_matches("./");
    let escapes = std::path::Path::new(relative)
        .components()
        .any(|part| !matches!(part, std::path::Component::Normal(_)));
    if relative.is_empty() || escapes {
        return Err(Error::Tool(format!(
            "市场条目 url {url:?} 必须是 https 或市场内的路径"
        )));
    }
    match market {
        Market::Builtin => skills::MARKETPLACE_SKILLS
            .iter()
            .find(|(path, _)| *path == relative)
            .map(|(path, text)| ((*text).to_owned(), format!("built-in marketplace: {path}")))
            .ok_or_else(|| Error::Tool(format!("{relative} 不在内置市场中"))),
        Market::Remote(base) => {
            let url = base
                .join(relative)
                .map_err(|err| Error::Tool(format!("市场条目 url {url:?} 无效：{err}")))?;
            download_skill(app, url.as_str()).await
        }
        Market::Local(registry) => {
            let path = registry
                .parent()
                .unwrap_or(std::path::Path::new("."))
                .join(relative);
            let text = skills::read_text(&path, SKILL_LIMIT)?;
            Ok((text, path.display().to_string()))
        }
    }
}

async fn download_skill(app: &Application, url: &str) -> Result<(String, String)> {
    if !url.starts_with("https://") {
        return Err(Error::Tool(
            "技能只能通过 https 下载；请改用 https:// 地址".into(),
        ));
    }
    authorize(app, Effect::Network(format!("download skill {url}"))).await?;
    let text =
        crate::tools::web::get_public_text(url, SKILL_LIMIT, &CancellationToken::new()).await?;
    Ok((text, url.to_owned()))
}

async fn skill_marketplace(app: &Application, query: &str) -> Result<()> {
    let query = query.to_lowercase();
    let (_, entries) = skill_registry(app).await;
    let list: Vec<Value> = entries
        .into_iter()
        .filter(|e| {
            query.is_empty()
                || e.name.to_lowercase().contains(&query)
                || e.description.to_lowercase().contains(&query)
        })
        .map(|e| {
            json!({"name":e.name,"description":skills::clip(&e.description, 80),
            "installed":app.tools.skills.contains(&e.name)})
        })
        .collect();
    show(json!({"skills":list,"install":"/config skills install <name>"}))
}

async fn install_skill(app: &Application, source: &str) -> Result<()> {
    let (name, url, text, origin) = if source.starts_with("https://") {
        let (text, origin) = download_skill(app, source).await?;
        (None, source.to_owned(), text, origin)
    } else {
        let (market, entries) = skill_registry(app).await;
        let entry = entries
            .into_iter()
            .find(|e| e.name.eq_ignore_ascii_case(source))
            .ok_or_else(|| {
                Error::Tool(format!(
                    "市场中没有技能 {source:?}；/config skills marketplace 查看可用技能"
                ))
            })?;
        let (text, origin) = marketplace_skill(app, &market, &entry.url).await?;
        (Some(entry.name), entry.url, text, origin)
    };
    let parsed = skills::parse(&text)?;
    if crate::tools::external::suspicious(&text) {
        return Err(Error::Tool("下载的技能包含覆盖指令的文本，未安装".into()));
    }
    let fallback = url
        .trim_end_matches('/')
        .rsplit('/')
        .nth(usize::from(url.ends_with("SKILL.md")))
        .unwrap_or_default()
        .trim_end_matches(".md")
        .to_owned();
    let name = name.or(parsed.name).unwrap_or(fallback);
    let path = write_skill(app, &name, &text).await?;
    show(
        json!({"installed":name,"path":path,"source":origin,"note":"技能文本是不受信任的指令数据；请用 /config skills show 审查。"}),
    )
}

fn list_plugins(app: &Application) -> Result<()> {
    let mut list = Vec::new();
    for plugin in &app.plugins.list {
        list.push(json!({"id":plugin.name,"version":plugin.manifest.version,
            "description":skills::clip(&plugin.manifest.description, 80),"author":plugin.manifest.author,
            "source":plugin.dir,"git":plugin.git,"format":"data_dir",
            "enabled":app.registry.active("plugin",&plugin.name,None)?}));
    }
    let mut manifests = app.catalog.plugins.clone();
    manifests.extend(app.registry.snapshot()?.installed);
    for (id, path) in manifests {
        list.push(json!({"id":id,"source":path,"format":"manifest","loaded":app.catalog.plugins.contains_key(&id),
            "enabled":app.registry.active("plugin",&id,None)?}));
    }
    let commands: Vec<Value> = app
        .plugins
        .commands(&app.registry)
        .into_iter()
        .map(|(name, c)| json!({"command":format!("/{name}"),"plugin":c.plugin,"description":c.description}))
        .collect();
    show(json!({"plugins":list,"commands":commands}))
}

async fn marketplace(app: &Application) -> Result<Vec<plugins::Entry>> {
    authorize(
        app,
        Effect::Network(format!("fetch {}", plugins::MARKETPLACE_URL)),
    )
    .await?;
    let text = crate::tools::web::get_public_text(
        plugins::MARKETPLACE_URL,
        2 * 1024 * 1024,
        &CancellationToken::new(),
    )
    .await?;
    plugins::parse_marketplace(&text)
}

async fn search_plugins(app: &Application, query: &str) -> Result<()> {
    let query = query.to_lowercase();
    let list: Vec<Value> = marketplace(app)
        .await?
        .into_iter()
        .filter(|e| {
            query.is_empty()
                || e.name.to_lowercase().contains(&query)
                || e.description.to_lowercase().contains(&query)
        })
        .take(50)
        .map(|e| {
            json!({"name":e.name,"description":skills::clip(&e.description, 80),"author":e.author,
            "installed":app.plugins.get(&e.name).is_some()})
        })
        .collect();
    show(json!({"plugins":list,"install":"/config plugins install <name|git-url>"}))
}

async fn install_plugin(app: &mut Application, source: &str) -> Result<()> {
    let (url, subdir, name) = if plugins::is_git_url(source) {
        (source.to_owned(), None, None)
    } else {
        let entry = marketplace(app)
            .await?
            .into_iter()
            .find(|e| e.name.eq_ignore_ascii_case(source))
            .ok_or_else(|| {
                Error::Tool(format!(
                    "市场中没有插件 {source:?}；/config plugins search 查看可用插件"
                ))
            })?;
        match (entry.url, entry.path) {
            (Some(url), _) => (url, None, Some(entry.name)),
            (None, path) => (plugins::MARKETPLACE_REPO.to_owned(), path, Some(entry.name)),
        }
    };
    let root = app.plugins.root.clone();
    authorize(
        app,
        Effect::Shell(format!("git clone --depth=1 {url} into {}", root.display())),
    )
    .await?;
    let plugin = plugins::install_git(
        &root,
        &url,
        subdir.as_deref(),
        name.as_deref(),
        &CancellationToken::new(),
    )
    .await?;
    let name = plugin.name.clone();
    app.plugins.list.push(plugin);
    show(json!({"installed":name,"source":url,
        "note":"插件命令现在即可使用；其技能在 /restart 后加载。cove 不会执行插件中的任何内容。"}))
}

async fn update_plugins(app: &Application, name: Option<&str>) -> Result<()> {
    let targets: Vec<_> = app
        .plugins
        .list
        .iter()
        .filter(|p| name.is_none_or(|n| p.name == n))
        .cloned()
        .collect();
    if targets.is_empty() {
        return Err(Error::Tool(
            "没有这个插件；/config plugins list 查看已安装的插件".into(),
        ));
    }
    let mut report = Vec::new();
    for plugin in targets {
        if !plugin.git {
            report.push(json!({"plugin":plugin.name,"result":"不是 git 检出，已跳过"}));
            continue;
        }
        authorize(
            app,
            Effect::Shell(format!("git pull --ff-only in {}", plugin.dir.display())),
        )
        .await?;
        let dir = plugin.dir.display().to_string();
        let result = plugins::git(
            &["-C", &dir, "pull", "--ff-only", "--quiet"],
            &plugin.dir,
            &CancellationToken::new(),
        )
        .await;
        report.push(json!({"plugin":plugin.name,"result":match result {
            Ok(_) => "已更新（/restart 后重新加载）".into(), Err(err) => err.to_string() }}));
    }
    show(json!(report))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mcp_listing_reads_as_sentences_with_the_next_step() {
        assert!(mcp_servers("[]").starts_with("还没有配置 MCP 服务器"));
        let listing = r#"[{"server":"atlassian","plugin":null,"transport":"http","url":"https://mcp.atlassian.com/v2/mcp","connected":false,"signed_in":false,"enabled":true}]"#;
        let text = mcp_servers(listing);
        assert!(
            text.contains("atlassian  http https://mcp.atlassian.com/v2/mcp  未连接"),
            "{text}"
        );
        assert!(
            text.contains("未登录") && text.contains("/mcp login atlassian"),
            "{text}"
        );
    }

    #[test]
    fn edit_distance() {
        assert_eq!(distance("stauts", "status"), 2);
        assert_eq!(distance("plan", "plan"), 0);
        assert_eq!(distance("", "ab"), 2);
    }
}
