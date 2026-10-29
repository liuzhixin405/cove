//! Model, provider, profile and live settings (/model, /config key value),
//! attachments (/attach, `@path`) and `/status limits`.

use std::{
    path::{Path, PathBuf},
    sync::Arc,
};

use serde_json::{Map, Value, json};

use crate::{
    app::Application,
    app::ui::print_text,
    config::{Settings, default_model, provider_defaults, read_json_file, write_json_file},
    domain::{Block, Error, Result},
    model::provider::{HttpProvider, ProviderOptions},
    model::routing::ModelRouter,
};

pub(crate) const HELP: &str = "\
/model [<name>|fast <name|off>|provider <name> [model]|profile [list|switch|save|delete|show] <name>]  查看或切换模型、快速模型、服务商和配置档
/config <model|provider|api_key|base_url|system|budget|max_output_tokens> [value]  查看或修改设置   /config budget auto|off|<usd>|save  会话预算   /config setup  重新运行配置向导
/attach [add] <files...>|list|remove <n>|clear  附加文件（也可在消息中写 @路径）   /status limits  速率限制";

/// Live model state owned by the application.
pub(crate) struct ModelState {
    pub(crate) router: Arc<ModelRouter>,
    /// Files staged with /attach for the next message.
    pub(crate) attachments: Vec<PathBuf>,
}

/// The HTTP provider `settings` describe.
pub(crate) fn build_provider(settings: &Settings) -> Result<Arc<HttpProvider>> {
    Ok(Arc::new(HttpProvider::with_options(
        settings.protocol,
        &settings.base_url,
        settings.model.clone(),
        settings.api_keys.clone(),
        ProviderOptions {
            thinking: settings.thinking,
            effort: settings.effort.clone(),
            bedrock: settings.provider == "bedrock",
        },
    )?))
}

/// The router, with learned windows saved to the user config.
pub(crate) fn new_router(settings: &Settings) -> Result<Arc<ModelRouter>> {
    let router = Arc::new(ModelRouter::new(
        build_provider(settings)?,
        settings.model_fast.clone(),
        settings.prices,
    ));
    let path = config_target(settings);
    router.on_window_learned(Box::new(move |model, window| {
        let _ = edit_file(&path, |root| {
            let windows = object(root)
                .entry("model_context_windows")
                .or_insert_with(|| json!({}));
            if !windows.is_object() {
                *windows = json!({});
            }
            windows[model.to_ascii_lowercase()] = json!(window);
            Ok(())
        });
    }));
    Ok(router)
}

fn config_target(settings: &Settings) -> PathBuf {
    settings
        .config_path
        .clone()
        .unwrap_or_else(|| settings.data_dir.join("config.json"))
}

fn read_root(path: &Path) -> Result<Value> {
    if !path.exists() {
        return Ok(json!({}));
    }
    let root = read_json_file(path).map_err(|err| Error::Session(format!("{err:#}")))?;
    if !root.is_object() {
        return Err(Error::Session(format!(
            "配置文件 {} 必须是 JSON 对象；请修正后重试",
            path.display()
        )));
    }
    Ok(root)
}

fn edit_file(path: &Path, edit: impl FnOnce(&mut Value) -> Result<()>) -> Result<()> {
    let mut root = read_root(path)?;
    edit(&mut root)?;
    write_json_file(path, &root).map_err(|err| Error::Session(format!("保存配置失败：{err:#}")))
}

fn object(value: &mut Value) -> &mut Map<String, Value> {
    if !value.is_object() {
        *value = json!({});
    }
    value.as_object_mut().expect("just made an object")
}

fn pointer(profile: &str) -> String {
    format!(
        "/profiles/{}",
        profile.replace('~', "~0").replace('/', "~1")
    )
}

/// The object a setting is written to: the active profile, else the root.
fn target<'a>(root: &'a mut Value, profile: Option<&str>) -> &'a mut Map<String, Value> {
    let exists = profile.is_some_and(|name| root.pointer(&pointer(name)).is_some());
    match profile {
        Some(name) if exists => object(root.pointer_mut(&pointer(name)).expect("checked")),
        _ => object(root),
    }
}

/// `sk-1****abcd`: enough to recognize a key, never enough to use it.
pub(crate) fn redact(key: &str) -> String {
    let chars: Vec<char> = key.chars().collect();
    if chars.len() <= 10 {
        "****".into()
    } else {
        format!(
            "{}****{}",
            chars[..4].iter().collect::<String>(),
            chars[chars.len() - 4..].iter().collect::<String>()
        )
    }
}

