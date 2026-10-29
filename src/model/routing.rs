//! Model routing: a premium (main) model and an optional fast model on one
//! provider. Each task is scored once and routed; a struggling fast model is
//! escalated, an overloaded model falls back to the other once, and background
//! work (compaction, summaries) runs on the fast model.

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex, RwLock},
};

use futures_util::future::BoxFuture;
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Block, Event, ModelRequest, ModelResponse, Provider, Result, emit},
    model::models::{self, Prices},
    model::provider::{HttpProvider, RateLimits, http_status},
};

/// Why the agent makes a request.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Purpose {
    /// The task itself (and its wrap-up).
    Task,
    /// Compaction and other bookkeeping the user does not read directly.
    Background,
}

/// What the agent loop tells the router and asks of it.
pub trait ModelPolicy: Send + Sync {
    /// A new task starts; `budget_left` is the remaining share of the budget (0..1).
    fn begin_task(&self, prompt: &str, budget_left: Option<f64>);
    /// Model for the next request; `None` lets the provider use its default.
    fn model_for(&self, purpose: Purpose) -> Option<String>;
    /// The verification gate failed; returns a notice when the task moved models.
    fn verification_failed(&self) -> Option<String>;
    /// A tool batch finished; three all-failed batches in a row escalate.
    fn batch_finished(&self, all_failed: bool) -> Option<String>;
    fn end_task(&self, completed: bool);
    /// Prices of `model`; `None` when unknown.
    fn prices(&self, model: &str) -> Option<Prices>;
    /// The server named its real window in an overflow error.
    fn learn_window(&self, model: &str, window: usize);
}

const COMPLEX_KEYWORDS: &[&str] = &[
    "refactor",
    "architecture",
    "design",
    "migrate",
    "rewrite",
    "debug",
    "optimize",
    "performance",
    "security audit",
    "重构",
    "架构",
    "设计",
    "迁移",
    "重写",
    "调试",
    "优化",
    "性能",
    "安全审计",
];
const WEIGHT_KEYWORD: f64 = 0.45;
const WEIGHT_LENGTH: f64 = 0.20;
const WEIGHT_FILES: f64 = 0.15;
const WEIGHT_FAILURES: f64 = 0.10;
const WEIGHT_BUDGET: f64 = 0.10;
const THRESHOLD: f64 = 0.35;
const HARD_LENGTH: usize = 2000;
/// A follow-up this short stays on the premium model the previous task used.
const FOLLOW_UP_CHARS: usize = 40;
const FAILED_BATCHES: u32 = 3;
const OUTCOME_HISTORY: usize = 10;

/// Multi-factor score of a task prompt: `(premium, reason)`.
pub fn classify(prompt: &str, failure_rate: f64, budget_left: Option<f64>) -> (bool, String) {
    static FILES: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let length = prompt.chars().count();
    if length >= HARD_LENGTH {
        return (true, format!("length {length} >= {HARD_LENGTH}"));
    }
    let lower = prompt.to_lowercase();
    let mut score = 0.0;
    let mut reasons = Vec::new();
    if let Some(keyword) = COMPLEX_KEYWORDS.iter().find(|word| lower.contains(*word)) {
        score += WEIGHT_KEYWORD;
        reasons.push(format!("keyword({keyword})+{WEIGHT_KEYWORD:.2}"));
    }
    if length > 0 {
        let part = (length as f64 / 1000.0).min(1.0) * WEIGHT_LENGTH;
        score += part;
        reasons.push(format!("length({length})+{part:.2}"));
    }
    let files = FILES
        .get_or_init(|| {
            regex::Regex::new(
                r"[\w./\\-]+\.(go|py|js|ts|tsx|jsx|java|rs|c|cpp|h|hpp|rb|php|json|yaml|yml|md|sql)\b",
            )
            .expect("valid file pattern")
        })
        .find_iter(prompt)
        .count();
    if files > 0 {
        let part = (files as f64 / 3.0).min(1.0) * WEIGHT_FILES;
        score += part;
        reasons.push(format!("files({files})+{part:.2}"));
    }
    if failure_rate > 0.0 {
        let part = failure_rate.min(1.0) * WEIGHT_FAILURES;
        score += part;
        reasons.push(format!("fast_failures({failure_rate:.2})+{part:.2}"));
    }
    if let Some(left) = budget_left.filter(|left| *left < 1.0) {
        let part = (1.0 - left.max(0.0)) * WEIGHT_BUDGET;
        score -= part;
        reasons.push(format!("budget_left({left:.2})-{part:.2}"));
    }
    (
        score >= THRESHOLD,
        format!("score {score:.2} [{}]", reasons.join(", ")),
    )
}

