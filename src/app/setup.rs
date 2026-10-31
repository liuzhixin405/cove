//! First-run setup: choose a provider, model and key, saved to the user config.

use std::path::{Path, PathBuf};

use serde_json::{Map, Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    app::ui::prompt_line,
    config::{Protocol, default_model, normalize_base_url, provider_defaults, provider_protocol},
    domain::{Error, Message, ModelRequest, Provider},
    model::provider::HttpProvider,
};

/// Offered in the wizard; any catalog name or alias can still be written to the config.
const CHOICES: &[(&str, &str)] = &[
    ("deepseek", "DeepSeek"),
    ("openai", "OpenAI"),
    ("anthropic", "Anthropic Claude"),
    ("glm", "智谱 GLM"),
    ("kimi", "月之暗面 Kimi"),
    ("qwen", "阿里云通义千问 Qwen (DashScope)"),
    ("doubao", "火山引擎豆包 Doubao (Ark)"),
    ("openrouter", "OpenRouter"),
    ("siliconflow", "硅基流动 SiliconFlow"),
    ("openai-compatible", "其他 OpenAI 兼容接口（本地或网关）"),
];

/// True when this process owns its console window, i.e. it was started by
/// double-clicking rather than from an existing terminal.
#[cfg(windows)]
pub fn own_console() -> bool {
    use windows_sys::Win32::System::Console::GetConsoleProcessList;
    let mut processes = [0_u32; 2];
    // The buffer is valid for its declared length.
    let count = unsafe { GetConsoleProcessList(processes.as_mut_ptr(), processes.len() as u32) };
    count == 1
}

#[cfg(not(windows))]
pub fn own_console() -> bool {
    false
}

/// Keeps a double-clicked window open so the message stays readable.
pub fn pause() {
    let _ = prompt_line("按 Enter 关闭此窗口。", false);
}

fn ask(prompt: &str, default: Option<&str>) -> anyhow::Result<String> {
    let prompt = match default {
        Some(default) if !default.is_empty() => format!("{prompt} [{default}]："),
        _ => format!("{prompt}："),
    };
    let answer = prompt_line(&prompt, false)?;
    let answer = answer.trim();
    Ok(if answer.is_empty() {
        default.unwrap_or_default().to_owned()
    } else {
        answer.to_owned()
    })
}

/// Asks for the project to work on when the start directory is just where the program lives.
pub fn ask_workspace(current: &Path) -> anyhow::Result<PathBuf> {
    eprintln!("\nCove 在一个项目目录里工作；文件工具不会改动目录之外的文件。");
    loop {
        let answer = ask("项目目录", Some(&current.display().to_string()))?;
        let path = PathBuf::from(answer.trim_matches('"'));
        if path.is_dir() {
            return Ok(path);
        }
        eprintln!(
            "不是已存在的目录：{}；请输入项目目录的完整路径",
            path.display()
        );
    }
}

/// One provider choice of the wizard.
struct Choice {
    name: &'static str,
    default_url: String,
    env: &'static [&'static str],
    base_url: String,
    model: String,
}

fn choose() -> anyhow::Result<Choice> {
    for (index, (_, label)) in CHOICES.iter().enumerate() {
        eprintln!("  {:>2}. {label}", index + 1);
    }
    let choice = loop {
        let answer = ask("选择供应商编号", Some("1"))?;
        match answer.parse::<usize>() {
            Ok(number) if (1..=CHOICES.len()).contains(&number) => break CHOICES[number - 1].0,
            _ => eprintln!("请输入 1 到 {} 之间的编号", CHOICES.len()),
        }
    };
    let (name, default_url, env) =
        provider_defaults(choice).ok_or_else(|| anyhow::anyhow!("未知的供应商 {choice}"))?;
    let custom = name == "openai-compatible";
    let base_url = loop {
        let url = ask(
            "API 地址 base URL（需要时包含 /v1）",
            (!custom).then_some(default_url.as_str()),
        )?;
        if url.starts_with("https://")
            || url.starts_with("http://127.0.0.1")
            || url.starts_with("http://localhost")
        {
            break url;
        }
        eprintln!("请使用 https:// 地址（只有 localhost/127.0.0.1 可以用 http）");
    };
    let model = loop {
        let model = ask("模型 ID", default_model(name).filter(|_| !custom))?;
        if !model.is_empty() {
            break model;
        }
        eprintln!("必须填写模型 ID（例如供应商文档里的模型名）");
    };
    Ok(Choice {
        name,
        default_url,
        env,
        base_url,
        model,
    })
}

