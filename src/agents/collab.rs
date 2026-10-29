//! Experimental coordination tools (`experimental_tools: true`, off by default).
//!
//! The `task*` tools only record state on an in-memory board; they say so in every result.
//! `team_create` really runs its members as concurrent sub-agents (through the plan
//! executor); `send_message` really delivers into sub-agent prompts or background agents.

use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::Duration,
};

use serde::Deserialize;
use serde_json::{Value, json};
use tokio_util::sync::CancellationToken;

use crate::{
    agents::lifecycle::Kind,
    agents::plan::{PlanTask, run_tasks},
    agents::subagent::{AgentHub, clip},
    domain::{Error, Result, ToolDefinition},
    tools::ToolOutput,
};

pub const TOOLS: &[&str] = &[
    "task",
    "task_list",
    "task_update",
    "task_stop",
    "task_get",
    "task_output",
    "team_create",
    "team_delete",
    "send_message",
    "brief",
    "sleep",
];

#[derive(Default)]
pub struct Board {
    inner: Mutex<BoardInner>,
}

#[derive(Default)]
struct BoardInner {
    tasks: BTreeMap<String, TaskRecord>,
    teams: BTreeMap<String, Vec<String>>,
    counter: u32,
    user_messages: Vec<String>,
}

#[derive(Clone)]
struct TaskRecord {
    title: String,
    description: String,
    status: String,
    team: Option<String>,
    output: String,
}

fn definition(
    name: &str,
    description: &str,
    properties: Value,
    required: &[&str],
) -> ToolDefinition {
    ToolDefinition {
        name: name.into(),
        description: description.into(),
        schema: json!({"type":"object","properties":properties,"required":required,"additionalProperties":false}),
    }
}

pub fn definitions() -> Vec<ToolDefinition> {
    let id = json!({"task_id":{"type":"string"}});
    vec![
        definition(
            "task",
            "[experimental] Record a to-do item on the task board. It only records state: nothing runs it.",
            json!({"title":{"type":"string"},"description":{"type":"string"}}),
            &["title", "description"],
        ),
        definition(
            "task_list",
            "[experimental] List task board items and team members.",
            json!({}),
            &[],
        ),
        definition(
            "task_get",
            "[experimental] Show one task board item.",
            id.clone(),
            &["task_id"],
        ),
        definition(
            "task_output",
            "[experimental] Output recorded for a task board item.",
            id.clone(),
            &["task_id"],
        ),
        definition(
            "task_update",
            "[experimental] Record a status/output for a board item (recording only).",
            json!({"task_id":{"type":"string"},"status":{"enum":["pending","in_progress","completed","cancelled"]},"output":{"type":"string"}}),
            &["task_id"],
        ),
        definition(
            "task_stop",
            "[experimental] Stop a background agent by id, or mark a board item cancelled (recording only).",
            id,
            &["task_id"],
        ),
        definition(
            "team_create",
            "[experimental] Create a team and run its members now as concurrent sub-agents (agent = type, default general); returns each member's outcome.",
            json!({"name":{"type":"string"},"members":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"object","properties":{
                "agent":{"type":"string"},"task":{"type":"string"}},"required":["task"],"additionalProperties":false}},
                "max_agents":{"type":"integer","minimum":1,"maximum":8}}),
            &["name", "members"],
        ),
        definition(
            "team_delete",
            "[experimental] Remove a team and its member records from the board.",
            json!({"name":{"type":"string"}}),
            &["name"],
        ),
        definition(
            "send_message",
            "[experimental] Send a message to a background agent id (queued at its next tool round, or resumes it), a pending execute_plan item id (added to its prompt), or \"user\" (shown on the terminal).",
            json!({"to":{"type":"string"},"message":{"type":"string"}}),
            &["to", "message"],
        ),
        definition(
            "brief",
            "[experimental] Summary of the board, sub-agent activity and the latest plan.",
            json!({"what":{"type":"string"}}),
            &[],
        ),
        definition(
            "sleep",
            "[experimental] Wait 1-300 seconds (cancellable).",
            json!({"seconds":{"type":"integer","minimum":1,"maximum":300}}),
            &["seconds"],
        ),
    ]
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Member {
    #[serde(default)]
    agent: Option<String>,
    task: String,
}

fn text(content: impl Into<String>) -> Result<ToolOutput> {
    Ok(ToolOutput {
        content: content.into(),
        failed: false,
        ..Default::default()
    })
}

fn failed(content: impl Into<String>) -> Result<ToolOutput> {
    Ok(ToolOutput {
        content: content.into(),
        failed: true,
        ..Default::default()
    })
}

fn field<'a>(input: &'a Value, name: &str) -> Option<&'a str> {
    input.get(name).and_then(Value::as_str).map(str::trim)
}