#[derive(Default)]
struct State {
    main: String,
    fast: Option<String>,
    /// Explicit prices configured for the main model.
    configured_prices: Option<(f64, f64)>,
    task: Option<String>,
    routed_fast: bool,
    last: Option<String>,
    fell_back: bool,
    failed_batches: u32,
    fast_failures: VecDeque<bool>,
    announce: Option<String>,
}

impl State {
    /// The other configured model, if any.
    fn other(&self, model: &str) -> Option<String> {
        [self.fast.as_deref(), Some(self.main.as_str())]
            .into_iter()
            .flatten()
            .find(|candidate| *candidate != model)
            .map(str::to_owned)
    }

    fn fast_distinct(&self) -> Option<&str> {
        self.fast.as_deref().filter(|fast| *fast != self.main)
    }

    fn record_fast(&mut self, failed: bool) {
        if self.fast_failures.len() == OUTCOME_HISTORY {
            self.fast_failures.pop_front();
        }
        self.fast_failures.push_back(failed);
    }

    fn escalate(&mut self, why: &str) -> Option<String> {
        let fast = self.fast_distinct()?.to_owned();
        if self.task.as_deref() != Some(fast.as_str()) {
            return None;
        }
        if self.routed_fast {
            self.routed_fast = false;
            self.record_fast(true);
        }
        self.task = Some(self.main.clone());
        Some(format!(
            "{why}；本任务接下来改用 {}（不再用 {fast}）",
            self.main
        ))
    }
}

/// Routes requests between the main and fast model of one live-replaceable provider.
pub struct ModelRouter {
    inner: RwLock<Arc<HttpProvider>>,
    state: Mutex<State>,
    window_sink: Mutex<Option<WindowSink>>,
}

type WindowSink = Box<dyn Fn(&str, usize) + Send + Sync>;

fn has_images(request: &ModelRequest) -> bool {
    request
        .messages
        .iter()
        .flat_map(|message| &message.blocks)
        .any(|block| matches!(block, Block::Image { .. }))
}

/// Replaces image parts with a note for a model that cannot see them.
fn downgrade_images(request: &mut ModelRequest, model: &str) {
    for message in &mut request.messages {
        for block in &mut message.blocks {
            if matches!(block, Block::Image { .. }) {
                *block = Block::Text(format!(
                    "[image omitted: model {model} may not accept images; switch to a vision model to include it]"
                ));
            }
        }
    }
}

impl ModelRouter {
    pub fn new(
        provider: Arc<HttpProvider>,
        fast: Option<String>,
        configured_prices: Option<(f64, f64)>,
    ) -> Self {
        let state = State {
            main: provider.model().to_owned(),
            fast: fast.filter(|fast| !fast.trim().is_empty()),
            configured_prices,
            ..State::default()
        };
        Self {
            inner: RwLock::new(provider),
            state: Mutex::new(state),
            window_sink: Mutex::new(None),
        }
    }

    fn state(&self) -> std::sync::MutexGuard<'_, State> {
        self.state
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }

    fn provider(&self) -> Arc<HttpProvider> {
        self.inner
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .clone()
    }

    /// Live switch: a new provider (and main model), fast model and prices.
    pub fn replace(
        &self,
        provider: Arc<HttpProvider>,
        fast: Option<String>,
        configured_prices: Option<(f64, f64)>,
    ) {
        let main = provider.model().to_owned();
        *self
            .inner
            .write()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = provider;
        let mut state = self.state();
        state.main = main;
        state.fast = fast.filter(|fast| !fast.trim().is_empty());
        state.configured_prices = configured_prices;
        state.task = None;
        state.routed_fast = false;
        state.last = None;
    }

    pub fn main_model(&self) -> String {
        self.state().main.clone()
    }

    pub fn fast_model(&self) -> Option<String> {
        self.state().fast.clone()
    }

    /// Model the current or last task ran on.
    pub fn current_model(&self) -> String {
        let state = self.state();
        state
            .task
            .clone()
            .or_else(|| state.last.clone())
            .unwrap_or_else(|| state.main.clone())
    }

    pub fn rate_limits(&self) -> Arc<RateLimits> {
        self.provider().rate_limits()
    }

    /// Called with each learned window so it can be saved to the user config.
    pub fn on_window_learned(&self, sink: WindowSink) {
        *self
            .window_sink
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) = Some(sink);
    }

    /// A provider for cheap background calls: the fast model when configured.
    pub fn background(self: &Arc<Self>) -> Arc<dyn Provider> {
        Arc::new(Background(self.clone()))
    }
}

