//! Cost ledger: one JSON line per billed model call in `<data_dir>/usage/YYYY-MM.jsonl`
//! (UTC months, 13 kept), written by `Metered`, a `Provider` decorator, so every model
//! call (task, compaction, sub-agent, memory extraction, dream) is recorded at the one
//! place where it really happens.

use std::{
    collections::BTreeMap,
    io::{BufRead, Write},
    path::{Path, PathBuf},
    sync::{Arc, Mutex, Once},
};

use futures_util::future::BoxFuture;
use serde::{Deserialize, Serialize};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Event, ModelRequest, ModelResponse, Provider, Result},
    session::now,
};

/// Month files kept; older ones are removed on the first record of a process.
pub const KEEP_MONTHS: i64 = 13;
const LINE_LIMIT: usize = 4096;

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq)]
pub struct UsageEntry {
    /// Unix seconds.
    pub t: u64,
    pub provider: String,
    pub model: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub session: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub source: String,
    #[serde(rename = "in")]
    pub input: u64,
    #[serde(rename = "out")]
    pub output: u64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cache_read: u64,
    #[serde(default, skip_serializing_if = "is_zero")]
    pub cache_write: u64,
    /// `None` when the model's prices are unknown.
    pub cost: Option<f64>,
}

fn is_zero(value: &u64) -> bool {
    *value == 0
}

/// Days since 1970-01-01 to (year, month, day), proleptic Gregorian (UTC).
pub fn civil(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let month = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    let year = yoe + era * 400 + i64::from(month <= 2);
    (year, month, day)
}

/// (year, month, day) to days since 1970-01-01.
pub fn days_from_civil(year: i64, month: u32, day: u32) -> i64 {
    let year = if month <= 2 { year - 1 } else { year };
    let era = year.div_euclid(400);
    let yoe = year.rem_euclid(400);
    let month = i64::from(month);
    let doy = (153 * (if month > 2 { month - 3 } else { month + 9 }) + 2) / 5 + i64::from(day) - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

/// `YYYY-MM-DD HH:MM` in UTC.
pub fn timestamp(secs: u64) -> String {
    let (y, m, d) = civil((secs / 86_400) as i64);
    let rest = secs % 86_400;
    format!(
        "{y:04}-{m:02}-{d:02} {:02}:{:02}",
        rest / 3600,
        rest % 3600 / 60
    )
}

fn month_key(secs: u64) -> (i64, u32) {
    let (y, m, _) = civil((secs / 86_400) as i64);
    (y, m)
}

fn month_start(year: i64, month: u32) -> u64 {
    days_from_civil(year, month, 1).max(0) as u64 * 86_400
}

fn next_month(year: i64, month: u32) -> (i64, u32) {
    if month == 12 {
        (year + 1, 1)
    } else {
        (year, month + 1)
    }
}

fn parse_month_file(name: &str) -> Option<(i64, u32)> {
    let stem = name.strip_suffix(".jsonl")?;
    let (year, month) = stem.split_once('-')?;
    if year.len() != 4 || month.len() != 2 {
        return None;
    }
    let month: u32 = month.parse().ok()?;
    (1..=12)
        .contains(&month)
        .then_some((year.parse().ok()?, month))
}

/// The ledger directory plus the session that calls are attributed to.
pub struct UsageLedger {
    dir: PathBuf,
    session: Mutex<String>,
    pruned: Once,
}

impl UsageLedger {
    pub fn new(data_dir: &Path) -> Self {
        Self {
            dir: data_dir.join("usage"),
            session: Mutex::new(String::new()),
            pruned: Once::new(),
        }
    }

    pub fn dir(&self) -> &Path {
        &self.dir
    }

    pub fn set_session(&self, id: &str) {
        if let Ok(mut current) = self.session.lock()
            && *current != id
        {
            *current = id.to_owned();
        }
    }

    pub fn session(&self) -> String {
        self.session.lock().map(|id| id.clone()).unwrap_or_default()
    }

    /// Appends one entry; a ledger failure never fails the model call itself.
    pub fn record(&self, entry: &UsageEntry) -> Result<()> {
        std::fs::create_dir_all(&self.dir)?;
        self.pruned.call_once(|| prune(&self.dir, entry.t));
        let (year, month) = month_key(entry.t);
        let path = self.dir.join(format!("{year:04}-{month:02}.jsonl"));
        let mut line = serde_json::to_vec(entry)?;
        line.push(b'\n');
        // Read access too: Windows refuses to lock an append-only handle.
        let mut file = std::fs::OpenOptions::new()
            .read(true)
            .create(true)
            .append(true)
            .open(path)?;
        fs2::FileExt::lock_exclusive(&file)?;
        let result = file.write_all(&line);
        let _ = fs2::FileExt::unlock(&file);
        result.map_err(Into::into)
    }

    /// Entries with `from <= t < to`, oldest first; malformed lines are skipped.
    pub fn load(&self, from: u64, to: u64) -> Result<Vec<UsageEntry>> {
        let mut entries = Vec::new();
        let Ok(dir) = std::fs::read_dir(&self.dir) else {
            return Ok(entries);
        };
        let mut files: Vec<((i64, u32), PathBuf)> = dir
            .filter_map(|entry| entry.ok())
            .filter_map(|entry| {
                let name = entry.file_name().to_string_lossy().into_owned();
                parse_month_file(&name).map(|month| (month, entry.path()))
            })
            .collect();
        files.sort();
        for ((year, month), path) in files {
            let (ny, nm) = next_month(year, month);
            if month_start(ny, nm) <= from || month_start(year, month) >= to {
                continue;
            }
            let file = std::fs::File::open(path)?;
            for line in std::io::BufReader::new(file).lines() {
                let line = line?;
                if line.len() > LINE_LIMIT {
                    continue;
                }
                if let Ok(entry) = serde_json::from_str::<UsageEntry>(&line)
                    && entry.t >= from
                    && entry.t < to
                {
                    entries.push(entry);
                }
            }
        }
        entries.sort_by_key(|entry| entry.t);
        Ok(entries)
    }
}

fn prune(dir: &Path, now: u64) {
    let (year, month) = month_key(now);
    let current = year * 12 + i64::from(month) - 1;
    let Ok(entries) = std::fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let name = entry.file_name().to_string_lossy().into_owned();
        if let Some((y, m)) = parse_month_file(&name)
            && current - (y * 12 + i64::from(m) - 1) >= KEEP_MONTHS
        {
            let _ = std::fs::remove_file(entry.path());
        }
    }
}

