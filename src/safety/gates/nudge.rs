//! Nudges when the model ends a turn: an empty reply, a step announced but not done,
//! a too-short ending after real work, an open todo list, and one "is the request fully
//! met?" check after file changes. Skipped near the call or budget limit.

use std::sync::{Arc, Mutex};

use futures_util::future::BoxFuture;
use serde_json::Value;

use super::{Evidence, Switch, last_reply};
use crate::{
    domain::Block, engine::turn::TurnHook, session::Session, session::workspace::Workspace,
};

const MAX_EMPTY: u32 = 2;
const MAX_CONTINUATION: u32 = 2;

pub const EMPTY: &str = "[system: Your response was empty. Provide the answer or call a tool.]";
pub const ANNOUNCED: &str = "[system: You announced a next step but did not perform it. Continue now by calling the required tools, or state clearly that the task is complete.]";
pub const TOO_SHORT: &str = "[system: Your last message is too brief to be a final answer after doing work. Summarize what you changed for the latest request and what remains, or continue.]";
pub const DONE_CHECK: &str = "[system: Before finishing, check whether the user's latest request has been fully met. If anything remains, continue working now; if everything is done, reply with the final answer about that request.]";

pub struct NudgeLimits {
    pub max_turns: u32,
    pub budget: Option<f64>,
    /// Whether the done check applies (config on, or auto and a non-Anthropic provider).
    pub done_check: bool,
    /// Heuristic nudges (announced step, too-short ending) need a watching user; scripted
    /// runs stay deterministic and are judged by their exit code and verification.
    pub heuristics: bool,
}

impl NudgeLimits {
    /// `auto` applies to interactive sessions with non-Anthropic providers.
    pub fn done_check(switch: Switch, anthropic: bool, interactive: bool) -> bool {
        match switch {
            Switch::On => true,
            Switch::Off => false,
            Switch::Auto => !anthropic && interactive,
        }
    }
}

#[derive(Default)]
struct State {
    start_calls: u64,
    empty: u32,
    continuation: u32,
    done_checked: bool,
    todo_nudged: bool,
}

pub struct Nudges {
    limits: NudgeLimits,
    workspace: Option<Arc<Workspace>>,
    evidence: Arc<Evidence>,
    state: Mutex<State>,
}

impl Nudges {
    pub fn new(
        limits: NudgeLimits,
        workspace: Option<Arc<Workspace>>,
        evidence: Arc<Evidence>,
    ) -> Self {
        Self {
            limits,
            workspace,
            evidence,
            state: Mutex::new(State::default()),
        }
    }

    fn near_limits(&self, session: &Session, state: &State) -> bool {
        let used = session
            .accounting
            .model_calls
            .saturating_sub(state.start_calls);
        if used + 1 >= u64::from(self.limits.max_turns) {
            return true;
        }
        matches!((self.limits.budget, session.accounting.cost), (Some(budget), Some(cost)) if cost >= budget * 0.9)
    }

    fn files_changed(&self) -> bool {
        self.workspace
            .as_ref()
            .is_some_and(|workspace| workspace.fingerprint().ok().flatten().is_some())
    }
}

/// Open items of the latest `todo` call (shared todo contract).
pub fn open_todos(session: &Session) -> Vec<String> {
    let Some(input) = session
        .messages
        .iter()
        .rev()
        .flat_map(|message| message.blocks.iter().rev())
        .find_map(|block| match block {
            Block::ToolUse { name, input, .. } if name == "todo" => Some(input),
            _ => None,
        })
    else {
        return Vec::new();
    };
    input
        .get("todos")
        .and_then(Value::as_array)
        .map(|todos| {
            todos
                .iter()
                .filter(|todo| {
                    matches!(
                        todo.get("status").and_then(Value::as_str),
                        Some("pending" | "in_progress")
                    )
                })
                .filter_map(|todo| todo.get("content").and_then(Value::as_str))
                .map(|content| content.trim().to_owned())
                .collect()
        })
        .unwrap_or_default()
}

