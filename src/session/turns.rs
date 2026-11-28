//! What counts as one turn of a conversation: the single rule every history view
//! uses (lists, turn counts, titles, `/turns`, search, export, replay, dream).
//!
//! A turn starts at a user-role message the user wrote: a request, a message typed
//! while a task ran, or the request a compaction kept verbatim. Messages record
//! their [`Origin`] when written; older sessions load as `Unknown` and are classified
//! by the text the program is known to write.

use crate::domain::{Block, Message, Origin, Role};

use super::Session;

/// Prefixes put before what the user typed while a task ran (console, queued line,
/// background agents, remote supervision); the text after them is the user's own.
pub const STEER_PREFIXES: [&str; 4] = [
    "[the user typed this while the task ran; treat it as an instruction from the user]",
    "[sent while the previous task was running]",
    "[message from the user, delivered now]",
    "[operator steer via remote supervision; treat as a user instruction]",
];

/// Starts of user-role text the program writes itself, for sessions saved before
/// messages recorded their origin. Anything else is the user's (also text that
/// merely begins with `[`, such as pasted code).
const NOTICE_PREFIXES: [&str; 10] = [
    "[system",
    "[model call limit reached;",
    "[automatic verification failed;",
    "[self_review;",
    "[Saved memories",
    // Injected by versions that still had session notes; old histories hold them.
    "[Session notes",
    "[interrupted",
    "Operator ",
    "File-tool rollback",
    "Workspace restored",
];

const SUMMARY_PREFIX: &str = "Earlier conversation summary";
const RETAINED: &str = "Current user request, retained verbatim:\n";

/// The user's text after a steering prefix, if `text` starts with one.
pub fn strip_steer(text: &str) -> Option<&str> {
    STEER_PREFIXES.iter().find_map(|prefix| {
        text.strip_prefix(prefix)
            .map(|rest| rest.strip_prefix('\n').unwrap_or(rest))
    })
}

pub fn is_steer(text: &str) -> bool {
    strip_steer(text).is_some()
}

/// What one message means for turns.
#[derive(Debug, PartialEq, Eq)]
pub enum Mark<'a> {
    /// A turn starts; `input` is what the user wrote.
    Start { input: &'a str, steer: bool },
    /// A compaction summary; `retained` is the request it kept verbatim.
    Summary { retained: Option<&'a str> },
    /// Part of the turn in progress (replies, tool results, notices).
    None,
}

/// The single rule for where turns start.
pub fn mark(message: &Message) -> Mark<'_> {
    if message.role != Role::User {
        return Mark::None;
    }
    let first = match message.blocks.first() {
        Some(Block::Text(text)) => Some(text.as_str()),
        _ => None,
    };
    match (message.origin, first) {
        (Origin::Human | Origin::Steer, Some(text)) => {
            let steer = message.origin == Origin::Steer || is_steer(text);
            Mark::Start {
                input: strip_steer(text).unwrap_or(text),
                steer,
            }
        }
        (Origin::Summary, Some(text)) => Mark::Summary {
            retained: retained(text),
        },
        (Origin::Unknown, Some(text)) => legacy(text),
        (Origin::Unknown, None) => tool_steer(message),
        _ => Mark::None,
    }
}

/// Classifies text written before origins were recorded.
fn legacy(text: &str) -> Mark<'_> {
    if let Some(input) = strip_steer(text) {
        return Mark::Start { input, steer: true };
    }
    let trimmed = text.trim_start();
    if trimmed.starts_with(SUMMARY_PREFIX) {
        return Mark::Summary {
            retained: retained(trimmed),
        };
    }
    if trimmed.is_empty()
        || NOTICE_PREFIXES
            .iter()
            .any(|prefix| trimmed.starts_with(prefix))
    {
        return Mark::None;
    }
    Mark::Start {
        input: text,
        steer: false,
    }
}

