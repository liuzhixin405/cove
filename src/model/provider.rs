use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

use futures_util::{StreamExt, future::BoxFuture};
use reqwest::header::{HeaderMap, HeaderValue};
use serde_json::{Value, json};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    config::Protocol,
    domain::{
        Block, Error, Event, Message, ModelRequest, ModelResponse, Provider, Result, Role, Usage,
        emit,
    },
};

const FRAME_LIMIT: usize = 1024 * 1024;
const RESPONSE_LIMIT: usize = 8 * 1024 * 1024;
const RETRIES: u32 = 3;
/// Thinking models can be silent for minutes before the first token.
const IDLE_TIMEOUT: Duration = Duration::from_secs(180);
/// A rate-limited key rests this long when the server names no time.
const KEY_COOLDOWN: Duration = Duration::from_secs(60);
/// Anthropic betas sent to the first-party API (not Bedrock).
const ANTHROPIC_BETA: &str = "token-efficient-tools-2025-11-18,prompt-caching-2024-07-31";

/// Exponential from 1 s with jitter in [0.5, 1.5).
fn backoff(attempt: u32) -> Duration {
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |time| time.subsec_nanos());
    let jitter = 0.5 + f64::from(nanos % 1000) / 1000.0;
    Duration::from_secs_f64(f64::from(1_u32 << attempt.min(6)) * jitter)
}

/// `retry-after-ms`, `Retry-After` seconds, or Anthropic's reset timestamps.
fn retry_after(headers: &HeaderMap) -> Option<Duration> {
    let value = |name: &str| headers.get(name).and_then(|value| value.to_str().ok());
    if let Some(ms) = value("retry-after-ms").and_then(|ms| ms.trim().parse::<f64>().ok()) {
        return Some(Duration::from_secs_f64(ms.max(0.0) / 1000.0));
    }
    value("retry-after")
        .and_then(|seconds| seconds.trim().parse::<f64>().ok())
        .map(|seconds| Duration::from_secs_f64(seconds.max(0.0)))
}

/// The provider's own error message, which usually names the actual problem.
fn error_detail(body: &str) -> String {
    let message = serde_json::from_str::<Value>(body).ok().and_then(|value| {
        value
            .pointer("/error/message")
            .or_else(|| value.get("message"))
            .and_then(Value::as_str)
            .map(str::to_owned)
    });
    let text = message.unwrap_or_else(|| body.trim().to_owned());
    if text.is_empty() {
        String::new()
    } else {
        format!(": {}", text.chars().take(300).collect::<String>())
    }
}

/// HTTP status of a provider error produced by this module (`"HTTP 503 ..."`).
pub fn http_status(err: &Error) -> Option<u16> {
    match err {
        Error::Provider(message) => message.strip_prefix("HTTP ")?.get(..3)?.parse().ok(),
        _ => None,
    }
}

/// The server refused the prompt as larger than its context window.
pub fn context_overflow(err: &Error) -> bool {
    let Error::Provider(message) = err else {
        return false;
    };
    match http_status(err) {
        Some(413) => return true,
        Some(400) | None => {}
        Some(_) => return false,
    }
    let message = message.to_ascii_lowercase();
    [
        "context_length_exceeded",
        "maximum context length",
        "context length",
        "context window",
        "prompt is too long",
        "request_too_large",
        "input is too long",
        "too many input tokens",
        "exceed_context_size",
        "context size",
        "n_ctx",
    ]
    .iter()
    .any(|pattern| message.contains(pattern))
}

/// The window an overflow error names, most specific wording first.
pub fn reported_window(err: &Error) -> Option<usize> {
    static PATTERNS: std::sync::OnceLock<Vec<regex::Regex>> = std::sync::OnceLock::new();
    let Error::Provider(message) = err else {
        return None;
    };
    PATTERNS
        .get_or_init(|| {
            [
                r#"n_ctx"?\s*:\s*(\d+)"#,
                r"context size \((\d+) tokens\)",
                r"maximum context length is (\d+)",
                r"context length of only (\d+)",
                r"\d+\s*\+\s*\d+\s*>\s*(\d+)",
                r"(\d+) maximum",
            ]
            .iter()
            .filter_map(|pattern| regex::Regex::new(pattern).ok())
            .collect()
        })
        .iter()
        .find_map(|pattern| pattern.captures(message)?.get(1)?.as_str().parse().ok())
        .filter(|window| *window > 0)
}

/// Anthropic extended thinking, as the `thinking` config key selects it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Thinking {
    Adaptive,
    Disabled,
}

/// Request options beyond the wire protocol.
#[derive(Clone, Debug, Default)]
pub struct ProviderOptions {
    pub thinking: Option<Thinking>,
    /// Anthropic `output_config.effort` (low, medium, high, max).
    pub effort: Option<String>,
    /// Claude in Amazon Bedrock: same Messages API without first-party betas.
    pub bedrock: bool,
}

struct PoolKey {
    key: String,
    cool_until: Option<Instant>,
    dead: bool,
}

/// API keys used round-robin; a rate-limited key rests, a rejected key is dropped.
pub struct KeyPool {
    keys: Mutex<(Vec<PoolKey>, usize)>,
}

impl KeyPool {
    pub fn new(keys: Vec<String>) -> Self {
        let keys = keys
            .into_iter()
            .map(|key| key.trim().to_owned())
            .filter(|key| !key.is_empty())
            .map(|key| PoolKey {
                key,
                cool_until: None,
                dead: false,
            })
            .collect();
        Self {
            keys: Mutex::new((keys, 0)),
        }
    }