const COMPLETION_MARKERS: &[&str] = &[
    "完成",
    "已提交",
    "已修复",
    "已修改",
    "已更新",
    "已添加",
    "已删除",
    "已创建",
    "搞定",
    "done",
    "finished",
    "complete",
    "committed",
];
const ANNOUNCE_ZH: &[&str] = &[
    "接下来",
    "下一步",
    "现在我来",
    "现在让我",
    "让我",
    "我这就",
    "我现在就",
    "我将",
    "现在开始",
    "然后我",
];
const ANNOUNCE_EN: &[&str] = &[
    "let me ",
    "i'll ",
    "i will ",
    "next, ",
    "now i'll ",
    "now i will ",
    "now let me ",
    "i'm going to ",
    "i am going to ",
    "then i'll ",
];
const EXCLUSIONS: &[&str] = &[
    "let me know",
    "建议",
    "如需",
    "如果需要",
    "是否需要",
    "如有问题",
    "请",
    "我将不",
    "不会",
    "不再",
    "等待",
    "确认",
];
const EXCLUDED_WORDS: &[&str] = &[
    "you", "your", "yours", "please", "confirm", "once", "not", "won't", "don't", "can't",
    "cannot", "never",
];

fn completion_marker(lower: &str) -> bool {
    if COMPLETION_MARKERS
        .iter()
        .any(|marker| lower.contains(marker))
    {
        return true;
    }
    // 已…修改 within a few characters.
    let chars: Vec<char> = lower.chars().collect();
    chars.iter().enumerate().any(|(index, c)| {
        *c == '已' && {
            let rest: String = chars[index + 1..chars.len().min(index + 9)]
                .iter()
                .take_while(|c| !c.is_whitespace() && !"，。,.!！;；".contains(**c))
                .collect();
            ["修改", "更新", "添加", "删除", "创建", "修复", "提交"]
                .iter()
                .any(|verb| rest.contains(verb))
        }
    })
}

fn last_paragraph(text: &str) -> &str {
    text.rsplit("\n\n")
        .map(str::trim)
        .find(|p| !p.is_empty())
        .unwrap_or("")
}

/// "Let me run the tests." as the final words, without running them.
pub fn announced_without_action(text: &str) -> bool {
    let paragraph = last_paragraph(text);
    if paragraph.is_empty()
        || paragraph.chars().count() > 300
        || paragraph.ends_with(['?', '？'])
        || paragraph.contains('你')
    {
        return false;
    }
    let lower = paragraph.to_lowercase();
    if EXCLUSIONS.iter().any(|x| lower.contains(x)) || completion_marker(&lower) {
        return false;
    }
    if lower
        .split(|c: char| !(c.is_ascii_lowercase() || c == '\''))
        .any(|word| EXCLUDED_WORDS.contains(&word))
    {
        return false;
    }
    lower
        .split(['\n', '.', '。', '!', '！', ';', '；'])
        .any(|sentence| {
            let sentence = sentence.trim();
            if sentence.starts_with("让我们") {
                return false;
            }
            ANNOUNCE_ZH.iter().any(|m| sentence.starts_with(m))
                || ANNOUNCE_EN
                    .iter()
                    .any(|m| format!("{sentence} ").starts_with(m))
        })
}

/// A final message too short to report on work that was done.
pub fn degenerate_ending(text: &str, did_work: bool) -> bool {
    if !did_work {
        return false;
    }
    let text = text.trim();
    let cjk = text.chars().any(|c| ('\u{4E00}'..='\u{9FFF}').contains(&c));
    let limit = if cjk { 20 } else { 40 };
    text.chars().count() < limit && !completion_marker(&text.to_lowercase())
}