pub async fn execute(
    hub: &Arc<AgentHub>,
    name: &str,
    input: Value,
    cancel: CancellationToken,
) -> Result<ToolOutput> {
    let result = run(hub, name, input, cancel).await;
    match result {
        Err(err @ (Error::Approval(_) | Error::Cancelled | Error::Uncertain(_))) => Err(err),
        Err(err) => failed(err.to_string()),
        ok => ok,
    }
}

async fn run(
    hub: &Arc<AgentHub>,
    name: &str,
    input: Value,
    cancel: CancellationToken,
) -> Result<ToolOutput> {
    let board = &hub.board;
    let lock = || {
        board
            .inner
            .lock()
            .map_err(|_| Error::Tool("board lock poisoned".into()))
    };
    match name {
        "task" => {
            let (Some(title), Some(description)) =
                (field(&input, "title"), field(&input, "description"))
            else {
                return failed("title and description are required");
            };
            if title.is_empty() || title.len() > 200 || description.len() > 4000 {
                return failed("title 1..200 bytes, description up to 4000");
            }
            let mut inner = lock()?;
            if inner.tasks.len() >= 200 {
                return failed("the board holds at most 200 items");
            }
            inner.counter += 1;
            let id = format!("task-{}", inner.counter);
            inner.tasks.insert(
                id.clone(),
                TaskRecord {
                    title: title.into(),
                    description: description.into(),
                    status: "pending".into(),
                    team: None,
                    output: String::new(),
                },
            );
            text(format!(
                "Recorded {id} (pending). This only records state: nothing runs it. Do the work yourself or delegate it with agent, then task_update."
            ))
        }
        "task_list" => {
            let inner = lock()?;
            if inner.tasks.is_empty() {
                return text("The board is empty.");
            }
            let mut out = format!("{} board items (recorded state):\n", inner.tasks.len());
            for (id, task) in &inner.tasks {
                out.push_str(&format!(
                    "- {id} [{}]{} {}\n",
                    task.status,
                    task.team
                        .as_ref()
                        .map_or(String::new(), |team| format!(" team={team}")),
                    clip(&task.title, 100)
                ));
            }
            text(out)
        }
        "task_get" | "task_output" => {
            let id = field(&input, "task_id").unwrap_or_default();
            let inner = lock()?;
            let Some(task) = inner.tasks.get(id) else {
                return failed(format!("no board item {id}"));
            };
            if name == "task_output" {
                return text(if task.output.is_empty() {
                    format!("{id} has no output yet (status {})", task.status)
                } else {
                    task.output.clone()
                });
            }
            text(format!(
                "{id}\n  title: {}\n  status: {}\n  description: {}\n  output: {}",
                task.title,
                task.status,
                task.description,
                clip(&task.output, 500)
            ))
        }
        "task_update" => {
            let id = field(&input, "task_id").unwrap_or_default();
            let mut inner = lock()?;
            let Some(task) = inner.tasks.get_mut(id) else {
                return failed(format!("no board item {id}"));
            };
            if let Some(status) = field(&input, "status") {
                if !matches!(
                    status,
                    "pending" | "in_progress" | "completed" | "cancelled"
                ) {
                    return failed("invalid status");
                }
                task.status = status.into();
            }
            if let Some(output) = field(&input, "output") {
                task.output = clip(output, 8000);
            }
            text(format!(
                "{id} is now {} (recorded only; this does not run or verify anything)",
                task.status
            ))
        }
        "task_stop" => {
            let id = field(&input, "task_id").unwrap_or_default();
            if let Ok(record) = hub.find_record(id)
                && record.kind == Kind::Background
            {
                return text(hub.stop_agent(&record.id, false).await?);
            }
            let mut inner = lock()?;
            let Some(task) = inner.tasks.get_mut(id) else {
                return failed(format!("no board item or background agent {id}"));
            };
            task.status = "cancelled".into();
            text(format!(
                "{id} marked cancelled (recorded only; nothing was running it)"
            ))
        }
        "team_create" => {
            let name = field(&input, "name").unwrap_or_default().to_owned();
            if name.is_empty()
                || name.len() > 40
                || !name
                    .chars()
                    .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_'))
            {
                return failed("team name: 1..40 letters, digits, '-' or '_'");
            }
            let members: Vec<Member> =
                serde_json::from_value(input.get("members").cloned().unwrap_or(Value::Null))
                    .map_err(|err| Error::Tool(format!("members: {err}")))?;
            if members.is_empty() || members.len() > 8 {
                return failed("a team has 1..8 members");
            }
            let max = input
                .get("max_agents")
                .and_then(Value::as_u64)
                .unwrap_or(4)
                .clamp(1, 8) as usize;
            let mut tasks = Vec::new();
            for (index, member) in members.iter().enumerate() {
                let kind = hub
                    .types
                    .get(member.agent.as_deref().unwrap_or("general"))?;
                if member.task.trim().is_empty() || member.task.len() > 16 * 1024 {
                    return failed("each member needs a task of 1..16384 bytes");
                }
                tasks.push(PlanTask {
                    id: format!("team-{name}-{}", index + 1),
                    title: clip(&member.task, 100),
                    prompt: member.task.clone(),
                    depends: Vec::new(),
                    agent_type: kind.clone(),
                });
            }
            {
                let mut inner = lock()?;
                if inner.teams.contains_key(&name) {
                    return failed(format!("team {name} exists; team_delete it first"));
                }
                inner.teams.insert(
                    name.clone(),
                    tasks.iter().map(|task| task.id.clone()).collect(),
                );
                for task in &tasks {
                    inner.tasks.insert(
                        task.id.clone(),
                        TaskRecord {
                            title: task.title.clone(),
                            description: task.prompt.clone(),
                            status: "in_progress".into(),
                            team: Some(name.clone()),
                            output: String::new(),
                        },
                    );
                }
            }
            let report = run_tasks(hub, &format!("team {name}"), tasks, max, cancel).await;
            if let Ok(mut inner) = lock() {
                let ids = inner.teams.get(&name).cloned().unwrap_or_default();
                for id in ids {
                    if let Some(task) = inner.tasks.get_mut(&id) {
                        task.status = if report.done.contains(&id) {
                            "completed".into()
                        } else {
                            "failed".into()
                        };
                    }
                }
            }
            if let Some(err) = report.fatal {
                return Err(err);
            }
            Ok(ToolOutput {
                content: report.text,
                failed: !report.all_done,
                ..Default::default()
            })
        }
        "team_delete" => {
            let name = field(&input, "name").unwrap_or_default();
            let mut inner = lock()?;
            let Some(ids) = inner.teams.remove(name) else {
                return failed(format!("no team {name}"));
            };
            for id in &ids {
                inner.tasks.remove(id);
            }
            text(format!(
                "team {name} and {} member records removed from the board (recorded state only)",
                ids.len()
            ))
        }
        "send_message" => {
            let (Some(to), Some(message)) = (field(&input, "to"), field(&input, "message")) else {
                return failed("to and message are required");
            };
            if message.is_empty() || message.len() > 8000 {
                return failed("message must be 1..8000 bytes");
            }
            if to == "user" {
                hub.progress
                    .line(format!("[给用户的消息] {message}"), false)
                    .await;
                if let Ok(mut inner) = lock() {
                    inner.user_messages.push(message.into());
                }
                return text("shown to the user on the terminal");
            }
            if to.len() >= 4
                && let Ok(record) = hub.find_record(to)
                && record.kind == Kind::Background
            {
                let remaining = hub.ledger.remaining_budget();
                return text(
                    hub.send_agent(&record.id, message.into(), remaining)
                        .await?,
                );
            }
            let pending_item = hub
                .plan
                .latest()
                .and_then(|latest| crate::agents::plan::parse(&latest).ok())
                .is_some_and(|items| {
                    items
                        .iter()
                        .any(|item| item.id == to && item.status == "pending")
                });
            if pending_item {
                hub.inbox.push(to, message.into());
                return text(format!(
                    "queued for plan item {to}; added to its prompt when execute_plan runs it"
                ));
            }
            let mut inner = lock()?;
            if let Some(task) = inner.tasks.get_mut(to) {
                task.output.push_str(&format!("\nmessage: {message}"));
                return text(format!(
                    "recorded on board item {to} (recording only; no agent is running it)"
                ));
            }
            if inner.teams.contains_key(to) {
                return text(format!(
                    "team {to} already ran when it was created; the message is not delivered to anyone"
                ))
                .map(|mut output| {
                    output.failed = true;
                    output
                });
            }
            failed(format!(
                "unknown recipient {to}: use a background agent id, a pending plan item id, or user"
            ))
        }
        "brief" => {
            let mut out = format!(
                "Brief: {}\n",
                field(&input, "what")
                    .filter(|what| !what.is_empty())
                    .unwrap_or("current session")
            );
            out.push_str(&hub.status_text(None).await);
            {
                let inner = lock()?;
                out.push_str(&format!(
                    "board: {} items, {} teams, {} messages to the user\n",
                    inner.tasks.len(),
                    inner.teams.len(),
                    inner.user_messages.len()
                ));
            }
            if let Some(items) = hub
                .plan
                .latest()
                .and_then(|latest| crate::agents::plan::parse(&latest).ok())
            {
                out.push_str("latest plan:\n");
                for item in items {
                    out.push_str(&format!(
                        "- {} [{}] {}\n",
                        item.id,
                        item.status,
                        clip(&item.title, 100)
                    ));
                }
            }
            text(out)
        }
        "sleep" => {
            let seconds = input.get("seconds").and_then(Value::as_u64).unwrap_or(0);
            if !(1..=300).contains(&seconds) {
                return failed("seconds must be 1..300");
            }
            tokio::select! {
                _ = cancel.cancelled() => Err(Error::Cancelled),
                _ = tokio::time::sleep(Duration::from_secs(seconds)) => text(format!("slept {seconds} seconds")),
            }
        }
        other => failed(format!("unknown tool {other}")),
    }
}
