//! Outbound proxy policy shared by every HTTP client (models, web tools, MCP, embeddings).
//!
//! Default: the system proxy (Windows Internet Settings / macOS network settings, as set
//! by VPN clients in "system proxy" mode) and the `HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY`
//! environment variables. `proxy` in the config, `--proxy` or `COVE_PROXY` override it
//! with an explicit URL (`http://`, `https://`, `socks5://`, `socks5h://`) or `none`.

use std::sync::OnceLock;

use reqwest::{ClientBuilder, NoProxy, Proxy};

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ProxySetting {
    /// System proxy and proxy environment variables.
    System,
    /// Direct connections only.
    Disabled,
    Url(String),
}

static SETTING: OnceLock<ProxySetting> = OnceLock::new();

/// Loopback stays direct (local model servers), plus whatever `NO_PROXY` lists.
const ALWAYS_DIRECT: &str = "localhost,127.0.0.1,::1";

pub fn parse(raw: &str) -> Result<ProxySetting, String> {
    let value = raw.trim();
    match value.to_ascii_lowercase().as_str() {
        "" | "system" | "auto" => Ok(ProxySetting::System),
        "none" | "off" | "direct" | "false" => Ok(ProxySetting::Disabled),
        _ => {
            let url = url::Url::parse(value).map_err(|err| format!("invalid proxy URL: {err}"))?;
            if !matches!(url.scheme(), "http" | "https" | "socks5" | "socks5h") {
                return Err("proxy must be http://, https://, socks5:// or socks5h://".into());
            }
            if url.host_str().is_none() {
                return Err("proxy URL needs a host".into());
            }
            Ok(ProxySetting::Url(value.to_owned()))
        }
    }
}

/// Set once at startup; later calls are ignored.
pub fn configure(setting: ProxySetting) {
    let _ = SETTING.set(setting);
}

pub fn setting() -> &'static ProxySetting {
    SETTING.get_or_init(|| ProxySetting::System)
}

/// The proxy URL without credentials, for status output.
pub fn describe() -> String {
    match setting() {
        ProxySetting::System => "system (Internet settings / HTTPS_PROXY)".into(),
        ProxySetting::Disabled => "none".into(),
        ProxySetting::Url(raw) => match url::Url::parse(raw) {
            Ok(mut url) => {
                let _ = url.set_username("");
                let _ = url.set_password(None);
                url.to_string()
            }
            Err(_) => "explicit".into(),
        },
    }
}

fn explicit(url: &str) -> Option<Proxy> {
    let no_proxy = std::env::var("NO_PROXY")
        .or_else(|_| std::env::var("no_proxy"))
        .map(|list| format!("{list},{ALWAYS_DIRECT}"))
        .unwrap_or_else(|_| ALWAYS_DIRECT.to_owned());
    Proxy::all(url)
        .ok()
        .map(|proxy| proxy.no_proxy(NoProxy::from_string(&no_proxy)))
}

/// Applies the proxy policy to a client builder.
pub fn apply(builder: ClientBuilder) -> ClientBuilder {
    match setting() {
        // reqwest's own default: system proxy + environment variables.
        ProxySetting::System => builder,
        ProxySetting::Disabled => builder.no_proxy(),
        ProxySetting::Url(url) => match explicit(url) {
            Some(proxy) => builder.proxy(proxy),
            None => builder,
        },
    }
}

/// For `web_fetch`, whose direct connections are pinned to a vetted public address:
/// only an explicit proxy is used (the target host is still vetted before the request).
pub fn apply_explicit_only(builder: ClientBuilder) -> (ClientBuilder, bool) {
    match setting() {
        ProxySetting::Url(url) => match explicit(url) {
            Some(proxy) => (builder.proxy(proxy), true),
            None => (builder.no_proxy(), false),
        },
        _ => (builder.no_proxy(), false),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_proxy_settings() {
        assert_eq!(parse("").unwrap(), ProxySetting::System);
        assert_eq!(parse("None").unwrap(), ProxySetting::Disabled);
        assert_eq!(
            parse("socks5h://127.0.0.1:7891").unwrap(),
            ProxySetting::Url("socks5h://127.0.0.1:7891".into())
        );
        assert!(parse("ftp://x").is_err());
        assert!(parse("not a url").is_err());
    }
}
