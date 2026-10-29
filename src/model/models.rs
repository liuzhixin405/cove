//! Per-model facts: context window, output cap, list prices and capabilities
//! (built-in tables).

/// Substring match, first hit wins; more specific patterns come first.
const WINDOWS: &[(&str, usize)] = &[
    ("claude-fable", 1_000_000),
    ("claude-mythos", 1_000_000),
    ("claude-opus-5", 1_000_000),
    ("claude-opus-4-8", 1_000_000),
    ("claude-opus-4-7", 1_000_000),
    ("claude-opus-4-6", 1_000_000),
    ("claude-sonnet-5", 1_000_000),
    ("claude-sonnet-4-6", 1_000_000),
    ("claude-opus", 200_000),
    ("claude-sonnet", 200_000),
    ("claude-haiku", 200_000),
    ("claude-3", 200_000),
    ("gpt-5", 400_000),
    ("gpt-4.1", 1_000_000),
    ("gpt-4o", 128_000),
    ("gpt-4-turbo", 128_000),
    ("o1", 200_000),
    ("o3", 200_000),
    ("o4", 200_000),
    ("deepseek-v4", 1_000_000),
    ("deepseek-flash", 1_000_000),
    ("deepseek-chat", 64_000),
    ("deepseek-reasoner", 64_000),
    ("glm", 128_000),
    ("kimi", 128_000),
    ("moonshot", 128_000),
    ("qwen-long", 1_000_000),
    ("qwen", 32_000),
    ("doubao", 32_000),
    ("gemini-1.5", 1_000_000),
    ("gemini", 128_000),
];

const DEFAULT_WINDOW: usize = 32_000;

/// APIs that reject larger max_tokens values.
const OUTPUT_CAPS: &[(&str, u32)] = &[
    ("claude-opus-4-1", 32_000),
    ("claude-opus-4-0", 32_000),
    ("claude-opus-4-2025", 32_000),
    ("claude-opus-4@", 32_000),
    ("claude-opus-4-5", 64_000),
    ("claude-sonnet-4-5", 64_000),
    ("claude-sonnet-4-0", 64_000),
    ("claude-sonnet-4-2025", 64_000),
    ("claude-sonnet-4@", 64_000),
    ("claude-3-5-sonnet", 8192),
    ("claude-3-5-haiku", 8192),
    ("claude-3-opus", 4096),
    ("claude-3-haiku", 4096),
    ("gpt-4o", 16_384),
    ("deepseek-chat", 8192),
];

/// USD per million tokens: (input, cache read, output). Longest contained key wins.
/// Cache-read rates per model.
const PRICES: &[(&str, f64, f64, f64)] = &[
    ("claude-fable-5-1", 10.0, 0.25, 50.0),
    ("claude-fable-5", 10.0, 1.0, 50.0),
    ("claude-opus-5-5", 4.0, 0.20, 20.0),
    ("claude-opus-5", 5.0, 0.50, 25.0),
    ("claude-opus-4-8", 5.0, 0.50, 25.0),
    ("claude-opus-4-7", 5.0, 0.50, 25.0),
    ("claude-opus-4-6", 5.0, 0.50, 25.0),
    ("claude-opus-4-5", 5.0, 0.50, 25.0),
    ("claude-opus-4-1", 15.0, 1.5, 75.0),
    ("claude-opus-4", 15.0, 1.5, 75.0),
    ("claude-sonnet-5", 2.0, 0.20, 10.0),
    ("claude-sonnet-4-6", 3.0, 0.30, 15.0),
    ("claude-sonnet-4-5", 3.0, 0.30, 15.0),
    ("claude-sonnet-4", 3.0, 0.30, 15.0),
    ("claude-haiku-4-5", 1.0, 0.10, 5.0),
    ("claude-3-7-sonnet", 3.0, 0.30, 15.0),
    ("claude-3-5-sonnet", 3.0, 0.30, 15.0),
    ("claude-3-5-haiku", 0.8, 0.08, 4.0),
    ("claude-3-opus", 15.0, 1.5, 75.0),
    ("deepseek-chat", 0.14, 0.014, 0.28),
    ("deepseek-reasoner", 0.14, 0.014, 0.28),
    ("deepseek-v4-pro", 1.32, 0.044, 3.96),
    ("deepseek-flash", 0.30, 0.006, 1.20),
    ("deepseek-v4-flash", 0.30, 0.006, 1.20),
    ("gpt-4o", 2.5, 1.25, 10.0),
    ("gpt-4o-mini", 0.15, 0.075, 0.6),
    ("o3-mini", 1.1, 1.1, 4.4),
    ("gpt-5", 1.25, 0.125, 10.0),
    ("gpt-5-mini", 0.25, 0.025, 2.0),
    ("gpt-5-nano", 0.05, 0.005, 0.4),
    ("gpt-4.1", 2.0, 0.5, 8.0),
    ("gpt-4.1-mini", 0.4, 0.1, 1.6),
    ("gpt-4.1-nano", 0.1, 0.025, 0.4),
    ("o1", 15.0, 7.5, 60.0),
    ("o3", 2.0, 0.5, 8.0),
    ("o4-mini", 1.1, 0.275, 4.4),
];