fn redacted(mut value: Value) -> Value {
    if let Some(object) = value.as_object_mut() {
        for (key, item) in object.iter_mut() {
            if key == "api_key" {
                if let Some(text) = item.as_str() {
                    *item = json!(redact(text));
                }
            } else if key == "api_keys" {
                if let Some(keys) = item.as_array_mut() {
                    for key in keys {
                        if let Some(text) = key.as_str() {
                            *key = json!(redact(text));
                        }
                    }
                }
            } else {
                *item = redacted(item.take());
            }
        }
    }
    value
}

/// "auto" picks the provider's default; a `provider/` prefix naming the provider
/// itself is dropped (gateways such as OpenRouter keep it).
fn normalize_model(raw: &str, provider: &str) -> Result<String> {
    let model = raw.trim();
    if model.is_empty()
        || model.eq_ignore_ascii_case("auto")
        || model.eq_ignore_ascii_case("default")
    {
        return default_model(provider).map(str::to_owned).ok_or_else(|| {
            Error::Session(format!(
                "服务商 {provider} 没有默认模型；请指定模型名，例如 /model <name>"
            ))
        });
    }
    let gateway = matches!(
        provider,
        "openrouter" | "together" | "fireworks" | "siliconflow" | "groq" | "openai-compatible"
    );
    if !gateway
        && let Some((prefix, rest)) = model.split_once('/')
        && (prefix.eq_ignore_ascii_case(provider)
            || provider_defaults(prefix).is_some_and(|(name, ..)| name == provider))
        && !rest.is_empty()
    {
        return Ok(rest.to_owned());
    }
    Ok(model.to_owned())
}

/// What a change does to the command line before the config is reloaded.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Reload {
    /// Write the running model/provider into the file first, so a start-up
    /// `--model`/`--provider` survives a change of another key.
    Seeded,
    /// Profile switch: the profile's own values win.
    Profile,
}

/// The effective provider block, as an object, for edits of one of its keys.
fn provider_block<'a>(
    target: &'a mut Map<String, Value>,
    settings: &Settings,
) -> &'a mut Map<String, Value> {
    let effective = match settings.file.get("provider") {
        Some(Value::String(name)) => json!({"name": name}),
        Some(Value::Object(block)) => Value::Object(block.clone()),
        _ => json!({}),
    };
    let same = effective["name"]
        .as_str()
        .and_then(provider_defaults)
        .map(|(name, ..)| name)
        .unwrap_or_else(|| effective["name"].as_str().unwrap_or(""))
        == settings.provider;
    let entry = target.entry("provider").or_insert(Value::Null);
    if !entry.is_object() {
        *entry = if same {
            effective
        } else {
            json!({"name": settings.provider})
        };
    }
    let block = object(entry);
    block
        .entry("name")
        .or_insert_with(|| json!(settings.provider));
    block
}

impl Application {
    fn model_router(&self) -> Arc<ModelRouter> {
        self.models.router.clone()
    }

    /// Edits the user config, validates it by loading it, saves it and applies it live.
    /// Nothing is written when the result does not load.
    async fn change(
        &mut self,
        reload: Reload,
        profile: Option<String>,
        edit: impl FnOnce(&mut Value, &Settings) -> Result<()>,
    ) -> Result<()> {
        let path = config_target(&self.settings);
        let mut root = read_root(&path)?;
        if reload == Reload::Seeded {
            let settings = &self.settings;
            let target = target(&mut root, settings.profile.as_deref());
            if settings.file.get("model").and_then(Value::as_str) != Some(settings.model.as_str()) {
                target.insert("model".into(), json!(settings.model));
            }
            let file_provider = match settings.file.get("provider") {
                Some(Value::String(name)) => Some(name.clone()),
                Some(Value::Object(block)) => {
                    block.get("name").and_then(Value::as_str).map(str::to_owned)
                }
                _ => None,
            }
            .and_then(|name| {
                provider_defaults(&name)
                    .map(|(name, ..)| name.to_owned())
                    .or(Some(name))
            });
            if file_provider.as_deref() != Some(settings.provider.as_str()) {
                // A start-up provider replaces the configured block, key included.
                target.insert("provider".into(), json!({"name": settings.provider}));
            }
            if settings.cli.base_url.is_some() {
                provider_block(target, settings)
                    .insert("base_url".into(), json!(settings.base_url));
            }
        }
        edit(&mut root, &self.settings)?;
        let mut cli = self.settings.cli.clone();
        cli.provider = None;
        cli.model = None;
        cli.base_url = None;
        cli.max_output_tokens = None;
        cli.config = Some(path.clone());
        cli.profile = match reload {
            Reload::Profile => profile,
            Reload::Seeded => self.settings.profile.clone(),
        };
        let mut next = Settings::load_with(cli, Some(root.clone()))
            .map_err(|err| Error::Session(format!("修改未生效：{err:#}")))?;
        write_json_file(&path, &root)
            .map_err(|err| Error::Session(format!("保存配置失败：{err:#}")))?;
        next.config_path = Some(path);
        self.apply(next)
    }