    pub fn len(&self) -> usize {
        self.keys.lock().map_or(0, |state| state.0.len())
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Next healthy key; when every live key rests, the one that recovers first.
    fn get(&self) -> Option<(usize, String)> {
        let mut state = self.keys.lock().ok()?;
        let (keys, next) = &mut *state;
        let now = Instant::now();
        for offset in 0..keys.len() {
            let index = (*next + offset) % keys.len();
            let key = &keys[index];
            if !key.dead && key.cool_until.is_none_or(|until| until <= now) {
                *next = (index + 1) % keys.len();
                return Some((index, key.key.clone()));
            }
        }
        keys.iter()
            .enumerate()
            .filter(|(_, key)| !key.dead)
            .min_by_key(|(_, key)| key.cool_until)
            .map(|(index, key)| (index, key.key.clone()))
    }

    /// A live key without advancing the rotation (for side requests such as listings).
    fn peek(&self) -> Option<String> {
        let state = self.keys.lock().ok()?;
        state
            .0
            .iter()
            .find(|key| !key.dead)
            .map(|key| key.key.clone())
    }

    /// Whether a live key other than `index` can be used right now.
    fn other_ready(&self, index: usize) -> bool {
        let now = Instant::now();
        self.keys.lock().is_ok_and(|state| {
            state.0.iter().enumerate().any(|(other, key)| {
                other != index && !key.dead && key.cool_until.is_none_or(|until| until <= now)
            })
        })
    }

    fn rate_limited(&self, index: usize, wait: Duration) {
        if let Ok(mut state) = self.keys.lock()
            && let Some(key) = state.0.get_mut(index)
        {
            key.cool_until = Some(Instant::now() + wait);
        }
    }

    /// Marks a rejected key dead; true when another live key remains.
    fn reject(&self, index: usize) -> bool {
        let Ok(mut state) = self.keys.lock() else {
            return false;
        };
        if let Some(key) = state.0.get_mut(index) {
            key.dead = true;
        }
        state.0.iter().any(|key| !key.dead)
    }
}

/// The latest rate-limit headers a provider returned.
#[derive(Clone, Debug, Default)]
pub struct RateInfo {
    pub requests_limit: Option<u64>,
    pub requests_remaining: Option<u64>,
    pub requests_reset: Option<Instant>,
    pub tokens_limit: Option<u64>,
    pub tokens_remaining: Option<u64>,
    pub tokens_reset: Option<Instant>,
    pub updated: Option<Instant>,
}

#[derive(Default)]
pub struct RateLimits {
    info: Mutex<RateInfo>,
}

/// `1m30s`, `6ms`, `20s`, plain seconds or an RFC 3339 instant.
fn reset_duration(value: &str) -> Option<Duration> {
    let value = value.trim();
    if let Ok(seconds) = value.parse::<f64>() {
        return Some(Duration::from_secs_f64(seconds.max(0.0)));
    }
    let mut total = 0.0;
    let mut number = String::new();
    let mut chars = value.chars().peekable();
    let mut parsed = false;
    while let Some(c) = chars.next() {
        if c.is_ascii_digit() || c == '.' {
            number.push(c);
            continue;
        }
        let amount: f64 = number.parse().ok()?;
        number.clear();
        let unit = match c {
            'h' => 3600.0,
            'm' if chars.peek() == Some(&'s') => {
                chars.next();
                0.001
            }
            'm' => 60.0,
            's' => 1.0,
            _ => return parse_instant(value),
        };
        total += amount * unit;
        parsed = true;
    }
    (parsed && number.is_empty()).then(|| Duration::from_secs_f64(total))
}

/// Seconds until an RFC 3339 UTC instant such as `2026-10-27T12:00:30Z`.
fn parse_instant(value: &str) -> Option<Duration> {
    let (date, time) = value.trim_end_matches('Z').split_once('T')?;
    let mut date = date.split('-').map(|part| part.parse::<i64>().ok());
    let (year, month, day) = (date.next()??, date.next()??, date.next()??);
    let time = time.split(['+', '-']).next()?;
    let mut time = time.split(':');
    let hour: i64 = time.next()?.parse().ok()?;
    let minute: i64 = time.next()?.parse().ok()?;
    let second: f64 = time.next()?.parse().ok()?;
    // Days from the civil date (Howard Hinnant's algorithm).
    let year = if month <= 2 { year - 1 } else { year };
    let era = year.div_euclid(400);
    let yoe = year - era * 400;
    let doy = (153 * (month + if month > 2 { -3 } else { 9 }) + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    let at = (days * 86_400 + hour * 3600 + minute * 60) as f64 + second;
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .ok()?
        .as_secs_f64();
    Some(Duration::from_secs_f64((at - now).max(0.0)))
}

impl RateLimits {
    pub fn update(&self, headers: &HeaderMap) {
        let text = |names: &[&str]| {
            names
                .iter()
                .find_map(|name| headers.get(*name)?.to_str().ok().map(str::to_owned))
        };
        let number = |names: &[&str]| text(names).and_then(|value| value.trim().parse().ok());
        let reset = |names: &[&str]| {
            text(names)
                .and_then(|value| reset_duration(&value))
                .map(|wait| Instant::now() + wait)
        };
        let info = RateInfo {
            requests_limit: number(&[
                "x-ratelimit-limit-requests",
                "anthropic-ratelimit-requests-limit",
            ]),
            requests_remaining: number(&[
                "x-ratelimit-remaining-requests",
                "anthropic-ratelimit-requests-remaining",
            ]),
            requests_reset: reset(&[
                "x-ratelimit-reset-requests",
                "anthropic-ratelimit-requests-reset",
            ]),
            tokens_limit: number(&[
                "x-ratelimit-limit-tokens",
                "anthropic-ratelimit-tokens-limit",
            ]),
            tokens_remaining: number(&[
                "x-ratelimit-remaining-tokens",
                "anthropic-ratelimit-tokens-remaining",
            ]),
            tokens_reset: reset(&[
                "x-ratelimit-reset-tokens",
                "anthropic-ratelimit-tokens-reset",
            ]),
            updated: Some(Instant::now()),
        };
        if (info.requests_limit.is_some() || info.tokens_limit.is_some())
            && let Ok(mut current) = self.info.lock()
        {
            *current = info;
        }
    }

    pub fn snapshot(&self) -> RateInfo {
        self.info
            .lock()
            .map(|info| info.clone())
            .unwrap_or_default()
    }

    /// Human-readable summary for `/status limits`.
    pub fn report(&self) -> String {
        let info = self.snapshot();
        let Some(updated) = info.updated else {
            return "rate limits: the provider has not reported any rate-limit headers yet\n"
                .into();
        };
        let now = Instant::now();
        let line =
            |label: &str, limit: Option<u64>, remaining: Option<u64>, reset: Option<Instant>| {
                let reset = match reset {
                    Some(at) if at > now => format!(", resets in {}s", (at - now).as_secs()),
                    Some(_) => ", reset passed".into(),
                    None => String::new(),
                };
                format!(
                    "{label}: {}/{} remaining{reset}\n",
                    remaining.map_or("?".into(), |n| n.to_string()),
                    limit.map_or("?".into(), |n| n.to_string())
                )
            };
        format!(
            "rate limits (reported {}s ago)\n{}{}",
            now.duration_since(updated).as_secs(),
            line(
                "requests",
                info.requests_limit,
                info.requests_remaining,
                info.requests_reset
            ),
            line(
                "tokens",
                info.tokens_limit,
                info.tokens_remaining,
                info.tokens_reset
            )
        )
    }
}

pub struct HttpProvider {
    client: reqwest::Client,
    endpoint: String,
    /// Bedrock Runtime (`InvokeModelWithResponseStream`): the API root and its region.
    runtime: Option<(String, String)>,
    /// Bedrock inference profile IDs, listed once to resolve short model names.
    profiles: tokio::sync::OnceCell<Vec<String>>,
    model: String,
    protocol: Protocol,
    keys: KeyPool,
    options: ProviderOptions,
    limits: Arc<RateLimits>,
}

impl HttpProvider {
    pub fn new(protocol: Protocol, base_url: &str, model: String, api_key: &str) -> Result<Self> {
        Self::with_options(
            protocol,
            base_url,
            model,
            vec![api_key.to_owned()],
            ProviderOptions::default(),
        )
    }

    /// `keys` rotate round-robin; at least one is required.
    pub fn with_options(
        protocol: Protocol,
        base_url: &str,
        model: String,
        keys: Vec<String>,
        options: ProviderOptions,
    ) -> Result<Self> {
        let base = url::Url::parse(base_url)
            .map_err(|_| Error::Provider("invalid API root URL".into()))?;
        if !base.username().is_empty()
            || base.password().is_some()
            || base.query().is_some()
            || base.fragment().is_some()
        {
            return Err(Error::Provider(
                "API root cannot contain credentials, query or fragment".into(),
            ));
        }
        let loopback = matches!(base.host_str(), Some("localhost" | "127.0.0.1" | "[::1]"));
        if base.scheme() != "https" && !(base.scheme() == "http" && loopback) {
            return Err(Error::Provider(
                "HTTPS required (HTTP only for explicit loopback API roots)".into(),
            ));
        }
        let keys = KeyPool::new(keys);
        if keys.is_empty() {
            return Err(Error::Provider("an API key is required".into()));
        }
        // Claude in Amazon Bedrock through the native runtime API (what Claude Code uses):
        // account-based access, no first-party region check on the caller's address.
        let runtime = base
            .host_str()
            .filter(|host| options.bedrock && host.starts_with("bedrock-runtime."))
            .map(|host| {
                let region = host
                    .trim_start_matches("bedrock-runtime.")
                    .split('.')
                    .next()
                    .unwrap_or("us-east-1")
                    .to_owned();
                (format!("{}://{host}", base.scheme()), region)
            });
        // Every key must form a valid header before anything is sent.
        if let Ok(state) = keys.keys.lock() {
            for key in &state.0 {
                Self::key_header(protocol, runtime.is_some(), &key.key)?;
            }
        }
        let mut headers = HeaderMap::new();
        if matches!(protocol, Protocol::Anthropic) && runtime.is_none() {
            headers.insert("anthropic-version", HeaderValue::from_static("2023-06-01"));
            if !options.bedrock {
                headers.insert("anthropic-beta", HeaderValue::from_static(ANTHROPIC_BETA));
            }
        }
        let client = crate::model::net::apply(reqwest::Client::builder())
            .default_headers(headers)
            .redirect(reqwest::redirect::Policy::none())
            .connect_timeout(Duration::from_secs(10))
            .build()
            .map_err(|_| Error::Provider("cannot initialize HTTP client".into()))?;
        let suffix = match protocol {
            Protocol::Openai => "chat/completions",
            Protocol::Anthropic => "messages",
        };
        Ok(Self {
            client,
            endpoint: format!("{}/{suffix}", base_url.trim_end_matches('/')),
            runtime,
            profiles: tokio::sync::OnceCell::new(),
            model,
            protocol,
            keys,
            options,
            limits: Arc::new(RateLimits::default()),
        })
    }

    fn key_header(
        protocol: Protocol,
        runtime: bool,
        key: &str,
    ) -> Result<(&'static str, HeaderValue)> {
        let (name, value) = match protocol {
            // Bedrock API keys are bearer tokens on the runtime API.
            Protocol::Anthropic if runtime => ("authorization", format!("Bearer {key}")),
            Protocol::Openai => ("authorization", format!("Bearer {key}")),
            Protocol::Anthropic => ("x-api-key", key.to_owned()),
        };
        let mut value = HeaderValue::from_str(&value)
            .map_err(|_| Error::Provider("invalid API key header".into()))?;
        value.set_sensitive(true);
        Ok((name, value))
    }

    /// Default model, used when a request names none.
    pub fn model(&self) -> &str {
        &self.model
    }

    pub fn rate_limits(&self) -> Arc<RateLimits> {
        self.limits.clone()
    }

    fn body(&self, request: ModelRequest) -> Value {
        let model = request.model.clone().unwrap_or_else(|| self.model.clone());
        let mut body = build_body(self.protocol, &self.options, &model, request);
        if self.runtime.is_some() {
            // InvokeModel takes the model in the path and the version in the body.
            if let Some(object) = body.as_object_mut() {
                object.remove("model");
                object.remove("stream");
                object.insert("anthropic_version".into(), json!("bedrock-2023-05-31"));
                if model.ends_with("[1m]") {
                    object.insert("anthropic_beta".into(), json!(["context-1m-2025-08-07"]));
                }
            }
        }
        body
    }

    /// Endpoint for one request: fixed, or per model on Bedrock Runtime.
    fn endpoint_for(&self, model: &str) -> String {
        match &self.runtime {
            Some((root, region)) => {
                let id = bedrock_model_id(model, region);
                Self::runtime_endpoint(root, &id)
            }
            None => self.endpoint.clone(),
        }
    }

    fn runtime_endpoint(root: &str, id: &str) -> String {
        let encoded: String = url::form_urlencoded::byte_serialize(id.as_bytes()).collect();
        format!("{root}/model/{encoded}/invoke-with-response-stream")
    }

    /// The endpoint with the model resolved against the account's inference profiles,
    /// so `anthropic.claude-haiku-4-5` reaches `us.anthropic.claude-haiku-4-5-20251001-v1:0`.
    async fn resolved_endpoint(&self, model: &str, key: &str) -> String {
        let Some((root, region)) = &self.runtime else {
            return self.endpoint.clone();
        };
        let profiles = self
            .profiles
            .get_or_init(|| self.list_profiles(region, key))
            .await;
        let id = resolve_bedrock_model(model, region, profiles)
            .unwrap_or_else(|| bedrock_model_id(model, region));
        Self::runtime_endpoint(root, &id)
    }

    /// System-defined inference profiles; empty when the listing is not allowed.
    async fn list_profiles(&self, region: &str, key: &str) -> Vec<String> {
        let url = format!(
            "https://bedrock.{region}.amazonaws.com/inference-profiles?maxResults=1000&typeEquals=SYSTEM_DEFINED"
        );
        let Ok(response) = self
            .client
            .get(url)
            .bearer_auth(key)
            .timeout(Duration::from_secs(15))
            .send()
            .await
        else {
            return Vec::new();
        };
        let Ok(value) = response.json::<Value>().await else {
            return Vec::new();
        };
        value["inferenceProfileSummaries"]
            .as_array()
            .into_iter()
            .flatten()
            .filter_map(|profile| profile["inferenceProfileId"].as_str().map(str::to_owned))
            .collect()
    }
}

/// Bedrock model ID for the runtime API: a bare `anthropic.*` ID gets the cross-region
/// inference profile of the region (`us.`, `eu.`, `apac.`), which newer Claude models
/// require; Claude Code's `[1m]` suffix (1M-context beta) is not part of the ID.
pub fn bedrock_model_id(model: &str, region: &str) -> String {
    let model = model.trim().trim_end_matches("[1m]");
    if !model.starts_with("anthropic.") {
        return model.to_owned();
    }
    let geo = match region.split('-').next().unwrap_or("us") {
        "eu" => "eu",
        "ap" => "apac",
        _ => "us",
    };
    format!("{geo}.{model}")
}

/// The inference profile `model` names: an exact ID, or the profile whose ID continues
/// the short name with a date or version (`-20251001-v1:0`, `-v1`), preferring the
/// region's geography over `global.`.
pub fn resolve_bedrock_model(model: &str, region: &str, profiles: &[String]) -> Option<String> {
    let model = model.trim().trim_end_matches("[1m]");
    if profiles.iter().any(|id| id == model) {
        return Some(model.to_owned());
    }
    let strip = |id: &str| -> (String, String) {
        match id.split_once('.') {
            Some((geo, rest))
                if ["us", "eu", "apac", "global", "jp", "au", "ca"].contains(&geo) =>
            {
                (geo.to_owned(), rest.to_owned())
            }
            _ => (String::new(), id.to_owned()),
        }
    };
    let (wanted_geo, base) = strip(model);
    let region_geo = bedrock_model_id("anthropic.x", region)
        .split('.')
        .next()
        .unwrap_or("us")
        .to_owned();
    let mut best: Option<(u8, &String)> = None;
    for id in profiles {
        let (geo, rest) = strip(id);
        let Some(tail) = rest.strip_prefix(base.as_str()) else {
            continue;
        };
        if !(tail.is_empty()
            || tail.starts_with("-2")
            || tail.starts_with("-v")
            || tail.starts_with(':'))
        {
            continue;
        }
        let rank = if !wanted_geo.is_empty() {
            if geo == wanted_geo { 0 } else { continue }
        } else if geo == region_geo {
            0
        } else if geo == "global" {
            1
        } else {
            2
        };
        if best.is_none_or(|(current, _)| rank < current) {
            best = Some((rank, id));
        }
    }
    best.map(|(_, id)| id.clone())
}

/// Image parts as OpenAI `image_url` data URLs.
fn openai_image(media_type: &str, data: &str) -> Value {
    json!({"type":"image_url","image_url":{"url":format!("data:{media_type};base64,{data}")}})
}

fn build_body(
    protocol: Protocol,
    options: &ProviderOptions,
    model: &str,
    request: ModelRequest,
) -> Value {
    match protocol {
        Protocol::Openai => {
            let mut messages = vec![json!({"role":"system","content":request.system})];
            for message in request.messages {
                let mut text = String::new();
                let mut reasoning = String::new();
                let mut calls = Vec::new();
                let mut results = Vec::new();
                let mut images = Vec::new();
                for block in message.blocks {
                    match block {
                        Block::Text(part) => text.push_str(&part),
                        Block::Reasoning(part) => reasoning.push_str(&part),
                        Block::ToolUse {
                            id,
                            name,
                            input,
                            extra,
                        } => {
                            let mut call = json!({"id":id,"type":"function","function":{"name":name,"arguments":input.to_string()}});
                            if let Some(extra) = extra {
                                call["extra_content"] = extra;
                            }
                            calls.push(call);
                        }
                        Block::ToolResult { id, content, .. } => {
                            results.push(json!({"role":"tool","tool_call_id":id,"content":content}))
                        }
                        Block::Image {
                            media_type,
                            data_base64,
                        } => images.push(openai_image(&media_type, &data_base64)),
                        Block::Thinking(_) => {}
                    }
                }
                if message.role == Role::Assistant {
                    let mut wire = json!({"role":"assistant","content":text});
                    if !calls.is_empty() {
                        wire["tool_calls"] = json!(calls);
                        // DeepSeek requires the reasoning of a tool-calling turn back;
                        // elsewhere it only costs input tokens.
                        if !reasoning.is_empty() {
                            wire["reasoning_content"] = json!(reasoning);
                        }
                    }
                    messages.push(wire);
                    continue;
                }
                // Tool messages must directly follow the assistant's calls.
                let had_results = !results.is_empty();
                messages.extend(results);
                if images.is_empty() {
                    if !text.is_empty() || !had_results {
                        messages.push(json!({"role":"user","content":text}));
                    }
                } else {
                    let mut parts = Vec::new();
                    if !text.is_empty() {
                        parts.push(json!({"type":"text","text":text}));
                    } else if had_results {
                        parts.push(json!({"type":"text","text":"Images returned by the tool results above:"}));
                    }
                    parts.extend(images);
                    messages.push(json!({"role":"user","content":parts}));
                }
            }
            let tools: Vec<_> = request.tools.into_iter().map(|tool| json!({
                "type":"function","function":{"name":tool.name,"description":tool.description,"parameters":tool.schema}
            })).collect();
            let mut body = json!({"model":model,"messages":messages,"stream":true,
                "stream_options":{"include_usage":true}});
            if !tools.is_empty() && !crate::model::models::tool_free_reasoner(model) {
                body["tools"] = json!(tools);
            }
            let limit = if crate::model::models::openai_reasoning(model) {
                "max_completion_tokens"
            } else {
                "max_tokens"
            };
            body[limit] = json!(request.max_output_tokens);
            body
        }
        Protocol::Anthropic => {
            let with_thinking = options.thinking != Some(Thinking::Disabled);
            let mut messages: Vec<Value> = request
                .messages
                .into_iter()
                .map(|message| {
                    let blocks: Vec<_> = message
                        .blocks
                        .into_iter()
                        .filter_map(|block| match block {
                            Block::Text(text) => Some(json!({"type":"text","text":text})),
                            Block::ToolUse { id, name, input, .. } => Some(
                                json!({"type":"tool_use","id":id,"name":name,"input":input}),
                            ),
                            Block::ToolResult {
                                id,
                                content,
                                is_error,
                            } => Some(
                                json!({"type":"tool_result","tool_use_id":id,"content":content,"is_error":is_error}),
                            ),
                            Block::Image {
                                media_type,
                                data_base64,
                            } => Some(
                                json!({"type":"image","source":{"type":"base64","media_type":media_type,"data":data_base64}}),
                            ),
                            Block::Thinking(wire) => with_thinking.then_some(wire),
                            Block::Reasoning(_) => None,
                        })
                        .collect();
                    json!({"role": if message.role == Role::User { "user" } else { "assistant" }, "content":blocks})
                })
                .collect();
            // Prompt cache: the system prompt and the last three messages; thinking
            // blocks cannot carry cache_control, so the last other block does.
            let mut marked = 0;
            for message in messages.iter_mut().rev() {
                if marked == 3 {
                    break;
                }
                if let Some(block) = message["content"].as_array_mut().and_then(|blocks| {
                    blocks.iter_mut().rev().find(|block| {
                        !matches!(
                            block["type"].as_str(),
                            Some("thinking" | "redacted_thinking")
                        )
                    })
                }) {
                    block["cache_control"] = json!({"type":"ephemeral"});
                    marked += 1;
                }
            }
            let tools: Vec<_> = request
                .tools
                .into_iter()
                .map(|tool| {
                    json!({
                        "name":tool.name,"description":tool.description,"input_schema":tool.schema
                    })
                })
                .collect();
            let mut body = json!({"model":model,
                "system":[{"type":"text","text":request.system,"cache_control":{"type":"ephemeral"}}],
                "messages":messages,"stream":true,"max_tokens":request.max_output_tokens});
            if !tools.is_empty() {
                body["tools"] = json!(tools);
            }
            match options.thinking {
                // Summarized: the reasoning is shown like other providers' reasoning.
                Some(Thinking::Adaptive) => {
                    body["thinking"] = json!({"type":"adaptive","display":"summarized"})
                }
                Some(Thinking::Disabled) => body["thinking"] = json!({"type":"disabled"}),
                None => {}
            }
            if let Some(effort) = &options.effort {
                body["output_config"] = json!({"effort":effort});
            }
            body
        }
    }
}

impl Provider for HttpProvider {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            let model = request.model.clone().unwrap_or_else(|| self.model.clone());
            let body = self.body(request);
            let endpoint = match self.runtime.as_ref().and_then(|_| self.keys.peek()) {
                Some(key) => self.resolved_endpoint(&model, &key).await,
                None => self.endpoint_for(&model),
            };
            let mut attempt = 0;
            let response = loop {
                let (key_index, key) = self.keys.get().ok_or_else(|| {
                    Error::Provider("every API key was rejected (401/403)".into())
                })?;
                let (header, value) =
                    Self::key_header(self.protocol, self.runtime.is_some(), &key)?;
                let sent = tokio::select! {
                    _ = cancel.cancelled() => return Err(Error::Cancelled),
                    response = self.client.post(&endpoint).header(header, value).json(&body).send() => response,
                };
                let (wait, reason) = match sent {
                    Ok(response) if response.status().is_success() => {
                        self.limits.update(response.headers());
                        break response;
                    }
                    Ok(response) => {
                        let status = response.status();
                        self.limits.update(response.headers());
                        // A rejected key is dropped and the next one tried at once.
                        if matches!(status.as_u16(), 401 | 403)
                            && self.keys.len() > 1
                            && self.keys.reject(key_index)
                        {
                            emit(
                                &events,
                                &cancel,
                                Event::Notice(format!(
                                    "API key #{} 被拒绝（HTTP {}）；正在尝试下一个 key",
                                    key_index + 1,
                                    status.as_u16()
                                )),
                            )
                            .await?;
                            continue;
                        }
                        let server_wait = retry_after(response.headers());
                        if status.as_u16() == 429 {
                            self.keys
                                .rate_limited(key_index, server_wait.unwrap_or(KEY_COOLDOWN));
                            if attempt < RETRIES && self.keys.other_ready(key_index) {
                                attempt += 1;
                                continue;
                            }
                        }
                        // 429 and 5xx are refused before generation; other statuses will not change.
                        if attempt >= RETRIES
                            || !(status.as_u16() == 429 || status.is_server_error())
                        {
                            let detail = response.text().await.unwrap_or_default();
                            return Err(Error::Provider(format!(
                                "HTTP {status}{}{}",
                                if attempt > 0 {
                                    format!(" after {attempt} retries")
                                } else {
                                    String::new()
                                },
                                error_detail(&detail)
                            )));
                        }
                        (server_wait, format!("HTTP {status}"))
                    }
                    // Only a failed connection is retried; a request that may have been
                    // received is never sent twice.
                    Err(err) if err.is_connect() && attempt < RETRIES => {
                        (None, "连接失败".to_owned())
                    }
                    Err(_) => {
                        return Err(Error::Provider("HTTP request failed (check endpoint, proxy, certificates and connectivity)".into()));
                    }
                };
                let wait = wait
                    .unwrap_or_else(|| backoff(attempt))
                    .min(Duration::from_secs(60));
                attempt += 1;
                if wait >= Duration::from_secs(5) {
                    emit(
                        &events,
                        &cancel,
                        Event::Notice(format!(
                            "{reason}；{}s 后第 {attempt}/{RETRIES} 次重试",
                            wait.as_secs()
                        )),
                    )
                    .await?;
                }
                tokio::select! {
                    _ = cancel.cancelled() => return Err(Error::Cancelled),
                    _ = tokio::time::sleep(wait) => {}
                }
            };
            let expected = if self.runtime.is_some() {
                "application/vnd.amazon.eventstream"
            } else {
                "text/event-stream"
            };
            if !response
                .headers()
                .get("content-type")
                .and_then(|v| v.to_str().ok())
                .is_some_and(|v| v.starts_with(expected))
            {
                return Err(Error::Provider(format!("expected {expected} response")));
            }
            let mut stream = response.bytes_stream();
            let mut decoder = SseDecoder::default();
            let mut frames = crate::model::eventstream::Decoder::default();
            let mut collector = Collector::new(self.protocol);
            let mut bytes = 0;
            loop {
                let chunk = tokio::select! {
                    _ = cancel.cancelled() => return Err(Error::Cancelled),
                    result = tokio::time::timeout(IDLE_TIMEOUT, stream.next()) =>
                        result.map_err(|_| Error::Provider("stream idle timeout; result may be incomplete".into()))?,
                };
                let Some(chunk) = chunk else {
                    break;
                };
                let chunk = chunk.map_err(|_| {
                    Error::Provider(
                        "stream disconnected; partial output is not a completed response".into(),
                    )
                })?;
                bytes += chunk.len();
                if bytes > RESPONSE_LIMIT {
                    return Err(Error::Provider("response exceeds 8 MiB".into()));
                }
                // Bedrock wraps each Anthropic stream event in an event-stream frame.
                let data = if self.runtime.is_some() {
                    let mut data = Vec::new();
                    for frame in frames.push(&chunk)? {
                        data.extend(crate::model::eventstream::chunk_json(&frame)?);
                    }
                    data
                } else {
                    decoder.push(&chunk)?
                };
                for data in data {
                    for event in collector.accept(&data)? {
                        emit(&events, &cancel, event).await?;
                    }
                }
                if collector.done {
                    let mut response = collector.finish()?;
                    response.model = Some(model);
                    return Ok(response);
                }
            }
            Err(Error::Provider(
                "stream ended without its terminal event; no tools were dispatched".into(),
            ))
        })
    }
}

