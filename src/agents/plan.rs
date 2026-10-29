//! `execute_plan`: runs the pending items of the latest `todo` list as sub-agents.
//!
//! The list comes from the latest successful `todo` call (recorded as it passes through the
//! tool layer, and re-read from the session transcript at each task start). Items declare
//! dependencies with `id`/`depends` or the `depends:todo-1,todo-2 ` content
//! prefix. Ready items run as soon as their dependencies are done (a DAG schedule, not BFS
//! levels), up to `max_agents` at once; a failed item is retried once and its dependents
//! are skipped. Concurrent items never write the same file at once (`Claims`).

use std::{
    collections::{BTreeSet, HashMap},
    sync::{Arc, Mutex},
};

use serde::Deserialize;
use serde_json::Value;
use tokio::task::JoinSet;
use tokio_util::sync::CancellationToken;

use crate::{
    agents::subagent::{AgentHub, AgentType, Claims, Exit, RunOutcome, RunRequest, clip},
    domain::{Block, Error, Message, Result},
    tools::ToolOutput,
};

#[derive(Default)]
pub struct PlanState {
    inner: Mutex<PlanInner>,
}

#[derive(Default)]
struct PlanInner {
    latest: Option<Value>,
    /// Items completed by an earlier execute_plan in this process: id -> content.
    done: HashMap<String, String>,
    runs: u32,
}

impl PlanState {
    pub fn record(&self, todo: Value) {
        if let Ok(mut inner) = self.inner.lock() {
            inner.latest = Some(todo);
        }
    }

    /// Re-reads the latest successful `todo` call from a transcript.
    pub fn seed(&self, messages: &[Message]) {
        let mut ok = HashMap::new();
        for block in messages.iter().flat_map(|message| &message.blocks) {
            if let Block::ToolResult { id, is_error, .. } = block {
                ok.insert(id.clone(), !is_error);
            }
        }
        let latest = messages
            .iter()
            .flat_map(|message| &message.blocks)
            .filter_map(|block| match block {
                Block::ToolUse {
                    id, name, input, ..
                } if name == "todo" && ok.get(id) == Some(&true) => Some(input.clone()),
                _ => None,
            })
            .next_back();
        if let (Some(latest), Ok(mut inner)) = (latest, self.inner.lock()) {
            inner.latest = Some(latest);
        }
    }

    pub fn latest(&self) -> Option<Value> {
        self.inner.lock().ok()?.latest.clone()
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PlanItem {
    pub id: String,
    pub title: String,
    pub status: String,
    pub depends: Vec<String>,
}

fn valid_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 64
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '-' | '_' | '.'))
}

/// Parses a `todo` input (the shared contract, plus the `depends:` content prefix).
pub fn parse(input: &Value) -> Result<Vec<PlanItem>> {
    let todos = input
        .get("todos")
        .and_then(Value::as_array)
        .ok_or_else(|| Error::Tool("the latest todo call has no todos array".into()))?;
    let prefix =
        regex::Regex::new(r"^depends\s*:(\s*)([\w\-.]+(?:\s*,\s*[\w\-.]+)*)?\s*").expect("regex");
    let declared: BTreeSet<String> = todos
        .iter()
        .enumerate()
        .map(|(index, todo)| {
            todo.get("id")
                .and_then(Value::as_str)
                .map_or_else(|| format!("todo-{}", index + 1), str::to_owned)
        })
        .collect();
    let mut items = Vec::new();
    let mut seen = BTreeSet::new();
    for (index, todo) in todos.iter().enumerate() {
        let content = todo
            .get("content")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .trim()
            .to_owned();
        if content.is_empty() {
            continue;
        }
        let id = todo
            .get("id")
            .and_then(Value::as_str)
            .map_or_else(|| format!("todo-{}", index + 1), str::to_owned);
        if !valid_id(&id) || !seen.insert(id.clone()) {
            return Err(Error::Tool(format!(
                "todo id {id:?} is invalid or repeated"
            )));
        }
        let mut depends: Vec<String> = todo
            .get("depends")
            .and_then(Value::as_array)
            .map(|list| {
                list.iter()
                    .filter_map(Value::as_str)
                    .map(|dep| dep.trim().to_owned())
                    .collect()
            })
            .unwrap_or_default();
        let mut title = content.clone();
        if let Some(found) = prefix.captures(&content) {
            let whole = found.get(0).map_or(0, |m| m.end());
            let spaced = found.get(1).is_some_and(|m| !m.as_str().is_empty());
            let list = found.get(2).map_or("", |m| m.as_str());
            // "depends: Just do it" is a bare prefix; with a space after the colon a single
            // word is a dependency only when such an item exists.
            if spaced && !list.contains(',') && !declared.contains(list) {
                title = content[content.find(':').unwrap_or(0) + 1..]
                    .trim()
                    .to_owned();
            } else if !list.is_empty() {
                depends.extend(list.split(',').map(|dep| dep.trim().to_owned()));
                title = content[whole..].trim().to_owned();
            }
        }
        if title.is_empty() {
            title = content.clone();
        }
        depends.sort();
        depends.dedup();
        items.push(PlanItem {
            id,
            title,
            status: todo
                .get("status")
                .and_then(Value::as_str)
                .unwrap_or("pending")
                .to_owned(),
            depends,
        });
    }
    Ok(items)
}

