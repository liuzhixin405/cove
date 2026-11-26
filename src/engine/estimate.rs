//! Plan estimates and their calibration (docs/optimizations/0054).
//!
//! Every `execute_plan` run whose finished items carry `estimate_minutes` leaves one
//! record in `<data dir>/estimates.jsonl` ([`record_plan`]): the estimates of those items
//! against the minutes they worked, added up (serial work, also for parallel runs). A
//! task that only plans records nothing. The calibration factor is the median
//! actual/estimate of the latest records. When a fresh plan's calibrated estimate exceeds
//! `split_suggest_minutes` and some of its items can run in parallel, the `todo` result
//! suggests the task-split skill: once per task, once per plan in a session, and not for a
//! plan whose items all declare writes (it is split already).

use std::{
    collections::HashSet,
    hash::{Hash, Hasher},
    path::{Path, PathBuf},
    sync::Mutex,
    time::Duration,
};

use futures_util::future::BoxFuture;
use serde::{Deserialize, Serialize};
use serde_json::Value;

use crate::{
    agents::{
        plan::PlanItem,
        subagent::AgentHub,
        writes::{Unit, WriteSet},
    },
    domain::Result,
    engine::turn::TurnHook,
    session::Session,
    tools::ToolOutput,
};

pub const FILE: &str = "estimates.jsonl";
/// `split_suggest_minutes` when not configured.
pub const SPLIT_SUGGEST_MINUTES: u32 = 5;
/// Records the calibration looks at.
const CALIBRATION_WINDOW: usize = 20;
const FACTOR_MIN: f64 = 0.5;
const FACTOR_MAX: f64 = 4.0;
/// Records kept in the file.
const KEEP: usize = 500;

/// An `estimate_minutes` value: a non-negative number of minutes, decimals rounded
/// (`2.5` is 3); `None` for anything else.
pub fn minutes(value: &Value) -> Option<u32> {
    let minutes = value.as_f64()?;
    (minutes.is_finite() && minutes >= 0.0).then(|| minutes.round().min(f64::from(u32::MAX)) as u32)
}

/// Serde reader of `estimate_minutes` for the `todo` tool: like [`minutes`], an error for
/// a negative or non-numeric value.
pub fn deserialize_minutes<'de, D>(deserializer: D) -> std::result::Result<Option<u32>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    match Option::<Value>::deserialize(deserializer)? {
        None | Some(Value::Null) => Ok(None),
        Some(value) => minutes(&value).map(Some).ok_or_else(|| {
            serde::de::Error::custom(format!(
                "estimate_minutes must be a non-negative number of minutes, not {value}"
            ))
        }),
    }
}

/// One executed plan's estimate against how long its items worked.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct EstimateRecord {
    /// When the plan run ended (UTC, RFC 3339).
    pub t: String,
    /// Sum of `estimate_minutes` over the finished items that carried one.
    pub estimate_minutes: u64,
    /// Minutes those items worked, added up.
    pub actual_minutes: f64,
    pub items: usize,
    /// `execute_plan` ran more than one item at a time.
    pub parallel: bool,
    /// Only completed records calibrate.
    pub completed: bool,
    /// Written for an executed plan. Records without it timed from the `todo` call to the
    /// end of the task, planning-only tasks included, and are not used.
    #[serde(default)]
    pub executed: bool,
}

/// All records in `dir`, oldest first; unreadable lines are skipped.
pub fn records(dir: &Path) -> Vec<EstimateRecord> {
    std::fs::read_to_string(dir.join(FILE))
        .map(|text| {
            text.lines()
                .filter_map(|line| serde_json::from_str(line).ok())
                .collect()
        })
        .unwrap_or_default()
}

/// Appends `record`, keeping the latest [`KEEP`].
pub fn append(dir: &Path, record: &EstimateRecord) -> Result<()> {
    let path = dir.join(FILE);
    crate::persist::with_lock(&path, || {
        let mut lines: Vec<String> = std::fs::read_to_string(&path)
            .map(|text| {
                text.lines()
                    .filter(|line| !line.trim().is_empty())
                    .map(str::to_owned)
                    .collect()
            })
            .unwrap_or_default();
        lines.push(serde_json::to_string(record)?);
        let skip = lines.len().saturating_sub(KEEP);
        let mut text = lines.split_off(skip).join("\n");
        text.push('\n');
        crate::persist::write_atomic(&path, text.as_bytes(), true)
    })
}