/// The typed key; empty keeps a key already present in the environment.
fn ask_key(env: &[&str]) -> anyhow::Result<String> {
    let present = env
        .iter()
        .find(|name| std::env::var(name).is_ok_and(|key| !key.trim().is_empty()));
    Ok(match present {
        Some(variable) => {
            let key = prompt_line(
                &format!("API Key（直接按 Enter 则继续使用环境变量 ${variable}）："),
                true,
            )?;
            key.trim().to_owned()
        }
        None => loop {
            let key = prompt_line("API Key（输入内容不显示）：", true)?;
            let key = key.trim().to_owned();
            if !key.is_empty() {
                break key;
            }
            eprintln!("必须填写 API Key（本地服务可随便填，例如 local）");
        },
    })
}

/// How long the wizard waits for its verification request.
pub const VERIFY_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(20);

/// One tiny request ("reply ok") proving the root, model and key work together.
pub async fn verify(
    protocol: Protocol,
    base_url: &str,
    model: &str,
    key: &str,
    timeout: std::time::Duration,
) -> anyhow::Result<()> {
    let provider = HttpProvider::new(
        protocol,
        &normalize_base_url(base_url, protocol),
        model.to_owned(),
        key,
    )?;
    let (events, mut receiver) = tokio::sync::mpsc::channel(64);
    let drain = tokio::spawn(async move { while receiver.recv().await.is_some() {} });
    let request = ModelRequest {
        system: "Reply with the single word ok.".into(),
        messages: vec![Message::user("reply ok")],
        tools: Vec::new(),
        max_output_tokens: 256,
        estimated_tokens: None,
        model: None,
    };
    let result = tokio::time::timeout(
        timeout,
        provider.stream(request, events, CancellationToken::new()),
    )
    .await;
    drain.abort();
    match result {
        Err(_) => anyhow::bail!(
            "{} 秒内没有响应；请检查网络、代理和 base URL",
            timeout.as_secs()
        ),
        Ok(Ok(_)) => Ok(()),
        // The server accepted the key and model; only the tiny reply was unusual.
        Ok(Err(Error::Provider(message)))
            if message.contains("empty model response") || message.contains("stop reason") =>
        {
            Ok(())
        }
        Ok(Err(err)) => Err(err.into()),
    }
}

/// Interactive provider/model/key setup; verifies the choice with one request,
/// writes `config` and returns once it is saved.
pub async fn run(config: &Path, reason: &str) -> anyhow::Result<()> {
    eprintln!("\nCove 初始设置：{reason}");
    eprintln!(
        "设置会保存到 {}，以后每次启动都会使用。\n",
        config.display()
    );
    'provider: loop {
        let choice = choose()?;
        let mut key = ask_key(choice.env)?;
        loop {
            let effective = if key.is_empty() {
                choice
                    .env
                    .iter()
                    .find_map(|name| std::env::var(name).ok())
                    .unwrap_or_default()
            } else {
                key.clone()
            };
            eprintln!(
                "正在用一次简短请求检查 {}（最多 {} 秒）...",
                choice.model,
                VERIFY_TIMEOUT.as_secs()
            );
            match verify(
                provider_protocol(choice.name),
                &choice.base_url,
                &choice.model,
                &effective,
                VERIFY_TIMEOUT,
            )
            .await
            {
                Ok(()) => {
                    eprintln!("连接正常。");
                    break;
                }
                Err(err) => {
                    eprintln!("验证失败：{}", crate::app::ui::visible(&format!("{err:#}")));
                    let answer = ask(
                        "[r] 重试  [k] 重新输入 Key  [p] 换供应商  [s] 不验证直接保存",
                        Some("r"),
                    )?;
                    match answer.trim().to_ascii_lowercase().as_str() {
                        "k" => key = ask_key(choice.env)?,
                        "p" => continue 'provider,
                        "s" => break,
                        _ => {}
                    }
                }
            }
        }
        let mut provider = Map::new();
        provider.insert("name".into(), json!(choice.name));
        if choice.name == "openai-compatible" || choice.base_url != choice.default_url {
            provider.insert("base_url".into(), json!(choice.base_url));
        }
        if !key.is_empty() {
            provider.insert("api_key".into(), json!(key));
        }
        save(config, Value::Object(provider), &choice.model)?;
        eprintln!(
            "\n已保存。以后可编辑 {} 修改设置（参考 docs/config.example.json）。\n",
            config.display()
        );
        return Ok(());
    }
}