/// An unknown Claude ID is billed at the most expensive tier, so a budget stays a ceiling.
const UNKNOWN_CLAUDE: (f64, f64, f64) = (15.0, 1.5, 75.0);

/// Anthropic bills a five-minute cache write at 1.25x the input rate.
pub const CACHE_WRITE_MULTIPLIER: f64 = 1.25;
/// Cache reads cost a tenth of the input rate unless the table says otherwise.
pub const CACHE_READ_RATIO: f64 = 0.1;

/// USD per million tokens for one model.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Prices {
    pub input: f64,
    pub output: f64,
    pub cache_read: f64,
    pub cache_write: f64,
}

impl Prices {
    /// Prices without a table entry: cache rates derived from the input rate.
    pub fn from_pair((input, output): (f64, f64)) -> Self {
        Self {
            input,
            output,
            cache_read: input * CACHE_READ_RATIO,
            cache_write: input * CACHE_WRITE_MULTIPLIER,
        }
    }

    /// Estimated USD for one response; cache reads and writes are parts of `usage.input`.
    pub fn cost(&self, usage: &crate::domain::Usage) -> f64 {
        let read = usage.cache_read.min(usage.input);
        let write = usage.cache_write.min(usage.input - read);
        let plain = usage.input - read - write;
        (plain as f64 * self.input
            + read as f64 * self.cache_read
            + write as f64 * self.cache_write
            + usage.output as f64 * self.output)
            / 1_000_000.0
    }
}

fn lower(model: &str) -> String {
    model.trim().to_ascii_lowercase()
}

pub fn context_window(model: &str) -> usize {
    let model = lower(model);
    WINDOWS
        .iter()
        .find(|(pattern, _)| model.contains(pattern))
        .map_or(DEFAULT_WINDOW, |(_, window)| *window)
}

/// A quarter of the window clamped to 4000..64000, then the model's own cap.
pub fn max_output_tokens(model: &str, window: usize) -> u32 {
    let reserve = (window / 4).clamp(4000, 64_000) as u32;
    let model = lower(model);
    OUTPUT_CAPS
        .iter()
        .find(|(pattern, _)| model.contains(pattern))
        .map_or(reserve, |(_, cap)| reserve.min(*cap))
}

/// Context budget the agent compacts at three quarters of: 85% of the window,
/// but compaction must still leave room for the reply and a safety margin.
pub fn context_budget(window: usize, max_output: u32) -> usize {
    let margin = (window / 8).clamp(1024, 8000);
    let room = window.saturating_sub(max_output as usize + margin);
    (window * 85 / 100).min(room * 4 / 3).max(4096)
}

/// List prices per million tokens; `None` for models the table does not know.
pub fn prices(model: &str) -> Option<(f64, f64)> {
    price_card(model).map(|card| (card.input, card.output))
}

/// List prices including cache rates; `None` for models the table does not know.
pub fn price_card(model: &str) -> Option<Prices> {
    let model = lower(model);
    PRICES
        .iter()
        .filter(|(key, ..)| model.contains(key))
        .max_by_key(|(key, ..)| key.len())
        .map(|(_, input, cache, output)| (*input, *cache, *output))
        .or_else(|| model.contains("claude").then_some(UNKNOWN_CLAUDE))
        .map(|(input, cache_read, output)| Prices {
            input,
            output,
            cache_read,
            cache_write: input * CACHE_WRITE_MULTIPLIER,
        })
}

/// Prices for `model`; explicitly configured prices keep the table's cache-read ratio.
pub fn prices_for(model: &str, configured: Option<(f64, f64)>) -> Option<Prices> {
    match configured {
        Some(pair) => {
            let mut card = Prices::from_pair(pair);
            if let Some(table) = price_card(model).filter(|table| table.input > 0.0) {
                card.cache_read = pair.0 * table.cache_read / table.input;
            }
            Some(card)
        }
        None => price_card(model),
    }
}