#[derive(Default)]
struct SseDecoder {
    pending: Vec<u8>,
    data: Vec<u8>,
}

impl SseDecoder {
    fn push(&mut self, bytes: &[u8]) -> Result<Vec<String>> {
        self.pending.extend_from_slice(bytes);
        let mut frames = Vec::new();
        let mut consumed = 0;
        while let Some(end) = self.pending[consumed..]
            .iter()
            .position(|byte| *byte == b'\n')
        {
            let end = consumed + end;
            let line = self.pending[consumed..end]
                .strip_suffix(b"\r")
                .unwrap_or(&self.pending[consumed..end]);
            if line.is_empty() {
                if !self.data.is_empty() {
                    self.data.pop();
                    frames.push(
                        String::from_utf8(std::mem::take(&mut self.data))
                            .map_err(|_| Error::Provider("invalid UTF-8 in SSE frame".into()))?,
                    );
                }
            } else if let Some(data) = line.strip_prefix(b"data:") {
                let data = data.strip_prefix(b" ").unwrap_or(data);
                if self.data.len() + data.len() > FRAME_LIMIT {
                    return Err(Error::Provider("SSE frame exceeds 1 MiB".into()));
                }
                self.data.extend_from_slice(data);
                self.data.push(b'\n');
            }
            consumed = end + 1;
        }
        self.pending.drain(..consumed);
        if self.pending.len() > FRAME_LIMIT {
            return Err(Error::Provider("SSE line exceeds 1 MiB".into()));
        }
        Ok(frames)
    }
}

