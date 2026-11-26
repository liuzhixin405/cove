//! `execute_plan {isolation: "overlay"}` (docs/optimizations/0053): every item works in a
//! private overlay layer, nothing reaches the main workspace while items run, and the
//! layers are merged into it afterwards, in dependency order, as one checkpoint.
//!
//! - Schedule: items whose writes share no file run side by side; so do items sharing a
//!   file through distinct `::symbol`s. Any other overlap adds a dependency (the later
//!   item builds on the earlier one's layer), reported in the result.
//! - Layers: `<data dir>/overlays/<plan>/<item>/`; an item's stack is the layers of the
//!   items it depends on (directly or not, in dependency order) under its own. When two
//!   of those items are independent and changed the same file, a combined layer
//!   (`<item>-combined/`, filled just before the item starts) holds their versions merged
//!   with the same engine, so the item sees both; when they do not merge the item is
//!   deferred (skipped, still pending) to run on the merged workspace later.
//! - Merge: for each path a layer changed, `session::merge::merge_file` with the content
//!   the layer started from, the workspace's current content and the layer's result;
//!   writes go through the task's checkpoint. A conflicting item redoes its part once on
//!   the merged content (a new layer limited to the conflicting files); what still
//!   conflicts is not written: a copy with conflict markers goes to `conflicts/`.
//! - Only items whose layer merged completely count as done. When the plan stops (an error,
//!   or Esc while items run, during a redo or a mergiraf run), the report still reaches the
//!   model: what was written (one checkpoint, `/undo` reverts it) and what was not.
//! - Clean-up: the plan's directory is removed when everything merged; after conflicts or
//!   failed items it stays until the task ends (`OverlayCleanup`); when the plan stopped
//!   with finished but unmerged items it is kept for recovery (the result names it) for
//!   [`KEPT_DAYS`] days; [`prune_kept`] removes older plan directories at startup.

use std::{
    collections::{BTreeMap, BTreeSet, HashMap, HashSet},
    path::{Path, PathBuf},
    sync::Arc,
};

use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;

use crate::{
    agents::{
        plan::{BeforeStart, NotStarted, PlanItem, PlanReport, PlanTask, run_tasks},
        subagent::{AgentHub, AgentType},
        writes::{Implicit, Unit, WriteSet, implicit_depends},
    },
    domain::{Error, Result},
    engine::turn::TurnHook,
    safety::permission::Mode,
    session::{
        Session,
        merge::{EditIntent, FileMerge, Strategy, merge_file_using},
        overlay::{self, Content, Intent, Overlay, OverlayStack},
    },
};

/// Fixed cost of one item, as the task-split skill puts it: a sub-agent reads its
/// context again (about 10k tokens).
const TOKENS_PER_ITEM: u64 = 10_000;
/// Days a plan directory kept for recovery stays before [`prune_kept`] removes it (the
/// same retention as `tool-outputs/`).
pub const KEPT_DAYS: u64 = 7;
/// Lines of context around a conflicting region in the redo prompt.
const CONTEXT_LINES: usize = 3;

/// A plan ready to run in overlays: the items, their writes and dependencies (declared
/// plus the schedule's).
pub struct Prepared {
    pub items: Vec<PlanItem>,
    pub writes: Vec<WriteSet>,
    /// Per item: declared dependencies inside the plan plus implicit ones.
    pub depends: Vec<Vec<String>>,
    pub implicit: Vec<Implicit>,
}

impl Prepared {
    /// Implicit dependencies as `todo-3 after todo-1 (src/lib.rs)`.
    pub fn implicit_lines(&self) -> Vec<String> {
        self.implicit
            .iter()
            .map(|implicit| {
                format!(
                    "{} after {}: both write {} without distinct ::symbols, so {}'s layer is stacked on {}'s",
                    self.items[implicit.later].id,
                    self.items[implicit.earlier].id,
                    implicit.files.join(", "),
                    self.items[implicit.later].id,
                    self.items[implicit.earlier].id
                )
            })
            .collect()
    }
}

/// Computes the overlay schedule of `items` (every one with writes).
pub async fn prepare(root: &Path, items: Vec<PlanItem>, writes: Vec<WriteSet>) -> Prepared {
    let root = root.to_path_buf();
    let ids: HashSet<String> = items.iter().map(|item| item.id.clone()).collect();
    // Dependencies on items outside this run (completed earlier) are already satisfied.
    let declared: Vec<Vec<String>> = items
        .iter()
        .map(|item| {
            item.depends
                .iter()
                .filter(|dep| ids.contains(*dep))
                .cloned()
                .collect()
        })
        .collect();
    let (items, writes, declared, implicit) = tokio::task::spawn_blocking(move || {
        let units: Vec<Unit> = items
            .iter()
            .zip(&declared)
            .zip(&writes)
            .map(|((item, depends), writes)| Unit {
                id: &item.id,
                depends,
                writes: Some(writes),
            })
            .collect();
        let implicit = implicit_depends(&units, &root);
        (items, writes, declared, implicit)
    })
    .await
    .expect("overlay schedule");
    let mut depends = declared;
    for found in &implicit {
        let earlier = items[found.earlier].id.clone();
        depends[found.later].push(earlier);
    }
    Prepared {
        items,
        writes,
        depends,
        implicit,
    }
}

/// Item indices in dependency order (plan order among the ready ones).
fn topo_order(prepared: &Prepared) -> Vec<usize> {
    let index: HashMap<&str, usize> = prepared
        .items
        .iter()
        .enumerate()
        .map(|(position, item)| (item.id.as_str(), position))
        .collect();
    let mut placed = vec![false; prepared.items.len()];
    let mut order = Vec::new();
    while order.len() < prepared.items.len() {
        let next = (0..prepared.items.len()).find(|&position| {
            !placed[position]
                && prepared.depends[position]
                    .iter()
                    .all(|dep| index.get(dep.as_str()).is_none_or(|&other| placed[other]))
        });
        // `validate` rejected cycles and implicit edges never close one; stop anyway.
        let Some(position) = next else { break };
        placed[position] = true;
        order.push(position);
    }
    order
}

/// For each item, the items it depends on directly or not.
fn ancestors(prepared: &Prepared) -> Vec<BTreeSet<usize>> {
    let index: HashMap<&str, usize> = prepared
        .items
        .iter()
        .enumerate()
        .map(|(position, item)| (item.id.as_str(), position))
        .collect();
    (0..prepared.items.len())
        .map(|position| {
            let mut found = BTreeSet::new();
            let mut stack: Vec<usize> = prepared.depends[position]
                .iter()
                .filter_map(|dep| index.get(dep.as_str()).copied())
                .collect();
            while let Some(next) = stack.pop() {
                if found.insert(next) {
                    stack.extend(
                        prepared.depends[next]
                            .iter()
                            .filter_map(|dep| index.get(dep.as_str()).copied()),
                    );
                }
            }
            found
        })
        .collect()
}