impl TurnHook for Nudges {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock() {
            *state = State {
                start_calls: session.accounting.model_calls,
                ..State::default()
            };
        }
        Box::pin(async { None })
    }

    fn finish_nudge<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        let nudge = (|| {
            let mut state = self.state.lock().ok()?;
            if self.near_limits(session, &state) {
                return None;
            }
            let text = last_reply(session);
            if text.trim().is_empty() {
                if state.empty < MAX_EMPTY {
                    state.empty += 1;
                    return Some(EMPTY.to_owned());
                }
                return None;
            }
            if self.limits.heuristics && state.continuation < MAX_CONTINUATION {
                if announced_without_action(&text) {
                    state.continuation += 1;
                    return Some(ANNOUNCED.to_owned());
                }
                if degenerate_ending(&text, self.evidence.did_work()) {
                    state.continuation += 1;
                    return Some(TOO_SHORT.to_owned());
                }
            }
            if !state.todo_nudged {
                let open = open_todos(session);
                if !open.is_empty() {
                    state.todo_nudged = true;
                    let list: Vec<String> = open
                        .iter()
                        .take(10)
                        .map(|item| format!("- {item}"))
                        .collect();
                    return Some(format!(
                        "[system: Your todo list still has {} open item(s):\n{}\nFinish them now, or update the todo list (completed/cancelled) and say why they are not needed.]",
                        open.len(),
                        list.join("\n")
                    ));
                }
            }
            if self.limits.done_check && !state.done_checked && self.files_changed() {
                state.done_checked = true;
                return Some(DONE_CHECK.to_owned());
            }
            None
        })();
        Box::pin(async move { nudge })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn announcement_heuristics() {
        assert!(announced_without_action(
            "I found the bug.\n\nLet me fix it now."
        ));
        assert!(announced_without_action("接下来我修改配置文件"));
        assert!(!announced_without_action(
            "Let me know if you need anything else."
        ));
        assert!(!announced_without_action("I'll leave it as is? "));
        assert!(!announced_without_action("All changes are done."));
        assert!(!announced_without_action("已修改配置文件"));
        assert!(!announced_without_action("The fix is in src/main.rs."));
    }

    #[test]
    fn short_endings_after_work() {
        assert!(degenerate_ending("ok", true));
        assert!(!degenerate_ending("ok", false));
        assert!(!degenerate_ending("Done.", true));
        assert!(!degenerate_ending(
            "Updated src/lib.rs to handle the empty case and added a regression test.",
            true
        ));
    }

    fn session_with(messages: Vec<crate::domain::Message>) -> Session {
        let mut session = Session::new("/".into(), String::new(), String::new(), None);
        session.messages = messages.into();
        session
    }

    fn assistant(blocks: Vec<Block>) -> crate::domain::Message {
        crate::domain::Message {
            role: crate::domain::Role::Assistant,
            blocks,
        }
    }

    #[tokio::test]
    async fn nudges_are_bounded() {
        let nudges = Nudges::new(
            NudgeLimits {
                max_turns: 50,
                budget: None,
                done_check: true,
                heuristics: true,
            },
            None,
            Arc::new(Evidence::default()),
        );
        let empty = session_with(vec![assistant(vec![])]);
        nudges.task_note(&empty).await;
        assert_eq!(nudges.finish_nudge(&empty).await.as_deref(), Some(EMPTY));
        assert_eq!(nudges.finish_nudge(&empty).await.as_deref(), Some(EMPTY));
        assert_eq!(nudges.finish_nudge(&empty).await, None);
        let todo = Block::ToolUse {
            id: "t".into(),
            name: "todo".into(),
            input: serde_json::json!({"todos":[{"content":"write tests","status":"pending"},{"content":"x","status":"completed"},{"content":"y","status":"cancelled"}]}),
            extra: None,
        };
        let open = session_with(vec![
            assistant(vec![todo]),
            assistant(vec![Block::Text(
                "All of it is finished now, see summary.".into(),
            )]),
        ]);
        assert_eq!(open_todos(&open), ["write tests"]);
        let nudge = nudges.finish_nudge(&open).await.unwrap();
        assert!(nudge.contains("write tests"), "{nudge}");
        assert_eq!(nudges.finish_nudge(&open).await, None);
    }

    #[tokio::test]
    async fn nudges_stop_near_the_call_limit() {
        let nudges = Nudges::new(
            NudgeLimits {
                max_turns: 2,
                budget: None,
                done_check: true,
                heuristics: true,
            },
            None,
            Arc::new(Evidence::default()),
        );
        let mut session = session_with(vec![assistant(vec![])]);
        nudges.task_note(&session).await;
        session.accounting.model_calls = 1;
        assert_eq!(nudges.finish_nudge(&session).await, None);
    }
}