    /// Takes over the model-related parts of `next`; the session keeps running.
    fn apply(&mut self, next: Settings) -> Result<()> {
        let provider = build_provider(&next)?;
        self.models
            .router
            .replace(provider, next.model_fast.clone(), next.prices);
        let current = &mut self.settings;
        let mut notes = Vec::new();
        let budget = match (self.agent.limits().budget, next.prices) {
            (Some(_), None) => {
                notes.push("预算已关闭：新模型的价格未知".to_owned());
                None
            }
            (budget, _) => budget,
        };
        {
            let limits = self.agent.limits_mut();
            limits.max_output_tokens = next.max_output_tokens;
            limits.prices = next.prices;
            limits.budget = budget;
        }
        self.agent.set_context_tokens(next.context_tokens)?;
        self.agent.set_instructions(next.instructions.clone());
        crate::app::ui::set_show_reasoning(next.show_reasoning);
        current.budget = budget;
        current.file = next.file;
        current.config_path = next.config_path;
        current.profile = next.profile;
        current.provider = next.provider;
        current.protocol = next.protocol;
        current.model = next.model;
        current.base_url = next.base_url;
        current.api_key = next.api_key;
        current.api_keys = next.api_keys;
        current.model_fast = next.model_fast;
        current.thinking = next.thinking;
        current.effort = next.effort;
        current.show_reasoning = next.show_reasoning;
        current.instructions = next.instructions;
        current.context_window = next.context_window;
        current.context_tokens = next.context_tokens;
        current.max_output_tokens = next.max_output_tokens;
        current.prices = next.prices;
        current.cli = next.cli;
        self.session.model = current.model.clone();
        self.session.protocol = format!("{:?}", current.protocol);
        for warning in next.warnings {
            notes.push(warning);
        }
        print_text(
            &format!(
                "provider={} model={}{}（已保存）\n{}",
                current.provider,
                current.model,
                current
                    .model_fast
                    .as_deref()
                    .map_or(String::new(), |fast| format!(" fast={fast}")),
                notes
                    .iter()
                    .map(|note| format!("注意：{note}\n"))
                    .collect::<String>()
            ),
            false,
        )?;
        Ok(())
    }

    fn show_models(&self) -> Result<()> {
        let settings = &self.settings;
        let mut text = format!(
            "provider={} protocol={:?} base_url={}\nmodel={} fast={} current={}\nprofile={} keys={} thinking={:?} effort={} show_reasoning={}\n",
            settings.provider,
            settings.protocol,
            settings.base_url,
            settings.model,
            settings.model_fast.as_deref().unwrap_or("-"),
            self.models.router.current_model(),
            settings.profile.as_deref().unwrap_or("-"),
            settings.api_keys.len(),
            settings.thinking,
            settings.effort.as_deref().unwrap_or("-"),
            settings.show_reasoning
        );
        for (model, spend) in &self.session.accounting.by_model {
            text.push_str(&format!(
                "  {model}: calls={} input={} (cache read {} / write {}) output={} cost={}\n",
                spend.calls,
                spend.usage.input,
                spend.usage.cache_read,
                spend.usage.cache_write,
                spend.usage.output,
                spend
                    .cost
                    .map_or("unknown".into(), |cost| format!("${cost:.4}"))
            ));
        }
        print_text(&text, false)?;
        Ok(())
    }

    async fn set_model(&mut self, raw: &str) -> Result<()> {
        let model = normalize_model(raw, &self.settings.provider)?;
        self.change(Reload::Seeded, None, |root, settings| {
            target(root, settings.profile.as_deref()).insert("model".into(), json!(model));
            Ok(())
        })
        .await
    }

    async fn set_fast(&mut self, raw: &str) -> Result<()> {
        let fast = match raw.trim() {
            "off" | "none" | "" => None,
            name => Some(normalize_model(name, &self.settings.provider)?),
        };
        self.change(Reload::Seeded, None, |root, settings| {
            let target = target(root, settings.profile.as_deref());
            match fast {
                Some(fast) => target.insert("model_fast".into(), json!(fast)),
                None => target.insert("model_fast".into(), json!("off")),
            };
            Ok(())
        })
        .await
    }