/// One node to run.
#[derive(Clone)]
pub struct PlanTask {
    pub id: String,
    pub title: String,
    pub prompt: String,
    pub depends: Vec<String>,
    pub agent_type: AgentType,
}

/// Checks dependencies (inside the plan, or already completed) and cycles.
pub fn validate(items: &[PlanItem], done: &dyn Fn(&PlanItem) -> bool) -> Result<Vec<PlanItem>> {
    let by_id: HashMap<&str, &PlanItem> =
        items.iter().map(|item| (item.id.as_str(), item)).collect();
    let pending: Vec<PlanItem> = items
        .iter()
        .filter(|item| item.status == "pending" && !done(item))
        .cloned()
        .collect();
    let in_plan: BTreeSet<&str> = pending.iter().map(|item| item.id.as_str()).collect();
    for item in &pending {
        for dep in &item.depends {
            if in_plan.contains(dep.as_str()) {
                continue;
            }
            match by_id.get(dep.as_str()) {
                None => {
                    return Err(Error::Tool(format!(
                        "{} depends on unknown item {dep}",
                        item.id
                    )));
                }
                Some(other) if other.status == "completed" || done(other) => {}
                Some(other) => {
                    return Err(Error::Tool(format!(
                        "{} depends on {dep}, which is {}, not completed",
                        item.id, other.status
                    )));
                }
            }
        }
    }
    // Cycle detection over the pending subgraph.
    fn visit<'a>(
        id: &'a str,
        graph: &HashMap<&'a str, &'a PlanItem>,
        state: &mut HashMap<&'a str, u8>,
        path: &mut Vec<&'a str>,
    ) -> Option<Vec<String>> {
        match state.get(id) {
            Some(2) => return None,
            Some(1) => {
                let start = path.iter().position(|step| *step == id).unwrap_or(0);
                let mut cycle: Vec<String> =
                    path[start..].iter().map(|s| (*s).to_owned()).collect();
                cycle.push(id.to_owned());
                return Some(cycle);
            }
            _ => {}
        }
        state.insert(id, 1);
        path.push(id);
        if let Some(item) = graph.get(id) {
            for dep in &item.depends {
                if graph.contains_key(dep.as_str())
                    && let Some(cycle) = visit(dep, graph, state, path)
                {
                    return Some(cycle);
                }
            }
        }
        path.pop();
        state.insert(id, 2);
        None
    }
    let graph: HashMap<&str, &PlanItem> = pending
        .iter()
        .map(|item| (item.id.as_str(), item))
        .collect();
    let mut state = HashMap::new();
    for item in &pending {
        if let Some(cycle) = visit(&item.id, &graph, &mut state, &mut Vec::new()) {
            return Err(Error::Tool(format!(
                "circular dependency: {}",
                cycle.join(" -> ")
            )));
        }
    }
    Ok(pending)
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum NodeState {
    Pending,
    Running,
    Done,
    Failed,
    Skipped(String),
    Cancelled,
}

struct Node {
    task: PlanTask,
    state: NodeState,
    attempts: u32,
    output: String,
    exit: Option<Exit>,
    error: String,
    steps: u64,
}

pub struct PlanReport {
    pub text: String,
    pub all_done: bool,
    pub done: Vec<String>,
    pub fatal: Option<Error>,
}