/// Calibrated minutes one at a time and with `concurrency` items at once, when the
/// items carry estimates.
fn durations(prepared: &Prepared, concurrency: usize, factor: f64) -> Option<(f64, f64)> {
    let minutes: Vec<f64> = prepared
        .items
        .iter()
        .map(|item| f64::from(item.estimate_minutes.unwrap_or(0)) * factor)
        .collect();
    let serial: f64 = minutes.iter().sum();
    if serial <= 0.0 {
        return None;
    }
    let index: HashMap<&str, usize> = prepared
        .items
        .iter()
        .enumerate()
        .map(|(position, item)| (item.id.as_str(), position))
        .collect();
    let mut finish = vec![0.0_f64; prepared.items.len()];
    for position in topo_order(prepared) {
        let start = prepared.depends[position]
            .iter()
            .filter_map(|dep| index.get(dep.as_str()))
            .map(|&dep| finish[dep])
            .fold(0.0, f64::max);
        finish[position] = start + minutes[position];
    }
    let critical = finish.iter().copied().fold(0.0, f64::max);
    Some((serial, critical.max(serial / concurrency.max(1) as f64)))
}

/// The plan as the user confirms it (Chinese, like the rest of the interface).
pub fn plan_text(
    prepared: &Prepared,
    concurrency: usize,
    factor: (f64, usize),
    prices: Option<(f64, f64)>,
) -> String {
    let mut text = String::from(
        "\n[覆盖层拆分计划] 每个条目在自己的覆盖层里改，全部结束后合并进工作区，最后统一构建和测试。\n",
    );
    for (position, item) in prepared.items.iter().enumerate() {
        let depends = if prepared.depends[position].is_empty() {
            "无".to_owned()
        } else {
            prepared.depends[position].join(", ")
        };
        let estimate = item
            .estimate_minutes
            .map_or(String::new(), |minutes| format!("，预估 {minutes} 分钟"));
        text.push_str(&format!(
            "  {} {}\n    写集合：{}；依赖：{depends}{estimate}\n",
            item.id,
            crate::text::short(&item.title, 80),
            prepared.writes[position].summary(8)
        ));
    }
    if !prepared.implicit.is_empty() {
        text.push_str(
            "自动加的依赖（写同一个文件、没有分开的 ::符号，后一个叠在前一个的层上）：\n",
        );
        for implicit in &prepared.implicit {
            text.push_str(&format!(
                "  {} 在 {} 之后：{}\n",
                prepared.items[implicit.later].id,
                prepared.items[implicit.earlier].id,
                implicit.files.join(", ")
            ));
        }
    }
    match durations(prepared, concurrency, factor.0) {
        Some((serial, parallel)) => {
            let basis = if factor.1 == 0 {
                "没有历史记录，按模型的预估".to_owned()
            } else {
                format!("按最近 {} 次的实际/预估比 {:.1} 校准", factor.1, factor.0)
            };
            text.push_str(&format!(
                "预计：串行约 {} 分钟，并行（同时最多 {concurrency} 个）约 {} 分钟（{basis}）。\n",
                serial.round() as u64,
                parallel.round() as u64
            ));
        }
        None => text.push_str("预计：条目没有写预估时长。\n"),
    }
    let tokens = TOKENS_PER_ITEM * prepared.items.len() as u64;
    let cost = prices.map_or(String::new(), |(input, _)| {
        format!("，约 ${:.2}", tokens as f64 / 1_000_000.0 * input)
    });
    text.push_str(&format!(
        "额外开销：{} 个子 Agent 各自重新读背景，约 {} 万 token{cost}；有冲突的条目会再重做一次。\n",
        prepared.items.len(),
        (tokens as f64 / 10_000.0).round() as u64
    ));
    text.push_str("选择：1 确认  2 改为普通方式（isolation=main）  3 取消 [1/2/3] ");
    text
}

/// What happens to an overlay plan.
#[derive(Debug, PartialEq, Eq)]
pub enum Decision {
    Run,
    /// Run it with `isolation: "main"`; the note goes into the result.
    Main(String),
    Cancel,
}

/// Reads the user's choice.
pub fn choice(answer: &str) -> Decision {
    match answer.trim().to_lowercase().as_str() {
        "1" | "y" | "yes" | "确认" | "是" => Decision::Run,
        "2" | "main" | "普通" | "普通方式" => Decision::Main(
            "the user chose isolation=\"main\" instead of overlay when confirming the plan".into(),
        ),
        _ => Decision::Cancel,
    }
}

/// Asks the user when a terminal is there; without one, `auto` (and `bypass`, which
/// allows everything `auto` does) runs the plan and other modes fall back to `main`.
pub async fn decide(
    hub: &Arc<AgentHub>,
    prepared: &Prepared,
    concurrency: usize,
    cancel: CancellationToken,
) -> Result<Decision> {
    match &hub.input {
        Some(input) => {
            let question = plan_text(
                prepared,
                concurrency,
                crate::engine::estimate::factor(hub.store.root()),
                hub.settings.prices,
            );
            let answer = input.answer(question, cancel).await?;
            Ok(choice(&answer))
        }
        None if matches!(hub.permission, Mode::Auto | Mode::Bypass) => Ok(Decision::Run),
        None => Ok(Decision::Main(format!(
            "isolation=\"overlay\" needs the user's confirmation, and without a terminal only the auto permission mode runs it without one (this run: {}); \
             the plan ran with isolation=\"main\" instead",
            hub.permission.name()
        ))),
    }
}

/// How one path of a layer ended.
#[derive(Clone, Debug)]
enum Outcome {
    Written(Strategy),
    /// Written from a redo's layer.
    Redone,
    /// Not written; why, and a copy for the main task (conflict markers if any).
    Unresolved {
        reason: String,
        copy: Option<Vec<u8>>,
    },
}

/// A conflict that may still be redone.
struct Pending {
    path: PathBuf,
    reason: String,
    marked: Option<Vec<u8>>,
    regions: Vec<(usize, usize)>,
    ours: Option<Vec<u8>>,
}

struct FileLine {
    item: String,
    path: PathBuf,
    outcome: Outcome,
    copy: Option<PathBuf>,
}