#[derive(Default)]
struct OpenTool {
    id: String,
    name: String,
    arguments: String,
    extra: Option<Value>,
}

enum AnthropicBlock {
    Text(String),
    Thinking(Value),
    Tool {
        id: String,
        name: String,
        input: Value,
        partial: String,
    },
}

struct Collector {
    protocol: Protocol,
    text: String,
    reasoning: String,
    tools: BTreeMap<u64, OpenTool>,
    blocks: BTreeMap<u64, AnthropicBlock>,
    usage: Usage,
    usage_reported: bool,
    stop: Option<String>,
    done: bool,
}

impl Collector {
    fn new(protocol: Protocol) -> Self {
        Self {
            protocol,
            text: String::new(),
            reasoning: String::new(),
            tools: BTreeMap::new(),
            blocks: BTreeMap::new(),
            usage: Usage::default(),
            usage_reported: false,
            stop: None,
            done: false,
        }
    }

    fn accept(&mut self, data: &str) -> Result<Vec<Event>> {
        if data == "[DONE]" && matches!(self.protocol, Protocol::Openai) {
            self.done = true;
            return Ok(Vec::new());
        }
        let value: Value = serde_json::from_str(data)?;
        if value.get("error").is_some() || value["type"] == "error" {
            return Err(Error::Provider("server reported a streaming error".into()));
        }
        let mut events = Vec::new();
        match self.protocol {
            Protocol::Openai => {
                if let Some(usage) = value.get("usage").filter(|v| v.is_object()) {
                    self.usage.input = usage["prompt_tokens"]
                        .as_u64()
                        .ok_or_else(|| Error::Provider("missing input usage".into()))?;
                    self.usage.output = usage["completion_tokens"]
                        .as_u64()
                        .ok_or_else(|| Error::Provider("missing output usage".into()))?;
                    // OpenAI reports cached prompt tokens in the details, DeepSeek as hits.
                    self.usage.cache_read = usage
                        .pointer("/prompt_tokens_details/cached_tokens")
                        .and_then(Value::as_u64)
                        .or_else(|| usage["prompt_cache_hit_tokens"].as_u64())
                        .unwrap_or(0)
                        .min(self.usage.input);
                    self.usage_reported = true;
                }
                if let Some(choice) = value["choices"]
                    .as_array()
                    .and_then(|choices| choices.first())
                {
                    let delta = &choice["delta"];
                    if let Some(text) = delta["content"].as_str() {
                        self.text.push_str(text);
                        events.push(Event::Text(text.into()));
                    }
                    if let Some(text) = delta["reasoning_content"].as_str() {
                        self.reasoning.push_str(text);
                        events.push(Event::Reasoning(text.into()));
                    }
                    if let Some(calls) = delta["tool_calls"].as_array() {
                        for call in calls {
                            let index = call["index"]
                                .as_u64()
                                .filter(|i| *i < 64)
                                .ok_or_else(|| Error::Provider("invalid tool index".into()))?;
                            let builder = self.tools.entry(index).or_default();
                            if let Some(id) = call["id"].as_str() {
                                builder.id.push_str(id);
                            }
                            if let Some(name) = call["function"]["name"].as_str() {
                                builder.name.push_str(name);
                            }
                            if let Some(args) = call["function"]["arguments"].as_str() {
                                builder.arguments.push_str(args);
                            }
                            if let Some(extra) = call.get("extra_content") {
                                builder.extra = Some(extra.clone());
                            }
                        }
                    }
                    if let Some(stop) = choice["finish_reason"].as_str() {
                        self.stop = Some(stop.into());
                    }
                }
            }
            Protocol::Anthropic => match value["type"].as_str().unwrap_or("") {
                "message_start" => {
                    let usage = &value["message"]["usage"];
                    // Anthropic counts cache reads and writes apart from input_tokens.
                    self.usage.cache_read = usage["cache_read_input_tokens"].as_u64().unwrap_or(0);
                    self.usage.cache_write =
                        usage["cache_creation_input_tokens"].as_u64().unwrap_or(0);
                    self.usage.input = usage["input_tokens"]
                        .as_u64()
                        .ok_or_else(|| Error::Provider("missing input usage".into()))?
                        + self.usage.cache_read
                        + self.usage.cache_write;
                    self.usage_reported = true;
                }
                "content_block_start" => {
                    let index = block_index(&value)?;
                    let block = &value["content_block"];
                    let pending = match block["type"].as_str() {
                        Some("text") => {
                            let text = block["text"].as_str().unwrap_or("").to_owned();
                            if !text.is_empty() {
                                events.push(Event::Text(text.clone()));
                            }
                            AnthropicBlock::Text(text)
                        }
                        Some("thinking" | "redacted_thinking") => {
                            AnthropicBlock::Thinking(block.clone())
                        }
                        Some("tool_use") => AnthropicBlock::Tool {
                            id: required_string(block, "id")?,
                            name: required_string(block, "name")?,
                            input: block["input"].clone(),
                            partial: String::new(),
                        },
                        _ => {
                            return Err(Error::Provider(
                                "unsupported Anthropic content block".into(),
                            ));
                        }
                    };
                    if self.blocks.insert(index, pending).is_some() {
                        return Err(Error::Provider("duplicate block index".into()));
                    }
                }
                "content_block_delta" => {
                    let index = block_index(&value)?;
                    let pending = self
                        .blocks
                        .get_mut(&index)
                        .ok_or_else(|| Error::Provider("delta without block start".into()))?;
                    let delta = &value["delta"];
                    match (pending, delta["type"].as_str()) {
                        (AnthropicBlock::Text(text), Some("text_delta")) => {
                            let part = required_string(delta, "text")?;
                            text.push_str(&part);
                            events.push(Event::Text(part));
                        }
                        (AnthropicBlock::Tool { partial, .. }, Some("input_json_delta")) => {
                            partial.push_str(&required_string(delta, "partial_json")?)
                        }
                        (AnthropicBlock::Thinking(wire), Some("thinking_delta")) => {
                            let part = required_string(delta, "thinking")?;
                            let mut text = wire["thinking"].as_str().unwrap_or("").to_owned();
                            text.push_str(&part);
                            wire["thinking"] = json!(text);
                            events.push(Event::Reasoning(part));
                        }
                        (AnthropicBlock::Thinking(wire), Some("signature_delta")) => {
                            let mut signature = wire["signature"].as_str().unwrap_or("").to_owned();
                            signature.push_str(&required_string(delta, "signature")?);
                            wire["signature"] = json!(signature);
                        }
                        _ => return Err(Error::Provider("unsupported content delta".into())),
                    }
                }
                "message_delta" => {
                    self.stop = value["delta"]["stop_reason"].as_str().map(str::to_owned);
                    self.usage.output = value["usage"]["output_tokens"]
                        .as_u64()
                        .ok_or_else(|| Error::Provider("missing output usage".into()))?;
                }
                "message_stop" => self.done = true,
                "content_block_stop" | "ping" => {}
                _ => return Err(Error::Provider("unknown Anthropic stream event".into())),
            },
        }
        Ok(events)
    }