    async fn set_provider(&mut self, name: &str, model: Option<&str>) -> Result<()> {
        let (canonical, ..) = provider_defaults(name).ok_or_else(|| {
            Error::Session(format!(
                "未知服务商 {name:?}；其他兼容 OpenAI 的 API 请用 /model provider openai-compatible，再用 /config base_url <url> 设置地址"
            ))
        })?;
        let old_default = default_model(&self.settings.provider);
        let model = match model {
            Some(model) => Some(normalize_model(model, canonical)?),
            // The old provider's default follows the switch; a chosen model stays.
            None if old_default == Some(self.settings.model.as_str()) => {
                default_model(canonical).map(str::to_owned)
            }
            None => Some(self.settings.model.clone()),
        };
        let mut key: Option<String> = None;
        loop {
            let attempt_key = key.clone();
            let model = model.clone();
            let result = self
                .change(Reload::Seeded, None, |root, settings| {
                    let target = target(root, settings.profile.as_deref());
                    // A new provider never inherits the old provider's key or root.
                    let mut block = json!({"name": canonical});
                    if let Some(key) = attempt_key {
                        block["api_key"] = json!(key);
                    }
                    target.insert("provider".into(), block);
                    match model {
                        Some(model) => target.insert("model".into(), json!(model)),
                        None => target.remove("model"),
                    };
                    Ok(())
                })
                .await;
            match result {
                Err(Error::Session(message))
                    if key.is_none() && message.contains("API_KEY") && self.input.is_some() =>
                {
                    print_text(&format!("{message}\n"), true)?;
                    key = Some(ask_key(&format!("{canonical} 的 API 密钥（输入不显示）：")).await?);
                }
                other => return other,
            }
        }
    }

    async fn profile(&mut self, args: &[&str]) -> Result<()> {
        let path = config_target(&self.settings);
        let root = read_root(&path)?;
        let profiles = root
            .get("profiles")
            .and_then(Value::as_object)
            .cloned()
            .unwrap_or_default();
        match args {
            [] | ["list"] => {
                if profiles.is_empty() {
                    print_text(
                        "没有保存的配置档；用 /model profile save <name> 保存当前设置\n",
                        false,
                    )?;
                }
                for name in profiles.keys() {
                    let mark = if self.settings.profile.as_deref() == Some(name.as_str()) {
                        "*"
                    } else {
                        " "
                    };
                    print_text(&format!("{mark} {name}\n"), false)?;
                }
                Ok(())
            }
            ["show", name] => {
                let profile = profiles.get(*name).ok_or_else(|| {
                    Error::Session(format!("找不到配置档 {name}；/model profile list 查看全部"))
                })?;
                print_text(
                    &format!(
                        "{}\n",
                        serde_json::to_string_pretty(&redacted(profile.clone()))?
                    ),
                    false,
                )?;
                Ok(())
            }
            ["switch", name] => {
                if !profiles.contains_key(*name) {
                    return Err(Error::Session(format!(
                        "找不到配置档 {name}；/model profile list 查看全部"
                    )));
                }
                let name = (*name).to_owned();
                self.change(Reload::Profile, Some(name.clone()), |root, _| {
                    object(root).insert("active_profile".into(), json!(name));
                    Ok(())
                })
                .await
            }
            ["save", name] => {
                let settings = &self.settings;
                let mut profile = Map::new();
                profile.insert("model".into(), json!(settings.model));
                let mut block = Map::new();
                provider_block(&mut block, settings);
                if let Some(Value::Object(provider)) = block.remove("provider") {
                    let mut provider = provider;
                    if settings.cli.base_url.is_some() {
                        provider.insert("base_url".into(), json!(settings.base_url));
                    }
                    profile.insert("provider".into(), Value::Object(provider));
                }
                for key in [
                    "model_fast",
                    "thinking",
                    "effort",
                    "show_reasoning",
                    "max_output_tokens",
                    "system_prompt",
                ] {
                    if let Some(value) = settings.file.get(key) {
                        profile.insert(key.into(), value.clone());
                    }
                }
                let name = (*name).to_owned();
                edit_file(&path, |root| {
                    let profiles = object(root).entry("profiles").or_insert_with(|| json!({}));
                    object(profiles).insert(name.clone(), Value::Object(profile));
                    Ok(())
                })?;
                print_text(&format!("配置档已保存：{name}\n"), false)?;
                Ok(())
            }
            ["delete", name] => {
                if !profiles.contains_key(*name) {
                    return Err(Error::Session(format!(
                        "找不到配置档 {name}；/model profile list 查看全部"
                    )));
                }
                let name = (*name).to_owned();
                edit_file(&path, |root| {
                    let root = object(root);
                    if let Some(profiles) = root.get_mut("profiles").and_then(Value::as_object_mut)
                    {
                        profiles.remove(&name);
                    }
                    if root.get("active_profile").and_then(Value::as_str) == Some(name.as_str()) {
                        root.remove("active_profile");
                    }
                    Ok(())
                })?;
                if self.settings.profile.as_deref() == Some(name.as_str()) {
                    // Its values stay in effect for this run; the next start uses the root.
                    self.settings.profile = None;
                    self.settings.cli.profile = None;
                }
                print_text(&format!("配置档已删除：{name}\n"), false)?;
                Ok(())
            }
            _ => Err(Error::Session(
                "用法：/model profile [list|switch|save|delete|show] <name>".into(),
            )),
        }
    }