fn strategy_label(strategy: Strategy) -> &'static str {
    match strategy {
        Strategy::Direct => "direct write",
        Strategy::Replayed => "anchor replay",
        Strategy::LineMerged => "line merge",
        Strategy::Structured => "structural merge",
    }
}

fn slashed(path: &Path) -> String {
    path.to_string_lossy().replace('\\', "/")
}

/// The `edit` intents of `path` in `layer`, when edits are all the layer did to it.
fn edit_intents(layer: &Overlay, path: &Path) -> Vec<EditIntent> {
    let wanted = overlay::key(path);
    let intents: Vec<Intent> = layer
        .intents()
        .into_iter()
        .filter(|intent| overlay::key(intent.path()) == wanted)
        .collect();
    let mut edits = Vec::new();
    for intent in intents {
        match intent {
            Intent::Edit {
                old,
                new,
                replace_all,
                ..
            } => edits.push(EditIntent {
                old,
                new,
                replace_all,
            }),
            // A whole write or a deletion: nothing to replay.
            Intent::Write { .. } | Intent::Delete { .. } => return Vec::new(),
        }
    }
    edits
}

/// The current file's lines around `regions`, numbered, for the redo prompt.
fn excerpt(current: &[u8], regions: &[(usize, usize)]) -> String {
    let text = String::from_utf8_lossy(current);
    let lines: Vec<&str> = text.lines().collect();
    if regions.is_empty() || lines.is_empty() {
        return crate::text::clip(&text, 2000, crate::text::TRUNCATED);
    }
    let mut out = String::new();
    for &(start, end) in regions.iter().take(5) {
        let from = start.saturating_sub(1 + CONTEXT_LINES);
        let to = (end.max(start) + CONTEXT_LINES).min(lines.len());
        out.push_str(&format!("lines {}-{}:\n", from + 1, to));
        for (number, line) in lines.iter().enumerate().take(to).skip(from) {
            out.push_str(&format!("{:>5}  {line}\n", number + 1));
        }
    }
    crate::text::clip(&out, 3000, crate::text::TRUNCATED)
}

/// Merges layers into the hub's workspace and keeps the report lines.
struct Merger<'a> {
    hub: &'a Arc<AgentHub>,
    plan_dir: PathBuf,
    lines: Vec<FileLine>,
}

impl Merger<'_> {
    /// Merges every path `layer` changed, except those in `blocked` (left unresolved by
    /// a layer below). Returns the conflicts that a redo may still settle.
    async fn merge_layer(
        &mut self,
        item: &str,
        layer: &Overlay,
        blocked: &HashSet<String>,
        only: Option<&HashSet<String>>,
        cancel: &CancellationToken,
    ) -> Result<Vec<Pending>> {
        let mut pending = Vec::new();
        for change in layer.changes() {
            let key = overlay::key(&change.path);
            if only.is_some_and(|only| !only.contains(&key)) {
                continue;
            }
            let ours = match &change.content {
                Content::File(bytes) => Some(bytes.as_ref().clone()),
                Content::Deleted => None,
            };
            if blocked.contains(&key) {
                self.unresolved(
                    item,
                    &change.path,
                    "an item this one builds on left this file unmerged, so this layer's change is kept aside too".into(),
                    ours,
                );
                continue;
            }
            let base = match layer.base(&change.path) {
                Ok(base) => base,
                Err(err) => {
                    self.unresolved(
                        item,
                        &change.path,
                        format!("the layer's base copy cannot be read: {err}"),
                        ours,
                    );
                    continue;
                }
            };
            let target = self.hub.root.join(&change.path);
            let current = match std::fs::read(&target) {
                Ok(bytes) => Some(bytes),
                Err(err) if err.kind() == std::io::ErrorKind::NotFound => None,
                Err(err) => {
                    self.unresolved(
                        item,
                        &change.path,
                        format!("the workspace file cannot be read: {err}"),
                        ours,
                    );
                    continue;
                }
            };
            let intents = edit_intents(layer, &change.path);
            let merged = merge_file_using(
                self.hub.settings.mergiraf.as_deref(),
                &change.path,
                base.as_deref(),
                current.as_deref(),
                ours.as_deref(),
                &intents,
                cancel,
            )
            .await?;
            match merged {
                FileMerge::Merged { content, strategy } => {
                    match self.write(&target, current.as_deref(), &content) {
                        Ok(()) => self.lines.push(FileLine {
                            item: item.to_owned(),
                            path: change.path.clone(),
                            outcome: Outcome::Written(strategy),
                            copy: None,
                        }),
                        Err(err @ (Error::Uncertain(_) | Error::Cancelled)) => return Err(err),
                        Err(err) => self.unresolved(
                            item,
                            &change.path,
                            format!("writing the merged content failed: {err}"),
                            Some(content),
                        ),
                    }
                }
                FileMerge::Deleted { strategy } if current.is_none() => {
                    self.lines.push(FileLine {
                        item: item.to_owned(),
                        path: change.path.clone(),
                        outcome: Outcome::Written(strategy),
                        copy: None,
                    });
                }
                FileMerge::Deleted { .. } => self.unresolved(
                    item,
                    &change.path,
                    "the merge result deletes the file; deletions are not merged (the checkpoint cannot undo them), delete it yourself if that is right".into(),
                    None,
                ),
                FileMerge::Conflict(conflict) => pending.push(Pending {
                    path: change.path.clone(),
                    reason: conflict.reason,
                    marked: conflict.marked,
                    regions: conflict.regions,
                    ours,
                }),
            }
        }
        Ok(pending)
    }

    /// Writes `content` over `expected` through the task's checkpoint.
    fn write(&self, target: &Path, expected: Option<&[u8]>, content: &[u8]) -> Result<()> {
        if expected == Some(content) {
            return Ok(());
        }
        if let Some(parent) = target.parent()
            && !parent.exists()
        {
            // Recorded in the checkpoint, so /undo removes the new directories too.
            self.hub.workspace.create_dirs(parent)?;
            if !parent.canonicalize()?.starts_with(&self.hub.root) {
                return Err(Error::Tool("parent directory left the workspace".into()));
            }
        }
        self.hub.workspace.replace_bytes(target, expected, content)
    }

    fn unresolved(&mut self, item: &str, path: &Path, reason: String, copy: Option<Vec<u8>>) {
        self.lines.push(FileLine {
            item: item.to_owned(),
            path: path.to_path_buf(),
            outcome: Outcome::Unresolved { reason, copy },
            copy: None,
        });
    }

    /// Saves the copies of unresolved files under `conflicts/<item>/`.
    fn save_copies(&mut self) {
        let dir = self.plan_dir.join("conflicts");
        for line in &mut self.lines {
            if let Outcome::Unresolved {
                copy: Some(bytes), ..
            } = &line.outcome
            {
                let target = dir.join(&line.item).join(&line.path);
                if crate::persist::write_atomic(&target, bytes, false).is_ok() {
                    line.copy = Some(target);
                }
            }
        }
    }
}