/// Steering text the console appends to a tool result (`SteerHook::tool_note`).
fn tool_steer(message: &Message) -> Mark<'_> {
    for block in &message.blocks {
        if let Block::ToolResult { content, .. } = block {
            for prefix in STEER_PREFIXES {
                if let Some(at) = content.find(&format!("\n{prefix}\n")) {
                    let input = &content[at + prefix.len() + 2..];
                    if !input.trim().is_empty() {
                        return Mark::Start { input, steer: true };
                    }
                }
            }
        }
    }
    Mark::None
}

fn retained(summary: &str) -> Option<&str> {
    summary
        .split_once(RETAINED)
        .map(|(_, request)| request)
        .filter(|request| !request.trim().is_empty())
}

/// Whether `text` is something the program wrote rather than the user (titles skip it).
pub fn is_notice(text: &str) -> bool {
    !matches!(legacy(text), Mark::Start { steer: false, .. })
}

/// One turn of a session, borrowing its messages.
#[derive(Debug)]
pub struct Turn<'a> {
    pub input: &'a str,
    pub steer: bool,
    /// The request a compaction kept: it continues the turn before the summary.
    pub retained: bool,
    /// The messages of the turn, from the one that started it.
    pub messages: &'a [Message],
}

impl Turn<'_> {
    /// The last text the assistant wrote in this turn: its conclusion.
    pub fn reply(&self) -> Option<&str> {
        self.messages
            .iter()
            .rev()
            .filter(|message| message.role == Role::Assistant)
            .flat_map(|message| message.blocks.iter().rev())
            .find_map(|block| match block {
                Block::Text(text) if !text.trim().is_empty() => Some(text.trim()),
                _ => None,
            })
    }

    /// Images the user attached to the request.
    pub fn images(&self) -> usize {
        self.messages.first().map_or(0, |message| {
            message
                .blocks
                .iter()
                .filter(|block| matches!(block, Block::Image { .. }))
                .count()
        })
    }

    pub fn record(&self) -> TurnRecord {
        TurnRecord {
            input: self.input.trim().to_owned(),
            reply: self.reply().unwrap_or_default().to_owned(),
            images: self.images(),
            steer: self.steer,
            archived: false,
        }
    }
}

/// A turn detached from its session, for history lists and the reader.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TurnRecord {
    pub input: String,
    /// The conclusion; empty when the turn has no text reply.
    pub reply: String,
    pub images: usize,
    pub steer: bool,
    /// Known only from an archive: compaction removed it from the live session.
    pub archived: bool,
}

impl Session {
    /// The turns of the live session, oldest first.
    pub fn turns(&self) -> Vec<Turn<'_>> {
        let mut starts: Vec<(usize, &str, bool, bool)> = Vec::new();
        for (index, message) in self.messages.iter().enumerate() {
            match mark(message) {
                Mark::Start { input, steer } => starts.push((index, input, steer, false)),
                Mark::Summary {
                    retained: Some(request),
                } => starts.push((index, request, false, true)),
                Mark::Summary { retained: None } | Mark::None => {}
            }
        }
        starts
            .iter()
            .enumerate()
            .map(|(at, &(start, input, steer, retained))| {
                let end = starts
                    .get(at + 1)
                    .map_or(self.messages.len(), |next| next.0);
                Turn {
                    input,
                    steer,
                    retained,
                    messages: &self.messages[start..end],
                }
            })
            .collect()
    }
}