/// The median actual/estimate of the latest completed records, within 0.5–4; 1 without
/// any. Also returns how many records it used.
pub fn factor_of(records: &[EstimateRecord]) -> (f64, usize) {
    let mut ratios: Vec<f64> = records
        .iter()
        .rev()
        .filter(|record| record.completed && record.executed && record.estimate_minutes > 0)
        .take(CALIBRATION_WINDOW)
        .map(|record| record.actual_minutes / record.estimate_minutes as f64)
        .filter(|ratio| ratio.is_finite())
        .collect();
    if ratios.is_empty() {
        return (1.0, 0);
    }
    ratios.sort_by(f64::total_cmp);
    let middle = ratios.len() / 2;
    let median = if ratios.len().is_multiple_of(2) {
        (ratios[middle - 1] + ratios[middle]) / 2.0
    } else {
        ratios[middle]
    };
    (median.clamp(FACTOR_MIN, FACTOR_MAX), ratios.len())
}

/// The calibration factor from the records in `dir`.
pub fn factor(dir: &Path) -> (f64, usize) {
    factor_of(&records(dir))
}

/// Open items of a plan that could run at the same time as another open item: no
/// dependency path between them and, when both declare writes, no shared files.
pub fn parallel_items(items: &[PlanItem], root: &Path) -> usize {
    let open: Vec<&PlanItem> = items.iter().filter(|item| item.open()).collect();
    let writes: Vec<Option<WriteSet>> = open
        .iter()
        .map(|item| item.write_set().ok().flatten())
        .collect();
    let units: Vec<Unit> = open
        .iter()
        .zip(&writes)
        .map(|(item, writes)| Unit {
            id: &item.id,
            depends: &item.depends,
            writes: writes.as_ref(),
        })
        .collect();
    let listed = std::sync::OnceLock::new();
    let existing = || {
        listed
            .get_or_init(|| crate::agents::writes::workspace_files(root))
            .as_slice()
    };
    let mut able = std::collections::BTreeSet::new();
    for (a, b) in crate::agents::writes::independent_pairs(&units) {
        let clash = match (units[a].writes, units[b].writes) {
            (Some(one), Some(other)) => !one.overlap(other, &existing).is_empty(),
            _ => false,
        };
        if !clash {
            able.insert(a);
            able.insert(b);
        }
    }
    able.len()
}

/// The suggestion for a plan, if it deserves one: at least two open items, a calibrated
/// estimate over `threshold` minutes (0: never) and at least two items that can run in
/// parallel.
pub fn suggestion(
    items: &[PlanItem],
    factor: (f64, usize),
    threshold: u32,
    root: &Path,
) -> Option<String> {
    if threshold == 0 {
        return None;
    }
    let open: Vec<&PlanItem> = items.iter().filter(|item| item.open()).collect();
    let raw: u64 = open
        .iter()
        .filter_map(|item| item.estimate_minutes)
        .map(u64::from)
        .sum();
    if open.len() < 2 || raw == 0 {
        return None;
    }
    let calibrated = raw as f64 * factor.0;
    if calibrated <= f64::from(threshold) {
        return None;
    }
    let parallel = parallel_items(items, root);
    if parallel < 2 {
        return None;
    }
    let basis = if factor.1 == 0 {
        format!("{raw} min as estimated; no estimate history yet")
    } else {
        format!(
            "{raw} min estimated x {:.1}, the median actual/estimate of the last {} tasks",
            factor.0, factor.1
        )
    };
    Some(format!(
        "[task-split] Calibrated estimate for the {} open items: about {} min ({basis}); {parallel} of them can run in parallel. \
         Follow the task-split skill: show the user the split plan (units with writes, depends and estimate_minutes) and wait for confirmation, \
         then run it with execute_plan parallel=true isolation=\"overlay\" (every item needs writes; items work in private layers that are merged at the end, \
         and the build and tests run once after the merge). Keep doing it one by one only if the user prefers that.",
        open.len(),
        calibrated.round() as u64
    ))
}