/// The prompt of an item's redo.
fn redo_prompt(item: &PlanItem, conflicts: &[(Pending, Vec<u8>)], layer: &Overlay) -> String {
    let mut prompt = format!(
        "This item's changes were merged into the main workspace, except where they conflict with changes merged before it \
         (other plan items' or the user's). Redo your change in the places below on top of the current content: read the files first \
         (they hold the merged result now), keep the other changes and make your item's goal hold again. Change only these files.\n\
         Original goal: {}\n",
        item.title
    );
    for (pending, current) in conflicts {
        let path = slashed(&pending.path);
        prompt.push_str(&format!(
            "\n[conflict] {path}: {}\nCurrent content near the conflict:\n{}\n",
            crate::text::short(&pending.reason, 1200),
            excerpt(current, &pending.regions)
        ));
        let edits = edit_intents(layer, &pending.path);
        if edits.is_empty() {
            if let Some(ours) = &pending.ours {
                prompt.push_str(&format!(
                    "Your earlier version of the file (from your layer):\n{}\n",
                    crate::text::clip(&String::from_utf8_lossy(ours), 2000, crate::text::TRUNCATED)
                ));
            }
        } else {
            prompt.push_str("Your earlier edits of the file:\n");
            for edit in edits.iter().take(8) {
                prompt.push_str(&format!(
                    "- replace\n{}\n  with\n{}\n",
                    crate::text::clip(&edit.old, 600, crate::text::TRUNCATED),
                    crate::text::clip(&edit.new, 600, crate::text::TRUNCATED)
                ));
            }
        }
    }
    prompt
}

/// A layer below an item's own, for [`combine`].
struct Source {
    id: String,
    position: usize,
    layer: Arc<Overlay>,
    /// Positions of the items this source's item builds on.
    ancestors: BTreeSet<usize>,
}

/// Fills `combined` (the layer right under an item's own) with the merged versions of
/// every file that independent `sources` (bottom first) changed, so the item sees all of
/// them and not only the topmost. `Skipped` defers the item when they conflict;
/// cancelling the plan meanwhile gives `Cancelled`.
async fn combine(
    root: PathBuf,
    mergiraf: Option<PathBuf>,
    sources: Arc<Vec<Source>>,
    combined: Arc<Overlay>,
    cancel: CancellationToken,
) -> std::result::Result<(), NotStarted> {
    if cancel.is_cancelled() {
        return Err(NotStarted::Cancelled);
    }
    let mut writers: BTreeMap<String, (PathBuf, Vec<usize>)> = BTreeMap::new();
    for (index, source) in sources.iter().enumerate() {
        for path in source.layer.changed_paths() {
            writers
                .entry(overlay::key(&path))
                .or_insert_with(|| (path.clone(), Vec::new()))
                .1
                .push(index);
        }
    }
    let stack = OverlayStack::new(&root, vec![combined])
        .map_err(|err| NotStarted::Skipped(format!("the combined layer cannot open: {err}")))?;
    let content = |source: &Source, path: &Path| match source.layer.get(path) {
        Some(Content::File(bytes)) => Some(bytes.as_ref().clone()),
        _ => None,
    };
    for (path, indices) in writers.values() {
        // A writer another writer builds on is already in that one's version.
        let tops: Vec<&Source> = indices
            .iter()
            .map(|&index| &sources[index])
            .filter(|source| {
                !indices
                    .iter()
                    .any(|&other| sources[other].ancestors.contains(&source.position))
            })
            .collect();
        if tops.len() < 2 {
            continue;
        }
        let names = tops
            .iter()
            .map(|source| source.id.as_str())
            .collect::<Vec<_>>()
            .join(", ");
        let defer = |why: String| {
            NotStarted::Skipped(format!(
                "deferred: the items it builds on ({names}) both changed {} and {why}; it did not run and stays pending. \
                 Once this plan has merged, call execute_plan again to run it on the merged workspace",
                slashed(path)
            ))
        };
        let mut result = content(tops[0], path);
        for source in &tops[1..] {
            let base = source
                .layer
                .base(path)
                .map_err(|err| defer(format!("the base copy cannot be read ({err})")))?;
            let ours = content(source, path);
            let intents = edit_intents(&source.layer, path);
            let merged = merge_file_using(
                mergiraf.as_deref(),
                path,
                base.as_deref(),
                result.as_deref(),
                ours.as_deref(),
                &intents,
                &cancel,
            )
            .await
            .map_err(|err| {
                if matches!(err, Error::Cancelled) || cancel.is_cancelled() {
                    NotStarted::Cancelled
                } else {
                    defer(format!("merging them failed ({err})"))
                }
            })?;
            if cancel.is_cancelled() {
                return Err(NotStarted::Cancelled);
            }
            result = match merged {
                FileMerge::Merged { content, .. } => Some(content),
                FileMerge::Deleted { .. } => None,
                FileMerge::Conflict(conflict) => {
                    return Err(defer(format!(
                        "their versions conflict ({})",
                        crate::text::short(&conflict.reason, 300)
                    )));
                }
            };
        }
        // Through the combined layer: a retried item finds it filled already.
        let now = match stack.read(path) {
            Ok(bytes) => Some(bytes),
            Err(err) if err.kind() == std::io::ErrorKind::NotFound => None,
            Err(err) => return Err(defer(format!("the file cannot be read ({err})"))),
        };
        if now == result {
            continue;
        }
        let written = match (&result, &now) {
            (Some(bytes), _) => stack.write(path, now.as_deref(), bytes, None),
            (None, Some(now)) => stack.delete(path, now),
            (None, None) => Err(Error::Workspace("a deletion of a missing file".into())),
        };
        written.map_err(|err| defer(format!("the merged version cannot be stored ({err})")))?;
    }
    Ok(())
}