    async fn budget(&mut self, value: Option<&str>) -> Result<()> {
        let spent = self.session.accounting.cost;
        let current = self.agent.limits().budget;
        let label = |budget: Option<f64>| budget.map_or("关闭".into(), |b| format!("${b:.2}"));
        let set = |app: &mut Self, budget: Option<f64>| -> Result<()> {
            if budget.is_some() && app.agent.limits().prices.is_none() {
                return Err(Error::Session(
                    "设置预算需要已知价格；请在配置中设置 input_price 和 output_price，或用 /config budget off".into(),
                ));
            }
            app.agent.limits_mut().budget = budget;
            app.settings.budget = budget;
            Ok(())
        };
        match value.map(str::trim) {
            None | Some("") => {
                print_text(
                    &format!(
                        "本会话预算 {}；已花费 {}；配置值 {}\n用法：/config budget <usd>|auto|off（仅本会话）| save（写入配置）\n",
                        label(current),
                        spent.map_or("未知".into(), |cost| format!("${cost:.4}")),
                        self.settings
                            .file
                            .get("max_budget_usd")
                            .map_or("-".into(), Value::to_string)
                    ),
                    false,
                )?;
            }
            Some("off") => {
                set(self, None)?;
                print_text(
                    "本会话预算已关闭（/config budget save 写入配置的上限）\n",
                    false,
                )?;
            }
            Some("auto") => {
                // Ten dollars beyond what is already spent, never lower than the current cap.
                let budget = (spent.unwrap_or(0.0) + 10.0).max(current.unwrap_or(0.0));
                set(self, Some(budget))?;
                print_text(&format!("本会话预算 {}\n", label(Some(budget))), false)?;
            }
            Some("save") => {
                let budget = current.ok_or_else(|| {
                    Error::Session(
                        "不限预算只对本会话有效；请先设置金额，例如 /config budget 20，再 /config budget save".into(),
                    )
                })?;
                edit_file(&config_target(&self.settings), |root| {
                    target(root, self.settings.profile.as_deref())
                        .insert("max_budget_usd".into(), json!(budget));
                    Ok(())
                })?;
                print_text(
                    &format!("预算 {} 已保存到配置\n", label(Some(budget))),
                    false,
                )?;
            }
            Some(amount) => {
                let budget: f64 = amount
                    .trim_start_matches('$')
                    .parse()
                    .ok()
                    .filter(|b: &f64| b.is_finite() && *b > 0.0)
                    .ok_or_else(|| {
                        Error::Session(
                            "预算必须是正的美元金额、auto、off 或 save，例如 /config budget 20"
                                .into(),
                        )
                    })?;
                set(self, Some(budget))?;
                print_text(
                    &format!(
                        "本会话预算 {}（/config budget save 写入配置）\n",
                        label(Some(budget))
                    ),
                    false,
                )?;
            }
        }
        Ok(())
    }