/// The record of one plan run from its finished items' estimates and working times;
/// `None` when none of them had an estimate.
pub fn plan_record(worked: &[(u32, Duration)], parallel: bool) -> Option<EstimateRecord> {
    let estimated: Vec<&(u32, Duration)> = worked
        .iter()
        .filter(|(estimate, _)| *estimate > 0)
        .collect();
    if estimated.is_empty() {
        return None;
    }
    let seconds: f64 = estimated.iter().map(|(_, took)| took.as_secs_f64()).sum();
    Some(EstimateRecord {
        t: chrono::Utc::now().to_rfc3339_opts(chrono::SecondsFormat::Secs, true),
        estimate_minutes: estimated
            .iter()
            .map(|(estimate, _)| u64::from(*estimate))
            .sum(),
        actual_minutes: (seconds / 6.0).round() / 10.0,
        items: estimated.len(),
        parallel,
        completed: true,
        executed: true,
    })
}

/// Records one `execute_plan` run of `items` (the plan as it ran), `done` its finished
/// items: each one's estimate against the time of its last attempt (from the activity
/// board; earlier failed attempts are not counted).
pub async fn record_plan(hub: &AgentHub, items: &[PlanItem], done: &[String], parallel: bool) {
    let worked: Vec<(u32, Duration)> = items
        .iter()
        .filter(|item| done.contains(&item.id))
        .filter_map(|item| {
            let estimate = item.estimate_minutes?;
            let activity = hub.activity.get(&item.id)?;
            Some((estimate, activity.ended?.duration_since(activity.started)))
        })
        .collect();
    let Some(record) = plan_record(&worked, parallel) else {
        return;
    };
    let dir = hub.store.root().to_path_buf();
    let _ = tokio::task::spawn_blocking(move || append(&dir, &record)).await;
}

/// Identifies a plan by its open items' titles, in any order.
fn plan_key(items: &[PlanItem]) -> u64 {
    let mut titles: Vec<String> = items
        .iter()
        .filter(|item| item.open())
        .map(|item| item.title.trim().to_lowercase())
        .collect();
    titles.sort();
    let mut hasher = std::collections::hash_map::DefaultHasher::new();
    titles.hash(&mut hasher);
    hasher.finish()
}

#[derive(Default)]
struct Hints {
    /// A suggestion was given in the current task.
    task: bool,
    /// The session the plans below were suggested in.
    session: String,
    /// Plans suggested in that session, by [`plan_key`].
    plans: HashSet<u64>,
}

/// The main agent's hook: suggests splitting long plans. Sub-agents (execute_plan items
/// among them) do not have it, so they are never prompted.
pub struct Estimates {
    data_dir: PathBuf,
    root: PathBuf,
    /// `split_suggest_minutes`; 0 turns the suggestion off.
    threshold: u32,
    hints: Mutex<Hints>,
}

impl Estimates {
    pub fn new(data_dir: PathBuf, root: PathBuf, threshold: u32) -> Self {
        Self {
            data_dir,
            root,
            threshold,
            hints: Mutex::default(),
        }
    }

    fn todo(&self, input: &Value) -> Option<String> {
        let items = crate::agents::plan::parse(input).ok()?;
        // Writes on every open item: the plan was split by the skill already (the plan
        // the user confirmed comes back like this).
        if items
            .iter()
            .filter(|item| item.open())
            .all(|item| !item.writes.is_empty())
        {
            return None;
        }
        let key = plan_key(&items);
        let mut hints = self.hints.lock().ok()?;
        if hints.task || hints.plans.contains(&key) {
            return None;
        }
        let hint = suggestion(&items, factor(&self.data_dir), self.threshold, &self.root)?;
        hints.task = true;
        hints.plans.insert(key);
        Some(hint)
    }
}