/// Substrings of models that accept image input.
const VISION: &[&str] = &[
    "claude-sonnet",
    "claude-opus",
    "claude-haiku",
    "claude-fable",
    "claude-mythos",
    "claude-3",
    "gpt-4o",
    "gpt-4-turbo",
    "gpt-4-vision",
    "gpt-4.1",
    "gpt-5",
    "o1",
    "o3",
    "o4",
    "glm-4v",
    "glm-4.5v",
    "cogview",
    "kimi",
    "moonshot",
    "qwen-vl",
    "qwen2-vl",
    "qwen2.5-vl",
    "qwen3-vl",
    "qvq",
    "omni",
    "doubao-1.5",
    "doubao-vision",
    "doubao-pro",
    "doubao-seed",
    "gemini",
    "gemma",
    "mistral",
    "pixtral",
    "llava",
    "phi-3-vision",
    "llama-3.2-vision",
    "llama-4",
    "minicpm-v",
];

/// Whether `model` is expected to accept images; DeepSeek only on its flash models.
pub fn vision_capable(model: &str) -> bool {
    let model = lower(model);
    if model.is_empty() {
        return true;
    }
    let name = model.rsplit('/').next().unwrap_or(&model);
    if name.starts_with("deepseek") {
        return matches!(
            name,
            "deepseek-flash" | "deepseek-v4-flash" | "deepseek-v4-flash-vision-exp"
        );
    }
    VISION.iter().any(|pattern| model.contains(pattern))
}

/// OpenAI reasoning families that take `max_completion_tokens` instead of `max_tokens`.
pub fn openai_reasoning(model: &str) -> bool {
    let model = lower(model);
    let name = model.rsplit('/').next().unwrap_or(&model);
    ["o1", "o3", "o4", "gpt-5"].iter().any(|family| {
        name.strip_prefix(family)
            .is_some_and(|rest| rest.is_empty() || rest.starts_with(['-', '.']))
    })
}

/// DeepSeek reasoner models reject tool definitions.
pub fn tool_free_reasoner(model: &str) -> bool {
    let model = lower(model);
    model.contains("deepseek-reasoner") || model.contains("deepseek-r1")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn windows_outputs_and_prices_follow_the_built_in_tables() {
        assert_eq!(context_window("deepseek-v4-pro"), 1_000_000);
        assert_eq!(context_window("Claude-Opus-4-1-20250805"), 200_000);
        assert_eq!(context_window("my-local-model"), 32_000);
        assert_eq!(max_output_tokens("gpt-4o", 128_000), 16_384);
        assert_eq!(max_output_tokens("deepseek-v4-pro", 1_000_000), 64_000);
        assert_eq!(max_output_tokens("tiny", 8_000), 4000);
        assert_eq!(prices("gpt-4o-mini-2024-07-18"), Some((0.15, 0.6)));
        assert_eq!(prices("deepseek-v4-pro"), Some((1.32, 3.96)));
        assert_eq!(prices("claude-new-thing"), Some((15.0, 75.0)));
        assert_eq!(prices("qwen3.6-27b"), None);
    }

    #[test]
    fn cache_reads_are_billed_cheaper_and_writes_at_a_premium() {
        let card = price_card("claude-sonnet-4-5").unwrap();
        assert_eq!(card.cache_read, 0.30);
        assert!((card.cache_write - 3.75).abs() < 1e-9);
        let usage = crate::domain::Usage {
            input: 1_000_000,
            output: 0,
            cache_read: 600_000,
            cache_write: 200_000,
        };
        // 200k plain at 3.0, 600k reads at 0.3, 200k writes at 3.75.
        assert!((card.cost(&usage) - (0.6 + 0.18 + 0.75)).abs() < 1e-9);
        let configured = prices_for("gpt-4o", Some((5.0, 20.0))).unwrap();
        assert_eq!(configured.cache_read, 2.5);
        assert!((prices_for("local", Some((1.0, 2.0))).unwrap().cache_read - 0.1).abs() < 1e-12);
    }

    #[test]
    fn capability_patterns_follow_go() {
        assert!(vision_capable("claude-sonnet-4-5"));
        assert!(vision_capable("deepseek-v4-flash"));
        assert!(!vision_capable("deepseek-v4-pro"));
        assert!(!vision_capable("qwen3-coder"));
        assert!(openai_reasoning("o3-mini") && openai_reasoning("openai/gpt-5.1"));
        assert!(!openai_reasoning("gpt-4o") && !openai_reasoning("o1x"));
        assert!(tool_free_reasoner("deepseek-reasoner"));
    }

    #[test]
    fn budget_leaves_room_for_the_reply() {
        let budget = context_budget(32_000, 8000);
        assert!(budget * 3 / 4 + 8000 + 4000 <= 32_000, "{budget}");
        assert_eq!(context_budget(1_000_000, 64_000), 850_000);
    }
}