/// Estimated cost of one call in USD; prices are per million tokens.
pub fn bill(prices: Option<(f64, f64)>, input: u64, output: u64) -> Option<f64> {
    prices.map(|(pin, pout)| (input as f64 * pin + output as f64 * pout) / 1_000_000.0)
}

/// `Provider` decorator that records every reported call in the ledger.
pub struct Metered {
    pub inner: Arc<dyn Provider>,
    pub ledger: Arc<UsageLedger>,
    pub provider: String,
    pub model: String,
    pub prices: Option<(f64, f64)>,
    /// "session", "extraction", "dream", ...
    pub source: &'static str,
}

impl Provider for Metered {
    fn stream(
        &self,
        request: ModelRequest,
        events: mpsc::Sender<Event>,
        cancel: CancellationToken,
    ) -> BoxFuture<'_, Result<ModelResponse>> {
        Box::pin(async move {
            let response = self.inner.stream(request, events, cancel).await?;
            if response.usage_reported {
                let usage = response.usage;
                let _ = self.ledger.record(&UsageEntry {
                    t: now(),
                    provider: self.provider.clone(),
                    model: self.model.clone(),
                    session: self.ledger.session(),
                    source: self.source.into(),
                    input: usage.input,
                    output: usage.output,
                    cache_read: usage.cache_read,
                    cache_write: usage.cache_write,
                    cost: bill(self.prices, usage.input, usage.output),
                });
            }
            Ok(response)
        })
    }
}

#[derive(Debug, PartialEq)]
pub struct Period {
    pub label: String,
    pub from: u64,
    pub to: u64,
}