/// Runs `prepared` in overlays and merges the layers; the result text holds the item
/// report and the merge report.
pub async fn execute(
    hub: &Arc<AgentHub>,
    prepared: Prepared,
    kind: AgentType,
    concurrency: usize,
    label: &str,
    cancel: CancellationToken,
) -> PlanReport {
    let failed = |text: String| PlanReport {
        text,
        all_done: false,
        done: Vec::new(),
        fatal: None,
        records: HashMap::new(),
        partial: false,
    };
    let plan_dir = hub.store.root().join("overlays").join(format!(
        "{}-{}",
        label.replace(' ', "-"),
        crate::text::head(&crate::session::new_id(), 12)
    ));
    let mut layers = Vec::new();
    for item in &prepared.items {
        match Overlay::open(plan_dir.join(&item.id)) {
            Ok(layer) => layers.push(Arc::new(layer)),
            Err(err) => {
                let _ = std::fs::remove_dir_all(&plan_dir);
                return failed(format!("overlay layers could not be created: {err}"));
            }
        }
    }
    let order = topo_order(&prepared);
    let rank: HashMap<usize, usize> = order
        .iter()
        .enumerate()
        .map(|(rank, &position)| (position, rank))
        .collect();
    let ancestors = ancestors(&prepared);
    let mut tasks = Vec::new();
    for (position, item) in prepared.items.iter().enumerate() {
        let mut below: Vec<usize> = ancestors[position].iter().copied().collect();
        below.sort_by_key(|other| rank.get(other).copied().unwrap_or(usize::MAX));
        let mut stack_layers: Vec<Arc<Overlay>> =
            below.iter().map(|&other| layers[other].clone()).collect();
        // Two independent items below may change one file: combine them first.
        let independent = below.iter().any(|&a| {
            below
                .iter()
                .any(|&b| a != b && !ancestors[a].contains(&b) && !ancestors[b].contains(&a))
        });
        let mut before_start: Option<BeforeStart> = None;
        if independent {
            let combined = match Overlay::open(plan_dir.join(format!("{}-combined", item.id))) {
                Ok(layer) => Arc::new(layer),
                Err(err) => {
                    let _ = std::fs::remove_dir_all(&plan_dir);
                    return failed(format!("overlay layers could not be created: {err}"));
                }
            };
            stack_layers.push(combined.clone());
            let sources: Arc<Vec<Source>> = Arc::new(
                below
                    .iter()
                    .map(|&other| Source {
                        id: prepared.items[other].id.clone(),
                        position: other,
                        layer: layers[other].clone(),
                        ancestors: ancestors[other].clone(),
                    })
                    .collect(),
            );
            let (root, mergiraf) = (hub.root.clone(), hub.settings.mergiraf.clone());
            before_start = Some(Arc::new(move |cancel| {
                Box::pin(combine(
                    root.clone(),
                    mergiraf.clone(),
                    sources.clone(),
                    combined.clone(),
                    cancel,
                ))
            }));
        }
        stack_layers.push(layers[position].clone());
        let stack = match OverlayStack::new(&hub.root, stack_layers) {
            Ok(stack) => Arc::new(stack),
            Err(err) => {
                let _ = std::fs::remove_dir_all(&plan_dir);
                return failed(format!("overlay layers could not be created: {err}"));
            }
        };
        tasks.push(PlanTask {
            id: item.id.clone(),
            title: item.title.clone(),
            prompt: "Complete this item fully within the plan's scope; other items may run at the same time, so touch only what this item needs."
                .into(),
            depends: prepared.depends[position].clone(),
            agent_type: kind.clone(),
            writes: Some(prepared.writes[position].clone()),
            overlay: Some(stack),
            before_start,
        });
    }
    let mut report = run_tasks(hub, label, tasks, concurrency, cancel.clone()).await;
    let mut text = std::mem::take(&mut report.text);
    let implicit = prepared.implicit_lines();
    text.push_str("\nIsolation: overlay (layers under ");
    text.push_str(&crate::paths::display(&plan_dir));
    text.push(')');
    if implicit.is_empty() {
        text.push_str("\nImplicit dependencies: none.");
    } else {
        text.push_str(&format!(
            "\nImplicit dependencies (added instead of rejecting the plan):\n  {}",
            implicit.join("\n  ")
        ));
    }
    if let Some(err) = &report.fatal {
        // Finished items' work is only in their layers: keep them past the task's end.
        let kept = if report.done.is_empty() {
            keep(hub, &plan_dir);
            "until this task ends"
        } else {
            RECOVERY
        };
        text.push_str(&format!(
            "\nNothing was merged: the plan stopped ({err}); the main workspace is unchanged by it and every item stays pending. \
             The layers are kept {kept}: {}",
            crate::paths::display(&plan_dir)
        ));
        report.text = text;
        report.done.clear();
        report.all_done = false;
        report.partial = true;
        return report;
    }
    let done: HashSet<String> = report.done.iter().cloned().collect();
    let mut merger = Merger {
        hub,
        plan_dir: plan_dir.clone(),
        lines: Vec::new(),
    };
    // Keys left unmerged per item, so items stacked on it keep theirs aside too.
    let mut unmerged: HashMap<usize, HashSet<String>> = HashMap::new();
    let mut fatal = None;
    let mut not_done: Vec<String> = Vec::new();
    // Items whose layer merged completely: the only ones done.
    let mut merged: Vec<String> = Vec::new();
    let mut stopped_in = None;
    for &position in &order {
        let item = &prepared.items[position];
        if !done.contains(&item.id) {
            continue;
        }
        let blocked: HashSet<String> = ancestors[position]
            .iter()
            .filter_map(|other| unmerged.get(other))
            .flatten()
            .cloned()
            .collect();
        let before = merger.lines.len();
        let result = merge_item(
            &mut merger,
            item,
            &layers[position],
            &blocked,
            &kind,
            label,
            &plan_dir,
            &cancel,
        )
        .await;
        if let Err(err) = result {
            fatal = Some(err);
            stopped_in = Some(item.id.clone());
            break;
        }
        let left: HashSet<String> = merger.lines[before..]
            .iter()
            .filter(|line| matches!(line.outcome, Outcome::Unresolved { .. }))
            .map(|line| overlay::key(&line.path))
            .collect();
        if left.is_empty() {
            for dir in [
                item.id.clone(),
                format!("{}-redo", item.id),
                format!("{}-combined", item.id),
            ] {
                let _ = std::fs::remove_dir_all(plan_dir.join(dir));
            }
            merged.push(item.id.clone());
        } else {
            not_done.push(item.id.clone());
            unmerged.insert(position, left);
        }
    }
    merger.save_copies();
    // The merge report.
    let mut counts: HashMap<&'static str, usize> = HashMap::new();
    let mut file_lines = Vec::new();
    for line in &merger.lines {
        let (label, detail) = match &line.outcome {
            Outcome::Written(strategy) => (strategy_label(*strategy), String::new()),
            Outcome::Redone => ("merged after redo", String::new()),
            Outcome::Unresolved { reason, .. } => (
                "conflict unresolved",
                format!(
                    ": {}; not written{}",
                    crate::text::short(reason, 400),
                    line.copy.as_ref().map_or(String::new(), |copy| format!(
                        ", copy for you: {}",
                        crate::paths::display(copy)
                    ))
                ),
            ),
        };
        *counts.entry(label).or_default() += 1;
        file_lines.push(format!(
            "  {} [{}]: {label}{detail}",
            slashed(&line.path),
            line.item
        ));
    }
    text.push_str("\nMerge into the main workspace (one checkpoint of this task: /diff shows it, /undo reverts it):");
    if file_lines.is_empty() {
        text.push_str("\n  nothing to merge");
    } else {
        text.push('\n');
        text.push_str(&file_lines.join("\n"));
    }
    // Writes beyond the declared sets: the layer against the writes, plus what was refused.
    let mut beyond = Vec::new();
    for (position, item) in prepared.items.iter().enumerate() {
        let mut parts: Vec<String> = layers[position]
            .changed_paths()
            .iter()
            .map(|path| slashed(path))
            .filter(|path| !prepared.writes[position].covers(path))
            .map(|path| format!("{path} (in its layer, merged)"))
            .collect();
        if let Some(record) = report.records.get(&item.id)
            && record.outside()
        {
            parts.push(record.outside_text());
        }
        if !parts.is_empty() {
            beyond.push(format!("{}: {}", item.id, parts.join("; ")));
        }
    }
    if beyond.is_empty() {
        text.push_str("\nOutside declared writes: none.");
    } else {
        text.push_str(&format!(
            "\nOutside declared writes:\n  {}",
            beyond.join("\n  ")
        ));
    }
    let skipped: Vec<&str> = prepared
        .items
        .iter()
        .filter(|item| !done.contains(&item.id))
        .map(|item| item.id.as_str())
        .collect();
    if !skipped.is_empty() {
        text.push_str(&format!(
            "\nNot merged (the item did not finish): {}; its layer is kept with the others below.",
            skipped.join(", ")
        ));
    }
    if !not_done.is_empty() {
        text.push_str(&format!(
            "\nItems with unresolved conflicts stay pending: {}. Resolve the listed files in the main workspace yourself (the copies hold conflict markers or the item's version).",
            not_done.join(", ")
        ));
    }
    if let Some(err) = &fatal {
        let unmerged: Vec<String> = order
            .iter()
            .map(|&position| &prepared.items[position].id)
            .filter(|id| done.contains(*id) && !merged.contains(id) && !not_done.contains(id))
            .map(|id| {
                format!(
                    "{id} ({})",
                    crate::paths::display(&plan_dir.join(id.as_str()))
                )
            })
            .collect();
        text.push_str(&format!(
            "\nMerging stopped: {err}\nThe main workspace holds only the files listed above as written (one checkpoint of this task: /diff shows it, /undo reverts it).",
        ));
        if let Some(id) = &stopped_in {
            text.push_str(&format!(
                " {id} was being merged when it stopped: only its files listed as written reached the workspace."
            ));
        }
        if !unmerged.is_empty() {
            text.push_str(&format!(
                "\nNot merged, still pending (their layers are kept {RECOVERY}): {}",
                unmerged.join(", ")
            ));
        }
    }
    let clean = skipped.is_empty() && not_done.is_empty() && fatal.is_none();
    if clean {
        let _ = std::fs::remove_dir_all(&plan_dir);
        text.push_str("\nOverlay layers: merged and removed.");
    } else if fatal.is_some() {
        text.push_str(&format!(
            "\nOverlay layers are kept {RECOVERY}: {}",
            crate::paths::display(&plan_dir)
        ));
    } else {
        keep(hub, &plan_dir);
        text.push_str(&format!(
            "\nOverlay layers of failed or conflicting items are kept until this task ends: {}",
            crate::paths::display(&plan_dir)
        ));
    }
    text.push_str(
        "\nNext: run the build, the tests and the review once now, on the merged workspace.",
    );
    let summary = summary(&counts, beyond.len());
    hub.progress.notice(summary).await;
    report.done = merged;
    report.all_done = report.all_done && not_done.is_empty() && fatal.is_none();
    report.partial = fatal.is_some();
    report.fatal = fatal;
    report.text = text;
    report
}