    async fn config_key(&mut self, key: &str, value: Option<&str>) -> Result<()> {
        let show = |text: String| print_text(&format!("{text}\n"), false);
        match (key, value) {
            ("model", None) => show(self.settings.model.clone())?,
            ("model", Some(model)) => self.set_model(model).await?,
            ("provider", None) => show(self.settings.provider.clone())?,
            ("provider", Some(value)) => {
                let mut parts = value.split_whitespace();
                let name = parts.next().unwrap_or_default();
                self.set_provider(name, parts.next()).await?
            }
            ("api_key", value) => {
                let key = match value {
                    Some(key) => key.trim().to_owned(),
                    None if self.input.is_some() => ask_key("API 密钥（输入不显示）：").await?,
                    None => {
                        show(format!(
                            "api_key={}（{} 个密钥）",
                            redact(&self.settings.api_key),
                            self.settings.api_keys.len()
                        ))?;
                        return Ok(());
                    }
                };
                if key.is_empty() || key.contains("****") {
                    return Err(Error::Session(
                        "请输入完整的 API 密钥（不是带 **** 的脱敏值）：/config api_key <key>"
                            .into(),
                    ));
                }
                if std::env::var_os("COVE_API_KEY").is_some() {
                    print_text("注意：已设置 COVE_API_KEY，它仍优先于保存的密钥\n", true)?;
                }
                self.change(Reload::Seeded, None, |root, settings| {
                    let target = target(root, settings.profile.as_deref());
                    let block = provider_block(target, settings);
                    block.insert("api_key".into(), json!(key));
                    block.remove("api_keys");
                    Ok(())
                })
                .await?;
                show(format!("api_key={}", redact(&self.settings.api_key)))?;
            }
            ("base_url", None) => show(self.settings.base_url.clone())?,
            ("base_url", Some(url)) => {
                let url = url.trim().to_owned();
                self.change(Reload::Seeded, None, |root, settings| {
                    let target = target(root, settings.profile.as_deref());
                    provider_block(target, settings).insert("base_url".into(), json!(url));
                    Ok(())
                })
                .await?
            }
            ("system", None) => show(
                self.settings
                    .instructions
                    .clone()
                    .unwrap_or_else(|| "（无）".into()),
            )?,
            ("system", Some(text)) => {
                let text = text.trim().to_owned();
                let clear = matches!(text.as_str(), "off" | "none" | "clear");
                self.change(Reload::Seeded, None, |root, settings| {
                    let target = target(root, settings.profile.as_deref());
                    if clear {
                        target.insert("system_prompt".into(), json!(""));
                    } else {
                        target.insert("system_prompt".into(), json!(text));
                    }
                    Ok(())
                })
                .await?
            }
            ("budget", value) => self.budget(value).await?,
            ("max_output_tokens", None) => show(self.settings.max_output_tokens.to_string())?,
            ("max_output_tokens", Some(value)) => {
                let tokens: u32 = value
                    .trim()
                    .parse()
                    .ok()
                    .filter(|tokens| (1..=1_000_000).contains(tokens))
                    .ok_or_else(|| Error::Session("max_output_tokens 必须在 1..1000000 之间，例如 /config max_output_tokens 8192".into()))?;
                self.change(Reload::Seeded, None, |root, settings| {
                    target(root, settings.profile.as_deref())
                        .insert("max_output_tokens".into(), json!(tokens));
                    Ok(())
                })
                .await?
            }
            _ => unreachable!("dispatch only passes known keys"),
        }
        Ok(())
    }

    async fn setup(&mut self) -> Result<()> {
        if self.input.is_none() {
            return Err(Error::Session(
                "/config setup 需要交互式终端；也可以直接编辑配置文件，或用 /config <key> <value> 逐项设置".into(),
            ));
        }
        let path = config_target(&self.settings);
        crate::app::setup::run(&path, "重新配置服务商、模型和密钥")
            .await
            .map_err(|err| Error::Session(format!("{err:#}")))?;
        // The wizard wrote the active profile (or root); load it as written.
        self.change(
            Reload::Profile,
            self.settings.profile.clone(),
            |_, _| Ok(()),
        )
        .await
    }