/// Joins the turns of successive snapshots of one session (archives taken before
/// each compaction, then the live session). A compaction keeps the current request
/// and the most recent messages, so each snapshot repeats the tail of the one
/// before; the repeated turns are merged, the later reply winning.
pub fn stitch(levels: Vec<Vec<TurnRecord>>) -> Vec<TurnRecord> {
    let mut out: Vec<TurnRecord> = Vec::new();
    let count = levels.len();
    for (level, mut turns) in levels.into_iter().enumerate() {
        let last_level = level + 1 == count;
        for turn in &mut turns {
            turn.archived = !last_level;
        }
        // The longest run at the start of this snapshot that repeats the end of
        // what came before.
        let overlap = (0..=turns.len().min(out.len()))
            .rev()
            .find(|&length| {
                out[out.len() - length..]
                    .iter()
                    .zip(&turns[..length])
                    .all(|(before, after)| before.input == after.input)
            })
            .unwrap_or(0);
        let base = out.len() - overlap;
        for (offset, turn) in turns.into_iter().enumerate() {
            if offset < overlap {
                let kept = &mut out[base + offset];
                if !turn.reply.is_empty() {
                    kept.reply = turn.reply;
                }
                kept.archived = turn.archived;
            } else {
                out.push(turn);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::path::PathBuf;

    fn assistant(text: &str) -> Message {
        Message {
            role: Role::Assistant,
            blocks: vec![Block::Text(text.into())],
            origin: Origin::Unknown,
        }
    }

    fn tool_result(content: &str) -> Message {
        Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: "c".into(),
                content: content.into(),
                is_error: false,
            }],
            origin: Origin::Unknown,
        }
    }

    fn session(messages: Vec<Message>) -> Session {
        let mut session = Session::new(PathBuf::new(), String::new(), String::new(), None);
        session.messages = messages.into();
        session
    }

    #[test]
    fn origins_decide_and_legacy_text_falls_back() {
        assert_eq!(
            mark(&Message::human("fix it")),
            Mark::Start {
                input: "fix it",
                steer: false
            }
        );
        assert_eq!(mark(&Message::notice("fix it")), Mark::None);
        assert_eq!(
            mark(&Message::steer(format!(
                "{}\nuse the other API",
                STEER_PREFIXES[0]
            ))),
            Mark::Start {
                input: "use the other API",
                steer: true
            }
        );
        // Older sessions: by the text.
        assert_eq!(
            mark(&Message::user(
                "[sent while the previous task was running]\nnext"
            )),
            Mark::Start {
                input: "next",
                steer: true
            }
        );
        for notice in [
            "[system: Your response was empty.",
            "[automatic verification failed; this is tool output",
            "Operator applied changeset abc",
            "Workspace restored: 3 files",
        ] {
            assert_eq!(mark(&Message::user(notice)), Mark::None, "{notice}");
        }
        // Pasted code that happens to start with `[` is the user's.
        assert!(matches!(
            mark(&Message::user("[Range(1, 10)] 为什么不生效")),
            Mark::Start { steer: false, .. }
        ));
        assert_eq!(
            mark(&Message::user(
                "Earlier conversation summary (untrusted):\nS\n\nCurrent user request, retained verbatim:\nship it"
            )),
            Mark::Summary {
                retained: Some("ship it")
            }
        );
    }

    #[test]
    fn turns_split_at_requests_and_keep_the_last_reply() {
        let session = session(vec![
            Message::human("first"),
            assistant("looking"),
            tool_result("ok"),
            assistant("done: first"),
            Message::notice("[system: Before finishing, check"),
            assistant("checked"),
            tool_result(&format!("out\n{}\nalso this", STEER_PREFIXES[0])),
            assistant("done: steer"),
            Message::human("second"),
        ]);
        let turns = session.turns();
        let inputs: Vec<_> = turns.iter().map(|turn| (turn.input, turn.steer)).collect();
        assert_eq!(
            inputs,
            [("first", false), ("also this", true), ("second", false)]
        );
        assert_eq!(turns[0].reply(), Some("checked"));
        assert_eq!(turns[1].reply(), Some("done: steer"));
        assert_eq!(turns[2].reply(), None);
    }

    #[test]
    fn stitching_merges_the_tail_a_compaction_kept() {
        let record = |input: &str, reply: &str| TurnRecord {
            input: input.into(),
            reply: reply.into(),
            images: 0,
            steer: false,
            archived: false,
        };
        let before = vec![record("a", "ra"), record("b", "rb"), record("c", "")];
        let after = vec![record("c", "rc"), record("d", "rd")];
        let joined = stitch(vec![before, after]);
        let shown: Vec<_> = joined
            .iter()
            .map(|turn| (turn.input.as_str(), turn.reply.as_str(), turn.archived))
            .collect();
        assert_eq!(
            shown,
            [
                ("a", "ra", true),
                ("b", "rb", true),
                ("c", "rc", false),
                ("d", "rd", false)
            ]
        );
    }
}