impl ModelPolicy for ModelRouter {
    fn begin_task(&self, prompt: &str, budget_left: Option<f64>) {
        let mut state = self.state();
        state.fell_back = false;
        state.failed_batches = 0;
        state.routed_fast = false;
        let Some(fast) = state.fast_distinct().map(str::to_owned) else {
            state.task = Some(state.main.clone());
            return;
        };
        let failure_rate = if state.fast_failures.is_empty() {
            0.0
        } else {
            state.fast_failures.iter().filter(|failed| **failed).count() as f64
                / state.fast_failures.len() as f64
        };
        let (premium, mut reason) = classify(prompt, failure_rate, budget_left);
        let mut model = if premium {
            state.main.clone()
        } else {
            fast.clone()
        };
        if model == fast
            && state.last.as_deref() == Some(state.main.as_str())
            && prompt.trim().chars().count() <= FOLLOW_UP_CHARS
        {
            model = state.main.clone();
            reason = "简短的追问沿用上一个高级模型".into();
        }
        state.routed_fast = model == fast;
        state.announce = Some(format!("[model] {model} ({reason})"));
        state.task = Some(model);
    }

    fn model_for(&self, purpose: Purpose) -> Option<String> {
        let state = self.state();
        Some(match purpose {
            Purpose::Task => state.task.clone().unwrap_or_else(|| state.main.clone()),
            Purpose::Background => state.fast.clone().unwrap_or_else(|| state.main.clone()),
        })
    }

    fn verification_failed(&self) -> Option<String> {
        self.state().escalate("验证失败")
    }

    fn batch_finished(&self, all_failed: bool) -> Option<String> {
        let mut state = self.state();
        if !all_failed {
            state.failed_batches = 0;
            return None;
        }
        state.failed_batches += 1;
        if state.failed_batches < FAILED_BATCHES {
            return None;
        }
        state.failed_batches = 0;
        state.escalate("连续三批工具调用失败")
    }

    fn end_task(&self, completed: bool) {
        let mut state = self.state();
        if state.routed_fast {
            state.record_fast(!completed);
        }
        state.routed_fast = false;
        state.last = state.task.take();
    }

    fn prices(&self, model: &str) -> Option<Prices> {
        let state = self.state();
        let main = models::prices_for(&state.main, state.configured_prices);
        if model == state.main {
            return main;
        }
        // An unknown fast model is billed like the main one, so a budget stays a ceiling.
        models::price_card(model).or(main)
    }

    fn learn_window(&self, model: &str, window: usize) {
        if let Some(sink) = self
            .window_sink
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .as_ref()
        {
            sink(model, window);
        }
    }
}

impl Provider for ModelRouter {
    fn stream(
        &self,
        mut request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            let (mut model, announce, candidates) = {
                let mut state = self.state();
                let model = request
                    .model
                    .clone()
                    .or_else(|| state.task.clone())
                    .unwrap_or_else(|| state.main.clone());
                let candidates: Vec<String> = [state.fast.clone(), Some(state.main.clone())]
                    .into_iter()
                    .flatten()
                    .collect();
                (model, state.announce.take(), candidates)
            };
            if let Some(announce) = announce {
                emit(&events, &cancel, Event::Notice(announce)).await?;
            }
            let images = has_images(&request);
            if images && !models::vision_capable(&model) {
                match candidates
                    .iter()
                    .find(|candidate| models::vision_capable(candidate))
                {
                    Some(vision) => {
                        emit(
                            &events,
                            &cancel,
                            Event::Notice(format!("{model} 可能不支持图片；本次请求改用 {vision}")),
                        )
                        .await?;
                        model = vision.clone();
                    }
                    None => {
                        emit(
                            &events,
                            &cancel,
                            Event::Notice(format!(
                                "{model} 可能不支持图片，且没有配置视觉模型；图片已替换为文字说明"
                            )),
                        )
                        .await?;
                        downgrade_images(&mut request, &model);
                    }
                }
            }
            request.model = Some(model.clone());
            let provider = self.provider();
            let error = match provider
                .stream(request.clone(), events.clone(), cancel.clone())
                .await
            {
                Ok(response) => return Ok(response),
                Err(err) => err,
            };
            let fallback = {
                let mut state = self.state();
                let status = http_status(&error);
                let alternative = match status {
                    Some(404) if state.fast_distinct() == Some(model.as_str()) => {
                        Some((state.main.clone(), "不存在（请检查 model_fast）"))
                    }
                    Some(429 | 500..=599) if !state.fell_back => {
                        state.other(&model).map(|other| (other, "过载或被限流"))
                    }
                    _ => None,
                }
                .filter(|(other, _)| !images || models::vision_capable(other));
                if let Some((other, _)) = &alternative {
                    state.fell_back = true;
                    if state.task.as_deref() == Some(model.as_str()) {
                        state.task = Some(other.clone());
                    }
                }
                alternative
            };
            let Some((other, why)) = fallback else {
                return Err(error);
            };
            emit(
                &events,
                &cancel,
                Event::Notice(format!("模型 {model} {why}；改用 {other} 继续")),
            )
            .await?;
            request.model = Some(other);
            provider.stream(request, events, cancel).await
        })
    }
}