    fn attach(&mut self, line: &str) -> Result<()> {
        let rest = line
            .trim_start()
            .strip_prefix("/attach")
            .unwrap_or("")
            .trim();
        let words = split_quoted(rest)?;
        match words
            .iter()
            .map(String::as_str)
            .collect::<Vec<_>>()
            .as_slice()
        {
            [] | ["list"] => {
                if self.models.attachments.is_empty() {
                    print_text(
                        "没有附件；用 /attach <files...> 或在消息中写 @路径\n",
                        false,
                    )?;
                }
                for (index, path) in self.models.attachments.iter().enumerate() {
                    print_text(&format!("{}. {}\n", index + 1, path.display()), false)?;
                }
            }
            ["clear"] => {
                self.models.attachments.clear();
                print_text("附件已清空\n", false)?;
            }
            ["remove", number] => {
                let index = number
                    .parse::<usize>()
                    .ok()
                    .filter(|n| (1..=self.models.attachments.len()).contains(n))
                    .ok_or_else(|| {
                        Error::Session("没有这个编号的附件；/attach list 查看编号".into())
                    })?;
                let removed = self.models.attachments.remove(index - 1);
                print_text(&format!("已移除 {}\n", removed.display()), false)?;
            }
            paths => {
                let paths = match paths {
                    ["add", rest @ ..] => rest,
                    all => all,
                };
                if paths.is_empty() {
                    return Err(Error::Session("用法：/attach [add] <files...>".into()));
                }
                let mut added = Vec::new();
                for raw in paths {
                    let path = self.resolve_attachment(raw.trim_start_matches('@'));
                    if !path.is_file() {
                        return Err(Error::Session(format!(
                            "不是文件：{raw}；请检查路径（含空格的路径请加引号）"
                        )));
                    }
                    added.push(path);
                }
                if self.models.attachments.len() + added.len() > 20 {
                    return Err(Error::Session(
                        "每条消息最多 20 个附件；可用 /attach remove <n> 或 /attach clear 腾出位置"
                            .into(),
                    ));
                }
                for path in added {
                    print_text(
                        &format!("已附加 {}（随下一条消息发送）\n", path.display()),
                        false,
                    )?;
                    self.models.attachments.push(path);
                }
            }
        }
        Ok(())
    }

    fn resolve_attachment(&self, raw: &str) -> PathBuf {
        let path = PathBuf::from(raw);
        if path.is_absolute() {
            path
        } else {
            self.settings.cwd.join(path)
        }
    }

    /// `@path` tokens naming existing files plus staged /attach files, as blocks.
    /// The tokens leave the text; other `@words` stay. Staged files are consumed.
    pub(crate) fn take_attachments(&mut self, prompt: &str) -> Result<(String, Vec<Block>)> {
        let cwd = self.settings.cwd.clone();
        let (text, inline) = extract_inline(prompt, |raw| {
            let path = PathBuf::from(raw);
            let path = if path.is_absolute() {
                path
            } else {
                cwd.join(path)
            };
            path.is_file()
        });
        let mut paths: Vec<PathBuf> = std::mem::take(&mut self.models.attachments);
        paths.extend(inline.iter().map(|raw| self.resolve_attachment(raw)));
        let mut blocks = Vec::new();
        for path in &paths {
            blocks.push(crate::model::images::attachment(path)?);
        }
        if !paths.is_empty() {
            print_text(
                &format!("[attach] 本条消息附带 {} 个文件\n", paths.len()),
                true,
            )?;
        }
        let text = if text.trim().is_empty() && !blocks.is_empty() {
            "See the attached files.".to_owned()
        } else {
            text
        };
        Ok((text, blocks))
    }
}

async fn ask_key(prompt: &str) -> Result<String> {
    let prompt = prompt.to_owned();
    let key = tokio::task::spawn_blocking(move || crate::app::ui::prompt_line(&prompt, true))
        .await
        .map_err(|err| Error::Session(format!("读取输入失败：{err}")))??;
    let key = key.trim().to_owned();
    if key.is_empty() {
        return Err(Error::Session("没有输入密钥，未做任何修改".into()));
    }
    Ok(key)
}

/// Splits on whitespace, keeping "double" or 'single' quoted parts together.
fn split_quoted(input: &str) -> Result<Vec<String>> {
    let mut words = Vec::new();
    let mut current = String::new();
    let mut quote = None;
    let mut started = false;
    for c in input.chars() {
        match quote {
            Some(q) if c == q => quote = None,
            Some(_) => current.push(c),
            None if c == '"' || c == '\'' => {
                quote = Some(c);
                started = true;
            }
            None if c.is_whitespace() => {
                if started || !current.is_empty() {
                    words.push(std::mem::take(&mut current));
                    started = false;
                }
            }
            None => current.push(c),
        }
    }
    if quote.is_some() {
        return Err(Error::Session("引号没有闭合；请补上结尾的引号".into()));
    }
    if started || !current.is_empty() {
        words.push(current);
    }
    Ok(words)
}