    fn finish(self) -> Result<ModelResponse> {
        let complete = match (self.protocol, self.stop.as_deref()) {
            (Protocol::Openai, Some("stop" | "tool_calls")) => true,
            (Protocol::Anthropic, Some("end_turn" | "tool_use" | "stop_sequence")) => true,
            (_, Some("length" | "max_tokens")) => false,
            _ => return Err(Error::Provider("missing or unsupported stop reason".into())),
        };
        let mut blocks = Vec::new();
        if matches!(self.protocol, Protocol::Openai) {
            if !self.reasoning.is_empty() {
                blocks.push(Block::Reasoning(self.reasoning));
            }
            if !self.text.is_empty() {
                blocks.push(Block::Text(self.text));
            }
            for builder in self.tools.into_values() {
                if builder.id.is_empty() || builder.name.is_empty() {
                    return Err(Error::Provider("incomplete tool call".into()));
                }
                let input: Value = serde_json::from_str(&builder.arguments).map_err(|_| {
                    Error::Provider("invalid tool arguments; nothing dispatched".into())
                })?;
                if !input.is_object() {
                    return Err(Error::Provider("tool input must be an object".into()));
                }
                blocks.push(Block::ToolUse {
                    id: builder.id,
                    name: builder.name,
                    input,
                    extra: builder.extra,
                });
            }
        } else {
            for block in self.blocks.into_values() {
                blocks.push(match block {
                    AnthropicBlock::Text(text) => Block::Text(text),
                    AnthropicBlock::Thinking(wire) => Block::Thinking(wire),
                    AnthropicBlock::Tool {
                        id,
                        name,
                        input,
                        partial,
                    } => {
                        let input = if partial.is_empty() {
                            input
                        } else {
                            serde_json::from_str(&partial).map_err(|_| {
                                Error::Provider("invalid tool arguments; nothing dispatched".into())
                            })?
                        };
                        if !input.is_object() {
                            return Err(Error::Provider("tool input must be an object".into()));
                        }
                        Block::ToolUse {
                            id,
                            name,
                            input,
                            extra: None,
                        }
                    }
                });
            }
        }
        if blocks.is_empty() {
            return Err(Error::Provider("empty model response".into()));
        }
        Ok(ModelResponse {
            message: Message {
                role: Role::Assistant,
                blocks,
            },
            usage: self.usage,
            complete,
            usage_reported: self.usage_reported,
            model: None,
        })
    }
}