/// `ModelRouter::background`: requests without a model go to the fast model.
struct Background(Arc<ModelRouter>);

impl Provider for Background {
    fn stream(
        &self,
        mut request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        if request.model.is_none() {
            request.model = self.0.model_for(Purpose::Background);
        }
        self.0.stream(request, events, cancel)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Protocol;

    fn router(fast: Option<&str>) -> ModelRouter {
        let provider = HttpProvider::new(
            Protocol::Openai,
            "http://127.0.0.1:9/v1",
            "premium".into(),
            "key",
        )
        .unwrap();
        ModelRouter::new(Arc::new(provider), fast.map(str::to_owned), None)
    }

    #[test]
    fn classifier_weighs_keywords_length_files_failures_and_budget() {
        assert!(!classify("list the files", 0.0, None).0);
        assert!(classify("refactor the parser", 0.0, None).0);
        assert!(classify("重构解析器", 0.0, None).0);
        assert!(classify(&"x".repeat(2000), 0.0, None).0);
        // Three files (0.15) and a 1000-char message (0.20) together cross 0.35.
        let long = format!("touch a.rs b.rs c.rs {}", "y".repeat(1000));
        assert!(classify(&long, 0.0, None).0);
        // A tight budget pulls a borderline task back to the fast model.
        assert!(!classify(&long, 0.0, Some(0.0)).0);
        // Recent fast-model failures add to the score: 0.15 + 0.10 + 0.10 (keyword-free).
        assert!(!classify("update a.rs b.rs c.rs", 1.0, None).0);
        assert!(
            classify(
                &format!("update a.rs b.rs c.rs {}", "z".repeat(500)),
                1.0,
                None
            )
            .0
        );
    }

    #[test]
    fn routing_escalation_and_follow_ups() {
        let router = router(Some("fast"));
        router.begin_task("rename a variable", None);
        assert_eq!(router.model_for(Purpose::Task).as_deref(), Some("fast"));
        assert_eq!(
            router.model_for(Purpose::Background).as_deref(),
            Some("fast")
        );
        assert!(router.batch_finished(true).is_none());
        assert!(router.batch_finished(true).is_none());
        assert!(router.batch_finished(true).is_some());
        assert_eq!(router.model_for(Purpose::Task).as_deref(), Some("premium"));
        router.end_task(false);
        // The previous task ended on premium, so a terse follow-up stays there.
        router.begin_task("continue", None);
        assert_eq!(router.model_for(Purpose::Task).as_deref(), Some("premium"));
        router.end_task(true);
        router.begin_task("refactor everything", None);
        assert_eq!(router.model_for(Purpose::Task).as_deref(), Some("premium"));
        assert!(router.verification_failed().is_none());

        let single = self::router(None);
        single.begin_task("anything", None);
        assert_eq!(single.model_for(Purpose::Task).as_deref(), Some("premium"));
        assert_eq!(
            single.model_for(Purpose::Background).as_deref(),
            Some("premium")
        );
    }

    #[test]
    fn unknown_fast_model_is_billed_like_the_main_model() {
        let provider = HttpProvider::new(
            Protocol::Openai,
            "http://127.0.0.1:9/v1",
            "claude-sonnet-4-5".into(),
            "key",
        )
        .unwrap();
        let router = ModelRouter::new(Arc::new(provider), Some("local-fast".into()), None);
        assert_eq!(
            router.prices("local-fast"),
            router.prices("claude-sonnet-4-5")
        );
        assert_eq!(
            router.prices("claude-haiku-4-5").map(|p| p.input),
            Some(1.0)
        );
    }
}