/// `month` (default), `YYYY-MM`, `30d`, `24h`, `today` or `all`.
pub fn parse_period(arg: Option<&str>, now: u64) -> Result<Period> {
    let (year, month) = month_key(now);
    let period = |label: String, from: u64, to: u64| Period { label, from, to };
    Ok(match arg.unwrap_or("month") {
        "month" => {
            let (ny, nm) = next_month(year, month);
            period(
                format!("{year:04}-{month:02}"),
                month_start(year, month),
                month_start(ny, nm),
            )
        }
        "today" => {
            let start = now / 86_400 * 86_400;
            period("today (UTC)".into(), start, start + 86_400)
        }
        "24h" => period("last 24 hours".into(), now.saturating_sub(86_400), now + 1),
        "30d" => period(
            "last 30 days".into(),
            now.saturating_sub(30 * 86_400),
            now + 1,
        ),
        "all" => period("all kept months".into(), 0, u64::MAX),
        other => {
            let (y, m) = other
                .split_once('-')
                .filter(|(y, m)| y.len() == 4 && m.len() == 2)
                .and_then(|(y, m)| Some((y.parse::<i64>().ok()?, m.parse::<u32>().ok()?)))
                .filter(|(_, m)| (1..=12).contains(m))
                .ok_or_else(|| {
                    Error::Tool("时间段只能是 month、YYYY-MM、30d、24h、today 或 all".into())
                })?;
            let (ny, nm) = next_month(y, m);
            period(
                format!("{y:04}-{m:02}"),
                month_start(y, m),
                month_start(ny, nm),
            )
        }
    })
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct UsageRow {
    pub provider: String,
    pub model: String,
    pub calls: u64,
    pub input: u64,
    pub output: u64,
    pub cache_read: u64,
    pub cache_write: u64,
    pub cost: f64,
    /// Calls whose cost is unknown (no prices for the model).
    pub unpriced: u64,
}

impl UsageRow {
    fn add(&mut self, entry: &UsageEntry) {
        self.calls += 1;
        self.input += entry.input;
        self.output += entry.output;
        self.cache_read += entry.cache_read;
        self.cache_write += entry.cache_write;
        match entry.cost {
            Some(cost) if cost.is_finite() => self.cost += cost,
            _ => self.unpriced += 1,
        }
    }
}

/// Rows by provider/model, most expensive first, and the total.
pub fn aggregate(entries: &[UsageEntry]) -> (Vec<UsageRow>, UsageRow) {
    let mut rows: BTreeMap<(String, String), UsageRow> = BTreeMap::new();
    let mut total = UsageRow {
        provider: "total".into(),
        ..UsageRow::default()
    };
    for entry in entries {
        rows.entry((entry.provider.clone(), entry.model.clone()))
            .or_insert_with(|| UsageRow {
                provider: entry.provider.clone(),
                model: entry.model.clone(),
                ..UsageRow::default()
            })
            .add(entry);
        total.add(entry);
    }
    let mut rows: Vec<_> = rows.into_values().collect();
    rows.sort_by(|a, b| b.cost.total_cmp(&a.cost).then(b.calls.cmp(&a.calls)));
    (rows, total)
}

fn tokens(n: u64) -> String {
    match n {
        0..1_000 => n.to_string(),
        1_000..1_000_000 => format!("{:.1}K", n as f64 / 1e3),
        _ => format!("{:.2}M", n as f64 / 1e6),
    }
}

fn cost(row: &UsageRow) -> String {
    match (row.unpriced, row.calls) {
        (0, _) => format!("${:.4}", row.cost),
        (unpriced, calls) if unpriced == calls => "未知".into(),
        _ => format!(">=${:.4}", row.cost),
    }
}

pub fn render(period: &Period, rows: &[UsageRow], total: &UsageRow) -> String {
    // The label also names export files, so it stays ASCII; only the heading is localized.
    let label = match period.label.as_str() {
        "today (UTC)" => "今天",
        "last 24 hours" => "最近 24 小时",
        "last 30 days" => "最近 30 天",
        "all kept months" => "所有保留的月份",
        other => other,
    };
    let mut out = format!("{label} 的用量（估算，UTC）：\n");
    if rows.is_empty() {
        out.push_str("  没有记录到模型调用\n");
        return out;
    }
    out.push_str(&format!(
        "  {:<34} {:>6} {:>9} {:>9} {:>9} {:>12}\n",
        "provider/model", "calls", "input", "output", "cache_rd", "cost"
    ));
    for row in rows.iter().chain(std::iter::once(total)) {
        let name = if row.model.is_empty() {
            row.provider.clone()
        } else {
            format!("{}/{}", row.provider, row.model)
        };
        let name: String = name.chars().take(34).collect();
        out.push_str(&format!(
            "  {:<34} {:>6} {:>9} {:>9} {:>9} {:>12}\n",
            name,
            row.calls,
            tokens(row.input),
            tokens(row.output),
            tokens(row.cache_read),
            cost(row)
        ));
    }
    if total.unpriced > 0 {
        out.push_str(&format!(
            "  有 {} 次调用的价格未知；设置 input_price/output_price 可得到准确估算。\n",
            total.unpriced
        ));
    }
    out
}

fn csv_field(value: &str) -> String {
    if value.contains([',', '"', '\n', '\r']) {
        format!("\"{}\"", value.replace('"', "\"\""))
    } else {
        value.to_owned()
    }
}

pub fn csv(entries: &[UsageEntry]) -> String {
    let mut out = String::from(
        "time_utc,provider,model,session,source,input,output,cache_read,cache_write,cost_usd\n",
    );
    for entry in entries {
        out.push_str(&format!(
            "{},{},{},{},{},{},{},{},{},{}\n",
            timestamp(entry.t),
            csv_field(&entry.provider),
            csv_field(&entry.model),
            csv_field(&entry.session),
            csv_field(&entry.source),
            entry.input,
            entry.output,
            entry.cache_read,
            entry.cache_write,
            entry
                .cost
                .map_or(String::new(), |cost| format!("{cost:.6}"))
        ));
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(t: u64, model: &str, cost: Option<f64>) -> UsageEntry {
        UsageEntry {
            t,
            provider: "p".into(),
            model: model.into(),
            session: "s".into(),
            source: "session".into(),
            input: 1000,
            output: 10,
            cache_read: 5,
            cache_write: 0,
            cost,
        }
    }

    #[test]
    fn civil_dates_roundtrip() {
        assert_eq!(civil(0), (1970, 1, 1));
        assert_eq!(civil(days_from_civil(2026, 10, 27)), (2026, 10, 27));
        assert_eq!(civil(days_from_civil(2024, 2, 29)), (2024, 2, 29));
        assert_eq!(timestamp(86_400 + 3_660), "1970-01-02 01:01");
    }

    #[test]
    fn periods_parse_and_reject_garbage() {
        let now = days_from_civil(2026, 10, 27) as u64 * 86_400 + 100;
        let month = parse_period(None, now).unwrap();
        assert_eq!(month.label, "2026-10");
        assert_eq!(month.from, days_from_civil(2026, 10, 1) as u64 * 86_400);
        assert_eq!(month.to, days_from_civil(2026, 11, 1) as u64 * 86_400);
        let dec = parse_period(Some("2025-12"), now).unwrap();
        assert_eq!(dec.to, days_from_civil(2026, 1, 1) as u64 * 86_400);
        assert!(parse_period(Some("2025-13"), now).is_err());
        assert!(parse_period(Some("yesterday"), now).is_err());
        assert_eq!(parse_period(Some("24h"), now).unwrap().from, now - 86_400);
    }

    #[test]
    fn ledger_records_loads_aggregates_and_prunes_old_months() {
        let data = tempfile::tempdir().unwrap();
        let ledger = UsageLedger::new(data.path());
        std::fs::create_dir_all(ledger.dir()).unwrap();
        std::fs::write(ledger.dir().join("2020-01.jsonl"), "{}\n").unwrap();
        let now = days_from_civil(2026, 10, 27) as u64 * 86_400;
        ledger.record(&entry(now, "a", Some(0.5))).unwrap();
        ledger.record(&entry(now + 1, "b", None)).unwrap();
        ledger.record(&entry(now + 2, "a", Some(0.25))).unwrap();
        std::fs::OpenOptions::new()
            .append(true)
            .open(ledger.dir().join("2026-10.jsonl"))
            .unwrap()
            .write_all(b"not json\n")
            .unwrap();
        assert!(!ledger.dir().join("2020-01.jsonl").exists());
        let period = parse_period(Some("month"), now).unwrap();
        let entries = ledger.load(period.from, period.to).unwrap();
        assert_eq!(entries.len(), 3);
        let (rows, total) = aggregate(&entries);
        assert_eq!(rows[0].model, "a");
        assert_eq!(rows[0].calls, 2);
        assert!((rows[0].cost - 0.75).abs() < 1e-9);
        assert_eq!(total.unpriced, 1);
        let text = render(&period, &rows, &total);
        assert!(text.contains("p/a") && text.contains(">=$0.7500"), "{text}");
        let csv = csv(&entries);
        assert_eq!(csv.lines().count(), 4);
        assert!(csv.lines().nth(2).unwrap().ends_with(','));
        assert!(ledger.load(0, now).unwrap().is_empty());
    }
}