/// The interface's one-line merge summary (Chinese).
fn summary(counts: &HashMap<&'static str, usize>, beyond: usize) -> String {
    let count = |label: &str| counts.get(label).copied().unwrap_or(0);
    let written = count("direct write")
        + count("anchor replay")
        + count("line merge")
        + count("structural merge")
        + count("merged after redo");
    let mut text = format!(
        "[合并] 覆盖层合并进工作区：写入 {written} 个文件（直接写入 {}，锚点重放 {}，行级合并 {}，结构化合并 {}，重做后合并 {}）",
        count("direct write"),
        count("anchor replay"),
        count("line merge"),
        count("structural merge"),
        count("merged after redo")
    );
    let conflicts = count("conflict unresolved");
    if conflicts > 0 {
        text.push_str(&format!("；冲突未解决 {conflicts} 个，未写入"));
    }
    if beyond > 0 {
        text.push_str(&format!("；{beyond} 个条目有越界写入"));
    }
    text
}

/// Merges one item's layer, redoing its conflicts once.
#[allow(clippy::too_many_arguments)]
async fn merge_item(
    merger: &mut Merger<'_>,
    item: &PlanItem,
    layer: &Overlay,
    blocked: &HashSet<String>,
    kind: &AgentType,
    label: &str,
    plan_dir: &Path,
    cancel: &CancellationToken,
) -> Result<()> {
    let conflicts = merger
        .merge_layer(&item.id, layer, blocked, None, cancel)
        .await?;
    if conflicts.is_empty() {
        return Ok(());
    }
    let hub = merger.hub;
    // Redo on the merged content: a new layer over the workspace, limited to the files.
    let mut with_current = Vec::new();
    for pending in conflicts {
        let current = std::fs::read(hub.root.join(&pending.path)).unwrap_or_default();
        with_current.push((pending, current));
    }
    let files: Vec<String> = with_current
        .iter()
        .map(|(pending, _)| slashed(&pending.path))
        .collect();
    let redo = async {
        let writes = WriteSet::parse(&files)?;
        let layer = Arc::new(Overlay::open(plan_dir.join(format!("{}-redo", item.id)))?);
        let stack = Arc::new(OverlayStack::new(&hub.root, vec![layer.clone()])?);
        Ok::<_, Error>((writes, layer, stack))
    }
    .await;
    let (writes, redo_layer, stack) = match redo {
        Ok(parts) => parts,
        Err(err) => {
            for (pending, _) in with_current {
                let copy = pending.marked.or(pending.ours);
                merger.unresolved(
                    &item.id,
                    &pending.path,
                    format!(
                        "the redo could not start ({err}); the conflict: {}",
                        pending.reason
                    ),
                    copy,
                );
            }
            return Ok(());
        }
    };
    let task = PlanTask {
        id: format!("{}-redo", item.id),
        title: format!("redo the conflicting part of {}: {}", item.id, item.title),
        prompt: redo_prompt(item, &with_current, layer),
        depends: Vec::new(),
        agent_type: kind.clone(),
        writes: Some(writes),
        overlay: Some(stack),
        before_start: None,
    };
    let report = run_tasks(hub, &format!("{label} redo"), vec![task], 1, cancel.clone()).await;
    if let Some(err) = report.fatal {
        for (pending, _) in with_current {
            let copy = pending.marked.or(pending.ours);
            merger.unresolved(
                &item.id,
                &pending.path,
                format!("the redo stopped ({err}); the conflict: {}", pending.reason),
                copy,
            );
        }
        return Err(err);
    }
    let finished = report.all_done;
    let keys: HashSet<String> = with_current
        .iter()
        .map(|(pending, _)| overlay::key(&pending.path))
        .collect();
    let before = merger.lines.len();
    let again = if finished {
        merger
            .merge_layer(&item.id, &redo_layer, &HashSet::new(), Some(&keys), cancel)
            .await?
    } else {
        Vec::new()
    };
    // What the redo wrote counts as merged after the redo.
    for line in &mut merger.lines[before..] {
        if matches!(line.outcome, Outcome::Written(_)) {
            line.outcome = Outcome::Redone;
        }
    }
    let settled: HashSet<String> = merger.lines[before..]
        .iter()
        .map(|line| overlay::key(&line.path))
        .collect();
    let again: HashMap<String, Pending> = again
        .into_iter()
        .map(|pending| (overlay::key(&pending.path), pending))
        .collect();
    for (pending, _) in with_current {
        let key = overlay::key(&pending.path);
        if settled.contains(&key) {
            continue;
        }
        let (reason, copy) = match again.get(&key) {
            Some(later) => (
                format!("still conflicting after the redo: {}", later.reason),
                later.marked.clone().or_else(|| later.ours.clone()),
            ),
            None if finished => (
                format!(
                    "the redo did not change this file; the conflict: {}",
                    pending.reason
                ),
                pending.marked.clone().or_else(|| pending.ours.clone()),
            ),
            None => (
                format!("the redo did not finish; the conflict: {}", pending.reason),
                pending.marked.clone().or_else(|| pending.ours.clone()),
            ),
        };
        merger.unresolved(&item.id, &pending.path, reason, copy);
    }
    Ok(())
}