fn required_string(value: &Value, name: &str) -> Result<String> {
    value[name]
        .as_str()
        .map(str::to_owned)
        .ok_or_else(|| Error::Provider(format!("missing {name}")))
}

fn block_index(value: &Value) -> Result<u64> {
    value["index"]
        .as_u64()
        .filter(|index| *index < 128)
        .ok_or_else(|| Error::Provider("invalid block index".into()))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::ToolDefinition;

    #[test]
    fn short_bedrock_names_resolve_to_inference_profiles() {
        let profiles: Vec<String> = [
            "global.anthropic.claude-haiku-4-5-20251001-v1:0",
            "us.anthropic.claude-haiku-4-5-20251001-v1:0",
            "us.anthropic.claude-opus-5",
            "us.anthropic.claude-opus-5-5",
            "global.anthropic.claude-sonnet-4-6",
            "us.anthropic.claude-opus-4-6-v1",
        ]
        .map(String::from)
        .to_vec();
        let resolve = |model: &str| resolve_bedrock_model(model, "us-east-1", &profiles);
        assert_eq!(
            resolve("anthropic.claude-haiku-4-5").as_deref(),
            Some("us.anthropic.claude-haiku-4-5-20251001-v1:0")
        );
        assert_eq!(
            resolve("anthropic.claude-opus-5").as_deref(),
            Some("us.anthropic.claude-opus-5")
        );
        assert_eq!(
            resolve("us.anthropic.claude-opus-5-5[1m]").as_deref(),
            Some("us.anthropic.claude-opus-5-5")
        );
        assert_eq!(
            resolve("anthropic.claude-sonnet-4-6").as_deref(),
            Some("global.anthropic.claude-sonnet-4-6")
        );
        assert_eq!(
            resolve("anthropic.claude-opus-4-6").as_deref(),
            Some("us.anthropic.claude-opus-4-6-v1")
        );
        assert_eq!(resolve("anthropic.claude-unknown"), None);
    }

    #[test]
    fn bedrock_runtime_ids_endpoint_and_body() {
        assert_eq!(
            bedrock_model_id("anthropic.claude-opus-5-5", "us-east-1"),
            "us.anthropic.claude-opus-5-5"
        );
        assert_eq!(
            bedrock_model_id("us.anthropic.claude-opus-5-5[1m]", "us-east-1"),
            "us.anthropic.claude-opus-5-5"
        );
        assert_eq!(
            bedrock_model_id("anthropic.claude-haiku-4-5", "eu-west-1"),
            "eu.anthropic.claude-haiku-4-5"
        );
        let provider = HttpProvider::with_options(
            Protocol::Anthropic,
            "https://bedrock-runtime.us-east-1.amazonaws.com",
            "us.anthropic.claude-opus-5-5[1m]".into(),
            vec!["token".into()],
            ProviderOptions {
                bedrock: true,
                ..ProviderOptions::default()
            },
        )
        .unwrap();
        assert_eq!(
            provider.endpoint_for("us.anthropic.claude-opus-5-5[1m]"),
            "https://bedrock-runtime.us-east-1.amazonaws.com/model/us.anthropic.claude-opus-5-5/invoke-with-response-stream"
        );
        let body = provider.body(request(vec![Message::user("hi")]));
        assert!(body.get("model").is_none() && body.get("stream").is_none());
        assert_eq!(body["anthropic_version"], "bedrock-2023-05-31");
        assert_eq!(body["anthropic_beta"][0], "context-1m-2025-08-07");
        let (name, _) = HttpProvider::key_header(Protocol::Anthropic, true, "token").unwrap();
        assert_eq!(name, "authorization");
    }

    fn request(messages: Vec<Message>) -> ModelRequest {
        ModelRequest {
            system: "sys".into(),
            messages,
            tools: vec![ToolDefinition {
                name: "read".into(),
                description: "d".into(),
                schema: json!({"type":"object"}),
            }],
            max_output_tokens: 100,
            estimated_tokens: None,
            model: None,
        }
    }

    fn history() -> Vec<Message> {
        vec![
            Message::user("look"),
            Message {
                role: Role::Assistant,
                blocks: vec![
                    Block::Thinking(json!({"type":"thinking","thinking":"t","signature":"sig"})),
                    Block::Reasoning("why".into()),
                    Block::ToolUse {
                        id: "c1".into(),
                        name: "read".into(),
                        input: json!({"path":"a.png"}),
                        extra: None,
                    },
                ],
            },
            Message {
                role: Role::User,
                blocks: vec![
                    Block::ToolResult {
                        id: "c1".into(),
                        content: "image attached".into(),
                        is_error: false,
                    },
                    Block::Image {
                        media_type: "image/png".into(),
                        data_base64: "AAAA".into(),
                    },
                ],
            },
            Message {
                role: Role::Assistant,
                blocks: vec![Block::Reasoning("plain".into()), Block::Text("done".into())],
            },
        ]
    }

    #[test]
    fn openai_bodies_follow_model_families() {
        let options = ProviderOptions::default();
        let body = build_body(Protocol::Openai, &options, "o3-mini", request(history()));
        assert_eq!(body["max_completion_tokens"], 100);
        assert!(body.get("max_tokens").is_none());
        let messages = body["messages"].as_array().unwrap();
        // system, user, assistant(calls), tool, user(images), assistant
        assert_eq!(messages[2]["reasoning_content"], "why");
        assert_eq!(messages[3]["role"], "tool");
        assert_eq!(messages[4]["role"], "user");
        assert_eq!(
            messages[4]["content"][1]["image_url"]["url"],
            "data:image/png;base64,AAAA"
        );
        // Reasoning without tool calls is not echoed.
        assert!(messages[5].get("reasoning_content").is_none());
        assert!(body["tools"].is_array());

        let body = build_body(Protocol::Openai, &options, "gpt-4o", request(history()));
        assert_eq!(body["max_tokens"], 100);
        let body = build_body(
            Protocol::Openai,
            &options,
            "deepseek-reasoner",
            request(history()),
        );
        assert!(body.get("tools").is_none());
    }

    #[test]
    fn anthropic_bodies_cache_think_and_send_images() {
        let options = ProviderOptions {
            thinking: Some(Thinking::Adaptive),
            effort: Some("high".into()),
            bedrock: false,
        };
        let body = build_body(
            Protocol::Anthropic,
            &options,
            "claude-x",
            request(history()),
        );
        assert_eq!(body["system"][0]["cache_control"]["type"], "ephemeral");
        assert_eq!(body["thinking"]["type"], "adaptive");
        assert_eq!(body["output_config"]["effort"], "high");
        let messages = body["messages"].as_array().unwrap();
        let marked = messages
            .iter()
            .filter(|message| {
                message["content"]
                    .as_array()
                    .unwrap()
                    .iter()
                    .any(|block| block.get("cache_control").is_some())
            })
            .count();
        assert_eq!(marked, 3);
        assert!(messages[0]["content"][0].get("cache_control").is_none());
        // Thinking stays verbatim and never carries cache_control.
        assert_eq!(messages[1]["content"][0]["signature"], "sig");
        assert!(messages[1]["content"][0].get("cache_control").is_none());
        assert_eq!(messages[2]["content"][1]["source"]["data"], "AAAA");

        let disabled = ProviderOptions {
            thinking: Some(Thinking::Disabled),
            ..ProviderOptions::default()
        };
        let body = build_body(
            Protocol::Anthropic,
            &disabled,
            "claude-x",
            request(history()),
        );
        assert_eq!(body["thinking"]["type"], "disabled");
        assert_eq!(body["messages"][1]["content"][0]["type"], "tool_use");
    }

    #[test]
    fn cached_tokens_are_parsed_for_both_protocols() {
        let mut c = Collector::new(Protocol::Anthropic);
        c.accept(r#"{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":80,"cache_creation_input_tokens":5}}}"#).unwrap();
        assert_eq!(
            (c.usage.input, c.usage.cache_read, c.usage.cache_write),
            (95, 80, 5)
        );
        let mut c = Collector::new(Protocol::Openai);
        c.accept(r#"{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":64}}}"#).unwrap();
        assert_eq!(c.usage.cache_read, 64);
        let mut c = Collector::new(Protocol::Openai);
        c.accept(r#"{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":3,"prompt_cache_hit_tokens":30}}"#).unwrap();
        assert_eq!(c.usage.cache_read, 30);
    }

    #[test]
    fn key_pool_rotates_rests_and_drops_keys() {
        let pool = KeyPool::new(vec!["a".into(), " b ".into(), String::new()]);
        assert_eq!(pool.len(), 2);
        assert_eq!(pool.get().unwrap().1, "a");
        assert_eq!(pool.get().unwrap().1, "b");
        pool.rate_limited(0, Duration::from_secs(60));
        assert_eq!(pool.get().unwrap().1, "b");
        assert_eq!(pool.get().unwrap().1, "b");
        assert!(!pool.other_ready(1));
        assert!(pool.reject(1));
        // Only a resting key is left: it is used anyway rather than nothing.
        assert_eq!(pool.get().unwrap().1, "a");
        assert!(!pool.reject(0));
        assert!(pool.get().is_none());
    }

    #[test]
    fn rate_limit_headers_and_errors_are_understood() {
        assert_eq!(reset_duration("1m30s"), Some(Duration::from_secs(90)));
        assert_eq!(reset_duration("6ms"), Some(Duration::from_millis(6)));
        assert_eq!(reset_duration("20"), Some(Duration::from_secs(20)));
        assert_eq!(reset_duration("2000-01-01T00:00:00Z"), Some(Duration::ZERO));
        let limits = RateLimits::default();
        assert!(limits.report().contains("not reported"));
        let mut headers = HeaderMap::new();
        headers.insert(
            "x-ratelimit-limit-requests",
            HeaderValue::from_static("500"),
        );
        headers.insert(
            "x-ratelimit-remaining-requests",
            HeaderValue::from_static("499"),
        );
        headers.insert("x-ratelimit-reset-requests", HeaderValue::from_static("1m"));
        limits.update(&headers);
        let report = limits.report();
        assert!(
            report.contains("requests: 499/500 remaining, resets in"),
            "{report}"
        );

        let overflow = Error::Provider(
            "HTTP 400 Bad Request: This model's maximum context length is 65536 tokens".into(),
        );
        assert_eq!(http_status(&overflow), Some(400));
        assert!(context_overflow(&overflow));
        assert_eq!(reported_window(&overflow), Some(65536));
        let llama = Error::Provider("HTTP 400 Bad Request: {\"n_ctx\": 8192}".into());
        assert_eq!(reported_window(&llama), Some(8192));
        assert!(!context_overflow(&Error::Provider(
            "HTTP 503 Service Unavailable: context length".into()
        )));
        assert!(context_overflow(&Error::Provider(
            "HTTP 413 Payload Too Large".into()
        )));
    }

    #[test]
    fn sse_handles_every_byte_split_crlf_and_unicode() {
        let input = "event: message\r\ndata: {\"text\":\"中文\"}\r\n\r\ndata: [DONE]\n\n";
        let mut decoder = SseDecoder::default();
        let mut frames = Vec::new();
        for byte in input.as_bytes() {
            frames.extend(decoder.push(&[*byte]).unwrap());
        }
        assert_eq!(frames, vec!["{\"text\":\"中文\"}", "[DONE]"]);
    }

    #[test]
    fn incomplete_or_invalid_tools_are_not_accepted() {
        let mut collector = Collector::new(Protocol::Openai);
        collector.accept(r#"{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"shell","arguments":"{"}}]},"finish_reason":"tool_calls"}]}"#).unwrap();
        assert!(collector.finish().is_err());
    }

    #[test]
    fn thinking_signature_survives_streaming() {
        let mut c = Collector::new(Protocol::Anthropic);
        for frame in [
            r#"{"type":"message_start","message":{"usage":{"input_tokens":10}}}"#,
            r#"{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}"#,
            r#"{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}"#,
            r#"{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque"}}"#,
            r#"{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}"#,
            r#"{"type":"message_stop"}"#,
        ] {
            c.accept(frame).unwrap();
        }
        let result = c.finish().unwrap();
        assert!(
            matches!(&result.message.blocks[0], Block::Thinking(wire) if wire["signature"] == "opaque")
        );
    }
}
