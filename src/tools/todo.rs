//! The `todo` plan (shared contract with execute_plan): the latest `todo` call in the
//! session history is the current plan, so it survives resume without extra state.

use std::collections::HashSet;

use serde::{Deserialize, Serialize};

use crate::{
    agents::{plan::strip_writes_prefix, writes::WriteSet},
    domain::{Block, Error, Result},
    session::Session,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TodoStatus {
    Pending,
    InProgress,
    Completed,
    Cancelled,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Priority {
    High,
    Medium,
    Low,
}

impl Priority {
    fn name(self) -> &'static str {
        match self {
            Self::High => "high",
            Self::Medium => "medium",
            Self::Low => "low",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Todo {
    pub content: String,
    pub status: TodoStatus,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub priority: Option<Priority>,
    /// Defaults to `todo-<1-based index>`.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub depends: Vec<String>,
    /// Files (paths or globs, maybe `file::symbol`) the item may write; execute_plan
    /// enforces them per file (docs/optimizations/0054).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub writes: Vec<String>,
    /// Whole minutes; a decimal is rounded.
    #[serde(
        default,
        deserialize_with = "crate::engine::estimate::deserialize_minutes",
        skip_serializing_if = "Option::is_none"
    )]
    pub estimate_minutes: Option<u32>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct TodoArgs {
    todos: Vec<Todo>,
}

/// Fills default IDs and moves `depends:todo-1,todo-2 ` and `writes:a.rs,b.rs ` content
/// prefixes (in either order) into `depends` and `writes`.
pub fn normalize(mut todos: Vec<Todo>) -> Vec<Todo> {
    for (index, todo) in todos.iter_mut().enumerate() {
        if todo.id.as_deref().is_none_or(|id| id.trim().is_empty()) {
            todo.id = Some(format!("todo-{}", index + 1));
        }
        todo.content = strip_writes_prefix(&todo.content, &mut todo.writes).to_owned();
        let trimmed = todo.content.trim_start();
        if let Some(rest) = trimmed.strip_prefix("depends:") {
            let (list, content) = rest.split_once(char::is_whitespace).unwrap_or((rest, ""));
            for id in list.split(',').map(str::trim).filter(|id| !id.is_empty()) {
                if !todo.depends.iter().any(|known| known == id) {
                    todo.depends.push(id.to_owned());
                }
            }
            todo.content = content.trim().to_owned();
        }
        todo.content = strip_writes_prefix(&todo.content, &mut todo.writes).to_owned();
    }
    todos
}

fn validate(todos: &[Todo]) -> Result<()> {
    if todos.len() > 50 {
        return Err(Error::Tool("at most 50 todos".into()));
    }
    if todos
        .iter()
        .any(|todo| todo.content.trim().is_empty() || todo.content.len() > 500)
    {
        return Err(Error::Tool(
            "each todo needs content of 1..500 bytes".into(),
        ));
    }
    if todos
        .iter()
        .filter(|todo| todo.status == TodoStatus::InProgress)
        .count()
        > 1
    {
        return Err(Error::Tool("at most one todo may be in_progress".into()));
    }
    let mut ids = HashSet::new();
    for todo in todos {
        let id = todo.id.as_deref().unwrap_or_default();
        if id.len() > 64 || !ids.insert(id) {
            return Err(Error::Tool(format!("duplicate or invalid todo id {id:?}")));
        }
    }
    for todo in todos {
        WriteSet::parse(&todo.writes).map_err(|err| {
            Error::Tool(format!(
                "todo {}: {err}",
                todo.id.as_deref().unwrap_or_default()
            ))
        })?;
        if let Some(missing) = todo.depends.iter().find(|id| !ids.contains(id.as_str())) {
            return Err(Error::Tool(format!(
                "todo {} depends on unknown id {missing:?}",
                todo.id.as_deref().unwrap_or_default()
            )));
        }
        if todo
            .depends
            .iter()
            .any(|id| Some(id.as_str()) == todo.id.as_deref())
        {
            return Err(Error::Tool("a todo cannot depend on itself".into()));
        }
    }
    Ok(())
}

/// One line per todo: `[✓] todo-1. text [high] (after todo-0)`.
pub fn render(todos: &[Todo]) -> String {
    let mut out = String::new();
    for todo in todos {
        let mark = match todo.status {
            TodoStatus::Pending => "[ ]",
            TodoStatus::InProgress => "[>]",
            TodoStatus::Completed => "[✓]",
            TodoStatus::Cancelled => "[x]",
        };
        out.push_str(&format!(
            "{mark} {}. {}",
            todo.id.as_deref().unwrap_or_default(),
            todo.content.trim()
        ));
        if let Some(priority) = todo.priority {
            out.push_str(&format!(" [{}]", priority.name()));
        }
        if !todo.depends.is_empty() {
            out.push_str(&format!(" (after {})", todo.depends.join(", ")));
        }
        match (todo.writes.len(), todo.estimate_minutes) {
            (0, None) => {}
            (0, Some(minutes)) => out.push_str(&format!(" [~{minutes} min]")),
            (files, None) => out.push_str(&format!(" [writes {files}]")),
            (files, Some(minutes)) => out.push_str(&format!(" [writes {files}, ~{minutes} min]")),
        }
        out.push('\n');
    }
    out
}

pub(super) fn execute(args: TodoArgs) -> Result<String> {
    let todos = normalize(args.todos);
    validate(&todos)?;
    let open = todos
        .iter()
        .filter(|todo| matches!(todo.status, TodoStatus::Pending | TodoStatus::InProgress))
        .count();
    Ok(format!(
        "{}{open} open of {}\n",
        render(&todos),
        todos.len()
    ))
}

/// The plan of the latest successful `todo` call in `session` (finished history first,
/// then a finished call of the batch in progress); `None` without one.
pub fn latest_todos(session: &Session) -> Option<Vec<Todo>> {
    let failed: HashSet<&str> = session
        .messages
        .iter()
        .flat_map(|message| &message.blocks)
        .filter_map(|block| match block {
            Block::ToolResult { id, is_error, .. } if *is_error => Some(id.as_str()),
            _ => None,
        })
        .chain(
            session
                .pending
                .iter()
                .filter(|call| call.result.as_ref().is_none_or(|result| result.failed))
                .map(|call| call.id.as_str()),
        )
        .collect();
    session
        .messages
        .iter()
        .rev()
        .flat_map(|message| message.blocks.iter().rev())
        .find_map(|block| match block {
            Block::ToolUse {
                id, name, input, ..
            } if name == "todo" && !failed.contains(id.as_str()) => {
                let args: TodoArgs = serde_json::from_value(input.clone()).ok()?;
                let todos = normalize(args.todos);
                validate(&todos).ok()?;
                Some(todos)
            }
            _ => None,
        })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{Message, Role};
    use serde_json::json;

    fn parse(value: serde_json::Value) -> Result<String> {
        execute(serde_json::from_value(value).map_err(Error::from)?)
    }

    #[test]
    fn todos_show_ids_priorities_and_dependencies() {
        let out = parse(json!({"todos":[
            {"content":"design","status":"completed","priority":"high"},
            {"content":"depends:todo-1 build it","status":"in_progress"},
            {"content":"drop","status":"cancelled","id":"x"}
        ]}))
        .unwrap();
        assert_eq!(
            out,
            "[✓] todo-1. design [high]\n[>] todo-2. build it (after todo-1)\n[x] x. drop\n1 open of 3\n"
        );
        assert!(
            parse(json!({"todos":[{"content":"a","status":"pending","depends":["nope"]}]}))
                .is_err()
        );
    }

    #[test]
    fn writes_and_estimates_come_from_fields_or_the_content_prefix() {
        let out = parse(json!({"todos":[
            {"content":"writes:src/a.rs,src/b.rs::f build","status":"pending","estimate_minutes":6},
            {"content":"depends:todo-1 writes:docs/*.md docs","status":"pending","writes":["README.md"]},
            {"content":"plain","status":"pending","estimate_minutes":3}
        ]}))
        .unwrap();
        assert_eq!(
            out,
            "[ ] todo-1. build [writes 2, ~6 min]\n[ ] todo-2. docs (after todo-1) [writes 2]\n[ ] todo-3. plain [~3 min]\n3 open of 3\n"
        );
        let todos = normalize(
            serde_json::from_value::<TodoArgs>(json!({"todos":[
                {"content":"depends:todo-1 writes:docs/*.md docs","status":"pending","writes":["README.md"]}
            ]}))
            .unwrap()
            .todos,
        );
        assert_eq!(todos[0].writes, ["README.md", "docs/*.md"]);
        assert_eq!(todos[0].depends, ["todo-1"]);
        assert!(
            parse(json!({"todos":[{"content":"a","status":"pending","writes":["../x"]}]}))
                .unwrap_err()
                .to_string()
                .contains("workspace-relative")
        );
        assert!(
            parse(json!({"todos":[{"content":"a","status":"pending","estimate_minutes":-1}]}))
                .is_err()
        );
        // A decimal estimate is rounded, not a reason to refuse the whole list.
        assert!(
            parse(json!({"todos":[{"content":"a","status":"pending","estimate_minutes":2.5}]}))
                .unwrap()
                .contains("~3 min")
        );
    }

    #[test]
    fn latest_successful_plan_is_restored_from_history() {
        let mut session = Session::new("w".into(), "p".into(), "m".into(), None);
        let call = |id: &str, content: &str| Block::ToolUse {
            id: id.into(),
            name: "todo".into(),
            input: json!({"todos":[{"content":content,"status":"pending"}]}),
            extra: None,
        };
        session.messages.push(Message {
            role: Role::Assistant,
            blocks: vec![call("a", "first")],
            origin: crate::domain::Origin::Unknown,
        });
        session.messages.push(Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: "a".into(),
                content: "ok".into(),
                is_error: false,
            }],
            origin: crate::domain::Origin::Unknown,
        });
        session.messages.push(Message {
            role: Role::Assistant,
            blocks: vec![call("b", "")],
            origin: crate::domain::Origin::Unknown,
        });
        session.messages.push(Message {
            role: Role::User,
            blocks: vec![Block::ToolResult {
                id: "b".into(),
                content: "bad".into(),
                is_error: true,
            }],
            origin: crate::domain::Origin::Unknown,
        });
        let todos = latest_todos(&session).unwrap();
        assert_eq!(todos[0].content, "first");
        assert_eq!(todos[0].id.as_deref(), Some("todo-1"));
    }
}