/// Updates the active profile (or the root) and keeps every other setting in the file.
fn save(path: &Path, provider: Value, model: &str) -> anyhow::Result<()> {
    let mut root = match std::fs::read(path) {
        Ok(data) => {
            let data = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data);
            serde_json::from_slice::<Value>(data)?
        }
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => json!({}),
        Err(err) => return Err(err.into()),
    };
    let active = root
        .get("active_profile")
        .and_then(Value::as_str)
        .map(|name| format!("/profiles/{}", name.replace('~', "~0").replace('/', "~1")));
    let target = match active {
        Some(pointer) if root.pointer(&pointer).is_some() => root
            .pointer_mut(&pointer)
            .ok_or_else(|| anyhow::anyhow!("配置中的 profile 已不存在，请重新运行设置"))?,
        _ => &mut root,
    };
    let object = target.as_object_mut().ok_or_else(|| {
        anyhow::anyhow!(
            "{} 的顶层必须是 JSON 对象；请修正或删除该文件后重新运行",
            path.display()
        )
    })?;
    object.insert("provider".into(), provider);
    object.insert("model".into(), json!(model));
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let mut temporary = tempfile::NamedTempFile::new_in(path.parent().unwrap_or(Path::new(".")))?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        temporary
            .as_file()
            .set_permissions(std::fs::Permissions::from_mode(0o600))?;
    }
    use std::io::Write;
    temporary.write_all(serde_json::to_string_pretty(&root)?.as_bytes())?;
    temporary.as_file().sync_all()?;
    crate::session::replace_with(temporary, path)?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Serves one canned HTTP response, then closes.
    async fn serve_once(response: &'static str) -> String {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let mut buffer = vec![0_u8; 65536];
            let _ = socket.read(&mut buffer).await;
            socket.write_all(response.as_bytes()).await.unwrap();
            socket.shutdown().await.unwrap();
        });
        format!("http://{address}/v1")
    }

    #[tokio::test]
    async fn verification_accepts_a_reply_and_reports_a_rejected_key() {
        let sse = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n";
        let url = serve_once(sse).await;
        verify(Protocol::Openai, &url, "m", "k", VERIFY_TIMEOUT)
            .await
            .unwrap();
        let denied = "HTTP/1.1 401 Unauthorized\r\nContent-Type: application/json\r\nContent-Length: 35\r\nConnection: close\r\n\r\n{\"error\":{\"message\":\"bad api key\"}}";
        let url = serve_once(denied).await;
        let err = verify(Protocol::Openai, &url, "m", "k", VERIFY_TIMEOUT)
            .await
            .unwrap_err();
        assert!(format!("{err:#}").contains("bad api key"), "{err:#}");
        // A server that never answers is reported after the timeout.
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let url = format!("http://{}/v1", listener.local_addr().unwrap());
        let err = verify(
            Protocol::Openai,
            &url,
            "m",
            "k",
            std::time::Duration::from_millis(300),
        )
        .await
        .unwrap_err();
        assert!(err.to_string().contains("没有响应"));
        drop(listener);
    }

    #[test]
    fn save_updates_the_active_profile_and_keeps_other_settings() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("config.json");
        std::fs::write(
            &path,
            json!({"active_profile":"work","max_iterations":9,"profiles":{"work":{"model":"old"}}})
                .to_string(),
        )
        .unwrap();
        save(&path, json!({"name":"kimi","api_key":"k"}), "kimi-k2").unwrap();
        let saved: Value = serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        assert_eq!(saved["max_iterations"], 9);
        assert_eq!(saved["profiles"]["work"]["model"], "kimi-k2");
        assert_eq!(saved["profiles"]["work"]["provider"]["name"], "kimi");
        assert!(saved.get("model").is_none());

        let fresh = dir.path().join("new/config.json");
        save(&fresh, json!({"name":"deepseek"}), "deepseek-v4-pro").unwrap();
        let saved: Value = serde_json::from_slice(&std::fs::read(&fresh).unwrap()).unwrap();
        assert_eq!(saved["model"], "deepseek-v4-pro");
    }
}