/// Removes the `@path` tokens `wanted` accepts (an @ starting a word, optionally
/// quoted) with one adjacent space; line breaks and other text stay as typed.
fn extract_inline(input: &str, wanted: impl Fn(&str) -> bool) -> (String, Vec<String>) {
    static TOKEN: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let token = TOKEN.get_or_init(|| {
        regex::Regex::new(r#"(^|\s)@("[^"]+"|'[^']+'|\S+)"#).expect("valid token pattern")
    });
    let mut out = String::new();
    let mut paths = Vec::new();
    let mut last = 0;
    for captures in token.captures_iter(input) {
        let (Some(before), Some(raw)) = (captures.get(1), captures.get(2)) else {
            continue;
        };
        let path = raw.as_str().trim_matches(['"', '\'']);
        if path.is_empty() || !wanted(path) {
            continue;
        }
        let mut start = before.end();
        let mut end = raw.end();
        if matches!(before.as_str(), " " | "\t") && before.start() >= last {
            start = before.start();
        } else if input[end..].starts_with([' ', '\t']) {
            end += 1;
        }
        start = start.max(last);
        out.push_str(&input[last..start]);
        paths.push(path.to_owned());
        last = end;
    }
    if paths.is_empty() {
        return (input.to_owned(), paths);
    }
    out.push_str(&input[last..]);
    (out, paths)
}

const CONFIG_KEYS: &[&str] = &[
    "model",
    "provider",
    "api_key",
    "base_url",
    "system",
    "budget",
    "max_output_tokens",
];

pub(crate) async fn handle(
    app: &mut Application,
    line: &str,
    args: &[&str],
) -> Result<Option<bool>> {
    // Everything after `/config <key> ` verbatim (system prompts keep their spacing).
    let rest_after = |count: usize| -> Option<String> {
        let mut rest = line.trim_start();
        for _ in 0..count {
            rest = rest.trim_start();
            let end = rest.find(char::is_whitespace).unwrap_or(rest.len());
            rest = &rest[end..];
        }
        let rest = rest.trim();
        (!rest.is_empty()).then(|| rest.to_owned())
    };
    match args {
        ["/model"] => app.show_models()?,
        ["/model", "fast", value @ ..] => app.set_fast(&value.join(" ")).await?,
        ["/model", "provider", name] => app.set_provider(name, None).await?,
        ["/model", "provider", name, model] => app.set_provider(name, Some(model)).await?,
        ["/model", "profile", rest @ ..] => app.profile(rest).await?,
        ["/model", name] => app.set_model(name).await?,
        ["/config", "setup"] => app.setup().await?,
        ["/config", key, ..] if CONFIG_KEYS.contains(key) => {
            let value = rest_after(2);
            app.config_key(key, value.as_deref()).await?
        }
        ["/attach", ..] => app.attach(line)?,
        ["/status", "limits"] => {
            print_text(&app.model_router().rate_limits().report(), false)?;
        }
        _ => return Ok(None),
    }
    Ok(Some(false))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn inline_tokens_leave_the_text_and_keep_layout() {
        let exists = |raw: &str| raw.ends_with(".png") || raw == "my file.txt";
        let (text, paths) = extract_inline("look at @a.png please", exists);
        assert_eq!(
            (text.as_str(), paths),
            ("look at please", vec!["a.png".to_owned()])
        );
        let (text, paths) = extract_inline("@a.png @b.png\n  keep\tindent", exists);
        assert_eq!(text, "\n  keep\tindent");
        assert_eq!(paths.len(), 2);
        let (text, paths) = extract_inline("mail me@x.com and @Override @\"my file.txt\"", exists);
        assert_eq!(text, "mail me@x.com and @Override");
        assert_eq!(paths, vec!["my file.txt".to_owned()]);
    }

    #[test]
    fn quoting_redaction_and_model_names() {
        assert_eq!(
            split_quoted(r#"add "a b.png" c.txt 'd e'"#).unwrap(),
            ["add", "a b.png", "c.txt", "d e"]
        );
        assert!(split_quoted("\"open").is_err());
        assert_eq!(redact("sk-1234567890abcd"), "sk-1****abcd");
        assert_eq!(redact("short"), "****");
        assert_eq!(
            normalize_model("deepseek/deepseek-chat", "deepseek").unwrap(),
            "deepseek-chat"
        );
        assert_eq!(
            normalize_model("anthropic/claude-sonnet-4", "openrouter").unwrap(),
            "anthropic/claude-sonnet-4"
        );
        assert_eq!(normalize_model("auto", "openai").unwrap(), "gpt-4o");
        assert!(normalize_model("auto", "kimi").is_err());
        let value = redacted(
            json!({"provider":{"api_key":"sk-1234567890abcd","api_keys":["abcdefghijklmn"]}}),
        );
        assert_eq!(value["provider"]["api_key"], "sk-1****abcd");
        assert_eq!(value["provider"]["api_keys"][0], "abcd****klmn");
    }
}