impl TurnHook for Estimates {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut hints) = self.hints.lock() {
            hints.task = false;
            if hints.session != session.id {
                hints.session = session.id.clone();
                hints.plans.clear();
            }
        }
        Box::pin(async { None })
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        if output.failed || name != "todo" {
            return None;
        }
        self.todo(input)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn record(estimate: u64, actual: f64, completed: bool) -> EstimateRecord {
        EstimateRecord {
            t: String::new(),
            estimate_minutes: estimate,
            actual_minutes: actual,
            items: 2,
            parallel: false,
            completed,
            executed: true,
        }
    }

    #[test]
    fn records_of_planning_only_tasks_from_before_do_not_calibrate() {
        // The format of earlier versions: no `executed`.
        let old: EstimateRecord = serde_json::from_str(
            r#"{"t":"","estimate_minutes":10,"actual_minutes":0.3,"items":2,"parallel":false,"completed":true}"#,
        )
        .unwrap();
        assert!(!old.executed);
        assert_eq!(factor_of(&[old]), (1.0, 0));
    }

    #[test]
    fn an_executed_plan_records_its_estimated_items_as_serial_work() {
        let minutes = |n: u64| Duration::from_secs(n * 60);
        // Two items of 4 and 6 minutes ran side by side for 5 and 9 minutes; one item
        // without an estimate is not counted.
        let record =
            plan_record(&[(4, minutes(5)), (6, minutes(9)), (0, minutes(30))], true).unwrap();
        assert_eq!(
            (record.estimate_minutes, record.actual_minutes, record.items),
            (10, 14.0, 2)
        );
        assert!(record.parallel && record.completed && record.executed);
        assert_eq!(factor_of(&[record]), (1.4, 1));
        assert!(plan_record(&[(0, minutes(3))], false).is_none());
        assert!(plan_record(&[], false).is_none());
        // Large estimates add up without overflowing.
        let huge = plan_record(&[(u32::MAX, minutes(1)), (u32::MAX, minutes(1))], false).unwrap();
        assert_eq!(huge.estimate_minutes, 2 * u64::from(u32::MAX));
    }

    #[test]
    fn the_factor_is_a_bounded_median_of_completed_records() {
        assert_eq!(factor_of(&[]), (1.0, 0));
        assert_eq!(factor_of(&[record(10, 20.0, true)]), (2.0, 1));
        assert_eq!(
            factor_of(&[record(10, 10.0, true), record(10, 30.0, true)]),
            (2.0, 2)
        );
        assert_eq!(factor_of(&[record(10, 100.0, true)]).0, 4.0);
        assert_eq!(factor_of(&[record(10, 1.0, true)]).0, 0.5);
        assert_eq!(factor_of(&[record(10, 50.0, false)]), (1.0, 0));
        // Only the latest 20 count.
        let mut many: Vec<_> = (0..20).map(|_| record(10, 10.0, true)).collect();
        many.insert(0, record(10, 40.0, true));
        assert_eq!(factor_of(&many), (1.0, 20));
    }

    #[test]
    fn records_append_and_read_back() {
        let dir = tempfile::tempdir().unwrap();
        append(dir.path(), &record(6, 9.0, true)).unwrap();
        append(dir.path(), &record(4, 2.0, false)).unwrap();
        let all = records(dir.path());
        assert_eq!(all.len(), 2);
        assert_eq!(all[0].estimate_minutes, 6);
        assert_eq!(factor(dir.path()), (1.5, 1));
    }

    fn items(value: Value) -> Vec<PlanItem> {
        crate::agents::plan::parse(&value).unwrap()
    }

    #[test]
    fn the_suggestion_needs_a_long_estimate_and_parallel_items() {
        let root = tempfile::tempdir().unwrap();
        let root = root.path();
        let parallel = items(json!({"todos":[
            {"content":"a","status":"pending","estimate_minutes":4,"writes":["a.rs"]},
            {"content":"b","status":"pending","estimate_minutes":3,"writes":["b.rs"]},
            {"content":"c","status":"completed","estimate_minutes":30}
        ]}));
        let hint = suggestion(&parallel, (1.0, 0), 5, root).unwrap();
        assert!(hint.contains("about 7 min"), "{hint}");
        assert!(hint.contains("2 of them can run in parallel"), "{hint}");
        assert!(hint.contains("task-split"), "{hint}");
        assert!(
            hint.contains("execute_plan parallel=true isolation=\"overlay\""),
            "{hint}"
        );
        assert!(
            suggestion(&parallel, (1.0, 0), 0, root).is_none(),
            "0 is off"
        );
        assert!(
            suggestion(&parallel, (1.0, 0), 7, root).is_none(),
            "not over"
        );
        assert!(
            suggestion(&parallel, (0.5, 3), 5, root).is_none(),
            "calibrated down"
        );
        assert!(
            suggestion(&parallel, (2.0, 3), 10, root)
                .unwrap()
                .contains("x 2.0")
        );
        let chain = items(json!({"todos":[
            {"content":"a","status":"pending","estimate_minutes":4},
            {"content":"depends:todo-1 b","status":"pending","estimate_minutes":4}
        ]}));
        assert!(
            suggestion(&chain, (1.0, 0), 5, root).is_none(),
            "nothing parallel"
        );
        let clash = items(json!({"todos":[
            {"content":"a","status":"pending","estimate_minutes":4,"writes":["x.rs::f"]},
            {"content":"b","status":"pending","estimate_minutes":4,"writes":["x.rs::g"]}
        ]}));
        assert!(suggestion(&clash, (1.0, 0), 5, root).is_none(), "same file");
        let single = items(json!({"todos":[
            {"content":"a","status":"pending","estimate_minutes":40}
        ]}));
        assert!(suggestion(&single, (1.0, 0), 5, root).is_none());
    }

    #[test]
    fn the_hook_suggests_once_per_plan_and_session_and_records_nothing_itself() {
        let data = tempfile::tempdir().unwrap();
        let root = tempfile::tempdir().unwrap();
        let hook = Estimates::new(data.path().into(), root.path().into(), 5);
        let plan = json!({"todos":[
            {"content":"a","status":"pending","estimate_minutes":6},
            {"content":"b","status":"pending","estimate_minutes":6}
        ]});
        let ok = ToolOutput::default();
        let session = Session::new("w".into(), "p".into(), "m".into(), None);
        let runtime = tokio::runtime::Runtime::new().unwrap();
        runtime.block_on(hook.task_note(&session));
        assert!(hook.tool_note("todo", &plan, &ok).is_some());
        assert!(
            hook.tool_note("todo", &plan, &ok).is_none(),
            "once per task"
        );
        runtime.block_on(hook.task_end(&session, true));
        // Only executed plans are recorded (by execute_plan, not by this hook).
        assert!(records(data.path()).is_empty());
        // The next task (the user's confirmation): the same plan, reordered, is not
        // suggested again, and neither is the split plan with writes on every item.
        runtime.block_on(hook.task_note(&session));
        let reordered = json!({"todos":[
            {"content":"b","status":"pending","estimate_minutes":6},
            {"content":"a","status":"pending","estimate_minutes":6}
        ]});
        assert!(
            hook.tool_note("todo", &reordered, &ok).is_none(),
            "same plan"
        );
        let split = json!({"todos":[
            {"content":"c","status":"pending","estimate_minutes":6,"writes":["c.rs"]},
            {"content":"d","status":"pending","estimate_minutes":6,"writes":["d.rs"]}
        ]});
        assert!(
            hook.tool_note("todo", &split, &ok).is_none(),
            "split already"
        );
        // A different plan in a later task is suggested.
        let other = json!({"todos":[
            {"content":"e","status":"pending","estimate_minutes":6},
            {"content":"f","status":"pending","estimate_minutes":6}
        ]});
        assert!(
            hook.tool_note("todo", &other, &ok).is_some(),
            "another plan"
        );
        // A new session starts over.
        let fresh = Session::new("w".into(), "p".into(), "m".into(), None);
        assert_ne!(fresh.id, session.id);
        runtime.block_on(hook.task_note(&fresh));
        assert!(
            hook.tool_note("todo", &plan, &ok).is_some(),
            "a new session"
        );
    }
}