/// How the result describes a plan directory kept for recovery.
const RECOVERY: &str = "for recovery: not removed when the task ends, removed automatically after 7 days \
     (each item's directory has overlay.json, listing the changed paths with their content's blob, and blobs/)";

/// Removes plan directories under `<data_dir>/overlays/` untouched for [`KEPT_DAYS`] days:
/// those kept for recovery after a stopped plan (others go when their task ends).
pub fn prune_kept(data_dir: &Path) {
    prune_older_than(
        data_dir,
        std::time::Duration::from_secs(KEPT_DAYS * 24 * 3600),
    );
}

fn prune_older_than(data_dir: &Path, retention: std::time::Duration) {
    let Ok(entries) = std::fs::read_dir(data_dir.join("overlays")) else {
        return;
    };
    for entry in entries.flatten() {
        let old = entry
            .metadata()
            .ok()
            .filter(std::fs::Metadata::is_dir)
            .and_then(|meta| meta.modified().ok())
            .and_then(|modified| modified.elapsed().ok())
            .is_some_and(|age| age > retention);
        if old {
            let _ = std::fs::remove_dir_all(entry.path());
        }
    }
}

fn keep(hub: &AgentHub, dir: &Path) {
    if let Ok(mut dirs) = hub.overlay_dirs.lock() {
        dirs.push(dir.to_path_buf());
    }
}

/// Removes the overlay directories kept for a task when it ends.
pub struct OverlayCleanup {
    pub hub: Arc<AgentHub>,
}

impl OverlayCleanup {
    pub fn clean(&self) {
        let dirs = self
            .hub
            .overlay_dirs
            .lock()
            .map(|mut dirs| std::mem::take(&mut *dirs))
            .unwrap_or_default();
        for dir in dirs {
            let _ = std::fs::remove_dir_all(dir);
        }
    }
}