/// Runs `tasks` in dependency order with at most `concurrency` at once.
pub async fn run_tasks(
    hub: &Arc<AgentHub>,
    label: &str,
    tasks: Vec<PlanTask>,
    concurrency: usize,
    cancel: CancellationToken,
) -> PlanReport {
    let concurrency = concurrency.clamp(1, 8);
    let claims = (concurrency > 1).then(|| Arc::new(Claims::default()));
    let mut nodes: Vec<Node> = tasks
        .into_iter()
        .map(|task| Node {
            task,
            state: NodeState::Pending,
            attempts: 0,
            output: String::new(),
            exit: None,
            error: String::new(),
            steps: 0,
        })
        .collect();
    let index: HashMap<String, usize> = nodes
        .iter()
        .enumerate()
        .map(|(position, node)| (node.task.id.clone(), position))
        .collect();
    let mut running: JoinSet<(usize, RunOutcome)> = JoinSet::new();
    let mut fatal = None;
    loop {
        // Skip items whose dependencies did not succeed, transitively.
        loop {
            let mut changed = false;
            for position in 0..nodes.len() {
                if nodes[position].state != NodeState::Pending {
                    continue;
                }
                let blocker = nodes[position].task.depends.iter().find(|dep| {
                    index.get(*dep).is_some_and(|&other| {
                        matches!(
                            nodes[other].state,
                            NodeState::Failed | NodeState::Skipped(_) | NodeState::Cancelled
                        )
                    })
                });
                if let Some(blocker) = blocker.cloned() {
                    nodes[position].state =
                        NodeState::Skipped(format!("dependency {blocker} did not succeed"));
                    changed = true;
                }
            }
            if !changed {
                break;
            }
        }
        if cancel.is_cancelled() || fatal.is_some() {
            for node in nodes
                .iter_mut()
                .filter(|node| node.state == NodeState::Pending)
            {
                node.state = NodeState::Cancelled;
            }
        }
        while running.len() < concurrency && !cancel.is_cancelled() && fatal.is_none() {
            let ready = nodes.iter().position(|node| {
                node.state == NodeState::Pending
                    && node.task.depends.iter().all(|dep| {
                        index
                            .get(dep)
                            .is_none_or(|&other| nodes[other].state == NodeState::Done)
                    })
            });
            let Some(position) = ready else { break };
            let budget = hub
                .ledger
                .remaining_budget()
                .map(|budget| budget / (running.len() as f64 + 1.0));
            if budget.is_some_and(|budget| budget <= 0.0) {
                nodes[position].state = NodeState::Failed;
                nodes[position].error = "session budget exhausted; not started".into();
                continue;
            }
            nodes[position].state = NodeState::Running;
            let task = nodes[position].task.clone();
            let mut prompt = format!(
                "You are item {} of {label}: {}\n\n{}",
                task.id, task.title, task.prompt
            );
            for dep in &task.depends {
                if let Some(&other) = index.get(dep) {
                    prompt.push_str(&format!(
                        "\n\n[result of {dep}]\n{}",
                        clip(&nodes[other].output, 2000)
                    ));
                }
            }
            let node = &mut nodes[position];
            for message in hub.inbox.take(&node.task.id) {
                prompt.push_str(&format!("\n\n[message for this item]\n{message}"));
            }
            if node.attempts > 0 {
                prompt.push_str(&format!(
                    "\n\n[previous attempt failed: {}]\nInspect what it left behind, fix the cause and finish the item.",
                    clip(&node.error, 1500)
                ));
            }
            node.attempts += 1;
            let request = RunRequest {
                id: node.task.id.clone(),
                kind: "plan",
                agent_type: node.task.agent_type.clone(),
                description: node.task.title.clone(),
                prompt,
                budget,
                worktree: None,
                claims: claims
                    .as_ref()
                    .map(|claims| (claims.clone(), node.task.id.clone())),
            };
            let hub = hub.clone();
            let cancel = cancel.clone();
            running.spawn(async move { (position, hub.run(request, cancel).await) });
        }
        let Some(joined) = running.join_next().await else {
            break;
        };
        let (position, outcome) = match joined {
            Ok(joined) => joined,
            Err(err) => {
                fatal.get_or_insert(Error::Tool(format!("plan task panicked: {err}")));
                continue;
            }
        };
        if let Some(claims) = &claims {
            claims.release(&outcome.id);
        }
        let node = &mut nodes[position];
        node.steps += outcome.steps;
        node.exit = Some(outcome.exit.clone());
        node.output = outcome.output.clone();
        if let Some(err) = outcome.fatal {
            node.state = NodeState::Cancelled;
            node.error = err.to_string();
            if !matches!(err, Error::Cancelled) || fatal.is_none() {
                fatal = Some(err);
            }
            continue;
        }
        if outcome.succeeded() {
            node.state = NodeState::Done;
            node.error.clear();
            continue;
        }
        node.error = outcome
            .exit
            .detail()
            .map(str::to_owned)
            .unwrap_or_else(|| format!("exit {}", outcome.exit.label()));
        if outcome.tools_failed > 0 && outcome.exit == Exit::Completed {
            node.error = format!("{} tool calls failed", outcome.tools_failed);
        }
        // A capped run would redo the same work and hit the same cap; keep its result.
        let retry = node.attempts < 2
            && !matches!(
                outcome.exit,
                Exit::MaxIterations | Exit::Budget | Exit::Interrupted
            )
            && !cancel.is_cancelled();
        node.state = if retry {
            NodeState::Pending
        } else {
            NodeState::Failed
        };
    }
    let mut text = format!("{label} executed (up to {concurrency} at once):\n");
    let mut counts: [usize; 4] = [0; 4];
    let mut done = Vec::new();
    for node in &nodes {
        let (mark, status) = match &node.state {
            NodeState::Done => {
                counts[0] += 1;
                done.push(node.task.id.clone());
                ("[done]", "done".to_owned())
            }
            NodeState::Failed => {
                counts[1] += 1;
                ("[failed]", format!("failed: {}", clip(&node.error, 300)))
            }
            NodeState::Skipped(reason) => {
                counts[2] += 1;
                ("[skipped]", format!("skipped: {reason}"))
            }
            NodeState::Cancelled | NodeState::Pending | NodeState::Running => {
                counts[3] += 1;
                (
                    "[cancelled]",
                    "cancelled; still pending, execute_plan again resumes it".to_owned(),
                )
            }
        };
        text.push_str(&format!(
            "  {mark} [{}] {} - {status} (exit: {}, attempts: {}, steps: {})\n",
            node.task.id,
            clip(&node.task.title, 100),
            node.exit.as_ref().map_or("not run", Exit::label),
            node.attempts,
            node.steps
        ));
        if !node.output.trim().is_empty() {
            text.push_str(&format!(
                "    result: {}\n",
                clip(&node.output, 1500).replace('\n', "\n    ")
            ));
        }
    }
    text.push_str(&format!(
        "Summary: {} done, {} failed, {} skipped, {} cancelled.",
        counts[0], counts[1], counts[2], counts[3]
    ));
    PlanReport {
        all_done: counts[0] == nodes.len(),
        text,
        done,
        fatal,
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ExecuteArgs {
    #[serde(default)]
    parallel: bool,
    #[serde(default)]
    max_agents: Option<u64>,
    #[serde(default, alias = "agent_type")]
    r#type: Option<String>,
}

pub async fn execute(
    hub: &Arc<AgentHub>,
    input: Value,
    cancel: CancellationToken,
) -> Result<ToolOutput> {
    let failed = |content: String| {
        Ok(ToolOutput {
            content,
            failed: true,
            ..Default::default()
        })
    };
    let args: ExecuteArgs = match serde_json::from_value(input) {
        Ok(args) => args,
        Err(err) => return failed(err.to_string()),
    };
    if args.max_agents.is_some_and(|max| !(1..=8).contains(&max)) {
        return failed("max_agents must be 1..8".into());
    }
    let Some(latest) = hub.plan.latest() else {
        return failed("no todo list yet; write the plan with the todo tool first".into());
    };
    let kind = match hub.types.get(args.r#type.as_deref().unwrap_or("general")) {
        Ok(kind) => kind.clone(),
        Err(err) => return failed(err.to_string()),
    };
    let (done_before, run) = {
        let mut inner = hub
            .plan
            .inner
            .lock()
            .map_err(|_| Error::Tool("plan lock poisoned".into()))?;
        inner.runs += 1;
        (inner.done.clone(), inner.runs)
    };
    let items = match parse(&latest) {
        Ok(items) => items,
        Err(err) => return failed(err.to_string()),
    };
    let pending = match validate(&items, &|item| {
        done_before
            .get(&item.id)
            .is_some_and(|title| *title == item.title)
    }) {
        Ok(pending) => pending,
        Err(err) => return failed(err.to_string()),
    };
    if pending.is_empty() {
        return failed(
            "no pending todo items to run (items done by an earlier execute_plan count as done; update the todo list)".into(),
        );
    }
    let titles: HashMap<String, String> = pending
        .iter()
        .map(|item| (item.id.clone(), item.title.clone()))
        .collect();
    let tasks = pending
        .into_iter()
        .map(|item| PlanTask {
            prompt: "Complete this item fully within the plan's scope; other items may run at the same time, so touch only what this item needs."
                .into(),
            id: item.id,
            title: item.title,
            depends: item.depends,
            agent_type: kind.clone(),
        })
        .collect();
    let concurrency = if args.parallel {
        args.max_agents.unwrap_or(4) as usize
    } else {
        1
    };
    let report = run_tasks(hub, &format!("plan {run}"), tasks, concurrency, cancel).await;
    if let Ok(mut inner) = hub.plan.inner.lock() {
        for id in &report.done {
            if let Some(title) = titles.get(id) {
                inner.done.insert(id.clone(), title.clone());
            }
        }
    }
    if let Some(err) = report.fatal {
        return Err(err);
    }
    Ok(ToolOutput {
        content: format!(
            "{}\nUpdate the todo list: mark done items completed; failed or skipped items stay pending.",
            report.text
        ),
        failed: !report.all_done,
        ..Default::default()
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parses_ids_depends_and_the_content_prefix() {
        let items = parse(&json!({"todos":[
            {"content":"scaffold","status":"completed"},
            {"content":"depends:todo-1 write parser","status":"pending"},
            {"content":"tests","status":"pending","id":"t","depends":["todo-2"]},
            {"content":"depends: Just do it","status":"pending"},
            {"content":"depends:todo-2, t docs","status":"pending"}
        ]}))
        .unwrap();
        assert_eq!(items[1].depends, ["todo-1"]);
        assert_eq!(items[1].title, "write parser");
        assert_eq!(items[2].id, "t");
        assert_eq!(items[2].depends, ["todo-2"]);
        assert!(items[3].depends.is_empty());
        assert_eq!(items[3].title, "Just do it");
        assert_eq!(items[4].depends, ["t", "todo-2"]);
        assert_eq!(items[4].title, "docs");
        let pending = validate(&items, &|_| false).unwrap();
        assert_eq!(pending.len(), 4);
    }

    #[test]
    fn rejects_unknown_unfinished_and_cyclic_dependencies() {
        let unknown =
            parse(&json!({"todos":[{"content":"depends:nope x","status":"pending"}]})).unwrap();
        assert!(validate(&unknown, &|_| false).is_err());
        let unfinished = parse(&json!({"todos":[
            {"content":"a","status":"in_progress"},
            {"content":"b","status":"pending","depends":["todo-1"]}
        ]}))
        .unwrap();
        assert!(
            validate(&unfinished, &|_| false)
                .unwrap_err()
                .to_string()
                .contains("in_progress")
        );
        let cycle = parse(&json!({"todos":[
            {"content":"a","status":"pending","id":"a","depends":["b"]},
            {"content":"b","status":"pending","id":"b","depends":["a"]}
        ]}))
        .unwrap();
        assert!(
            validate(&cycle, &|_| false)
                .unwrap_err()
                .to_string()
                .contains("circular")
        );
        let repeated = json!({"todos":[{"content":"a","status":"pending","id":"x"},{"content":"b","status":"pending","id":"x"}]});
        assert!(parse(&repeated).is_err());
    }

    #[test]
    fn seeds_the_latest_successful_todo_call() {
        let use_block = |id: &str, content: &str| Block::ToolUse {
            id: id.into(),
            name: "todo".into(),
            input: json!({"todos":[{"content":content,"status":"pending"}]}),
            extra: None,
        };
        let result = |id: &str, is_error| Block::ToolResult {
            id: id.into(),
            content: String::new(),
            is_error,
        };
        let messages = vec![
            Message {
                role: crate::domain::Role::Assistant,
                blocks: vec![use_block("1", "first"), use_block("2", "broken")],
            },
            Message {
                role: crate::domain::Role::User,
                blocks: vec![result("1", false), result("2", true)],
            },
        ];
        let state = PlanState::default();
        state.seed(&messages);
        assert_eq!(state.latest().unwrap()["todos"][0]["content"], "first");
    }
}