impl TurnHook for OverlayCleanup {
    fn task_end<'a>(&'a self, _session: &'a Session, _completed: bool) -> BoxFuture<'a, ()> {
        Box::pin(async move { self.clean() })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn item(id: &str, depends: &[&str], minutes: u32) -> PlanItem {
        PlanItem {
            id: id.into(),
            title: format!("item {id}"),
            status: "pending".into(),
            depends: depends.iter().map(|dep| (*dep).to_owned()).collect(),
            writes: Vec::new(),
            estimate_minutes: Some(minutes),
        }
    }

    fn writes(entries: &[&str]) -> WriteSet {
        WriteSet::parse(&entries.iter().map(|e| (*e).to_owned()).collect::<Vec<_>>()).unwrap()
    }

    #[tokio::test]
    async fn the_schedule_adds_implicit_dependencies_and_estimates_both_ways() {
        let root = tempfile::tempdir().unwrap();
        let prepared = prepare(
            root.path(),
            vec![
                item("a", &[], 4),
                item("b", &[], 4),
                item("c", &["done-before"], 2),
                item("d", &[], 3),
            ],
            vec![
                writes(&["src/lib.rs::one"]),
                writes(&["src/lib.rs::two"]),
                writes(&["src/lib.rs"]),
                writes(&["docs/x.md"]),
            ],
        )
        .await;
        // a and b split the file by symbols; c writes all of it, so it follows both. A
        // dependency on an item outside the run is already satisfied.
        assert!(prepared.depends[0].is_empty() && prepared.depends[1].is_empty());
        assert_eq!(prepared.depends[2], ["a", "b"]);
        assert_eq!(prepared.implicit.len(), 2);
        assert!(prepared.implicit_lines()[0].starts_with("c after a: both write src/lib.rs"));
        assert_eq!(topo_order(&prepared), [0, 1, 2, 3]);
        assert_eq!(ancestors(&prepared)[2], BTreeSet::from([0, 1]));
        // Serial 13 minutes; in parallel the critical path a -> c is 6, more than 13/4.
        assert_eq!(durations(&prepared, 4, 1.0), Some((13.0, 6.0)));
        assert_eq!(durations(&prepared, 1, 1.0), Some((13.0, 13.0)));
        let text = plan_text(&prepared, 4, (1.0, 0), Some((2.0, 8.0)));
        assert!(text.contains("c 在 a 之后：src/lib.rs"), "{text}");
        assert!(
            text.contains("串行约 13 分钟，并行（同时最多 4 个）约 6 分钟"),
            "{text}"
        );
        assert!(text.contains("约 4 万 token，约 $0.08"), "{text}");
        assert!(text.ends_with("[1/2/3] "));
    }

    #[test]
    fn the_confirmation_answers_run_switch_or_cancel() {
        assert_eq!(choice(" 1 "), Decision::Run);
        assert_eq!(choice("确认"), Decision::Run);
        assert!(matches!(choice("2"), Decision::Main(_)));
        assert_eq!(choice("3"), Decision::Cancel);
        assert_eq!(choice(""), Decision::Cancel);
    }

    #[test]
    fn the_redo_prompt_shows_the_conflicting_lines_and_the_earlier_edits() {
        let data = tempfile::tempdir().unwrap();
        let work = tempfile::tempdir().unwrap();
        std::fs::write(work.path().join("a.rs"), "x\n").unwrap();
        let layer = Arc::new(Overlay::open(data.path().join("l")).unwrap());
        let stack = OverlayStack::new(work.path(), vec![layer.clone()]).unwrap();
        stack
            .write(
                Path::new("a.rs"),
                Some(b"x\n"),
                b"y\n",
                Some(crate::session::overlay::EditSpec {
                    old: "x".into(),
                    new: "y".into(),
                    replace_all: false,
                }),
            )
            .unwrap();
        let current = b"1\n2\n3\n4\n5\n6\n7\n8\n9\n".to_vec();
        let pending = Pending {
            path: PathBuf::from("a.rs"),
            reason: "行级合并有 1 处冲突".into(),
            marked: None,
            regions: vec![(5, 5)],
            ours: Some(b"y\n".to_vec()),
        };
        let prompt = redo_prompt(&item("a", &[], 1), &[(pending, current)], &layer);
        assert!(prompt.contains("Original goal: item a"), "{prompt}");
        assert!(
            prompt.contains("[conflict] a.rs: 行级合并有 1 处冲突"),
            "{prompt}"
        );
        assert!(prompt.contains("lines 2-8:"), "{prompt}");
        assert!(prompt.contains("    5  5"), "{prompt}");
        assert!(!prompt.contains("    9  9"), "{prompt}");
        assert!(prompt.contains("- replace\nx\n  with\ny"), "{prompt}");
    }

    /// Two independent layers that both edited `a.rs` (one line each), as `combine` sources.
    fn diamond(work: &Path, data: &Path, second: (&str, &str)) -> Arc<Vec<Source>> {
        std::fs::write(work.join("a.rs"), "one\ntwo\n").unwrap();
        let mut sources = Vec::new();
        for (position, (old, new)) in [("one", "uno"), second].into_iter().enumerate() {
            let layer = Arc::new(Overlay::open(data.join(format!("l{position}"))).unwrap());
            let stack = OverlayStack::new(work, vec![layer.clone()]).unwrap();
            let next = "one\ntwo\n".replacen(old, new, 1);
            stack
                .write(
                    Path::new("a.rs"),
                    Some(b"one\ntwo\n"),
                    next.as_bytes(),
                    Some(crate::session::overlay::EditSpec {
                        old: old.into(),
                        new: new.into(),
                        replace_all: false,
                    }),
                )
                .unwrap();
            sources.push(Source {
                id: format!("todo-{}", position + 1),
                position,
                layer,
                ancestors: BTreeSet::new(),
            });
        }
        Arc::new(sources)
    }

    #[tokio::test]
    async fn combining_merges_the_layers_defers_on_a_conflict_and_honours_cancellation() {
        let work = tempfile::tempdir().unwrap();
        let data = tempfile::tempdir().unwrap();
        let root = work.path().canonicalize().unwrap();
        let sources = diamond(&root, data.path(), ("two", "dos"));
        let combined = Arc::new(Overlay::open(data.path().join("c")).unwrap());
        combine(
            root.clone(),
            None,
            sources.clone(),
            combined.clone(),
            CancellationToken::new(),
        )
        .await
        .unwrap();
        assert_eq!(
            combined.get(Path::new("a.rs")),
            Some(Content::File(Arc::new(b"uno\ndos\n".to_vec())))
        );
        // Cancelled: the item counts as cancelled, not deferred.
        let cancel = CancellationToken::new();
        cancel.cancel();
        let fresh = Arc::new(Overlay::open(data.path().join("c2")).unwrap());
        let result = combine(root.clone(), None, sources, fresh.clone(), cancel).await;
        assert!(matches!(result, Err(NotStarted::Cancelled)), "{result:?}");
        assert!(fresh.is_empty());

        let data = tempfile::tempdir().unwrap();
        let sources = diamond(&root, data.path(), ("one", "eins"));
        let combined = Arc::new(Overlay::open(data.path().join("c")).unwrap());
        let result = combine(root, None, sources, combined, CancellationToken::new()).await;
        match result {
            Err(NotStarted::Skipped(reason)) => assert!(
                reason.starts_with(
                    "deferred: the items it builds on (todo-1, todo-2) both changed a.rs"
                ),
                "{reason}"
            ),
            other => panic!("{other:?}"),
        }
    }

    #[test]
    fn kept_plan_directories_go_after_their_retention() {
        let data = tempfile::tempdir().unwrap();
        let plan = data.path().join("overlays").join("plan-1-abc");
        std::fs::create_dir_all(plan.join("todo-1")).unwrap();
        std::fs::write(plan.join("todo-1").join("overlay.json"), "{}").unwrap();
        prune_kept(data.path());
        assert!(plan.is_dir(), "a fresh directory stays");
        std::thread::sleep(std::time::Duration::from_millis(50));
        prune_older_than(data.path(), std::time::Duration::from_millis(10));
        assert!(!plan.exists());
        assert!(data.path().join("overlays").is_dir());
        assert!(
            RECOVERY.contains(&format!("after {KEPT_DAYS} days")),
            "{RECOVERY}"
        );
        // No overlays directory at all: nothing to do.
        prune_kept(tempfile::tempdir().unwrap().path());
    }
}
