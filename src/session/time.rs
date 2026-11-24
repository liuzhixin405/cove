//! How times are shown to people (history, `/undo`, memory, dream): always in local
//! time. Only the usage CSV export (`usage::csv`, column `time_utc`) stays in UTC.

use chrono::{DateTime, Datelike, Local, TimeZone};

fn local(secs: u64) -> DateTime<Local> {
    Local
        .timestamp_opt(i64::try_from(secs).unwrap_or(i64::MAX), 0)
        .single()
        .unwrap_or_else(Local::now)
}

fn days_ago(secs: u64) -> i64 {
    (Local::now().date_naive() - local(secs).date_naive()).num_days()
}

/// `2026-11-18 13:47` in local time.
pub fn stamp(secs: u64) -> String {
    local(secs).format("%Y-%m-%d %H:%M").to_string()
}

/// Short and relative for lists: `刚刚`, `7 分钟前`, `13:47`, `昨天 20:46`,
/// `周一 18:06`, `11-05 09:30`, `2025-11-05`.
pub fn relative(secs: u64) -> String {
    let age = Local::now().timestamp() - i64::try_from(secs).unwrap_or(i64::MAX);
    let time = local(secs);
    match (age, days_ago(secs)) {
        (..60, _) => "刚刚".into(),
        (60..3600, _) => format!("{} 分钟前", age / 60),
        (_, ..=0) => time.format("%H:%M").to_string(),
        (_, 1) => format!("昨天 {}", time.format("%H:%M")),
        (_, 2..=6) => {
            const WEEK: [&str; 7] = ["周一", "周二", "周三", "周四", "周五", "周六", "周日"];
            format!(
                "{} {}",
                WEEK[time.weekday().num_days_from_monday() as usize],
                time.format("%H:%M")
            )
        }
        _ if time.year() == Local::now().year() => time.format("%m-%d %H:%M").to_string(),
        _ => time.format("%Y-%m-%d").to_string(),
    }
}

/// How long ago, in words: `刚刚`, `5 分钟前`, `3 小时前`, `2 天前`.
pub fn ago(secs: u64) -> String {
    let age = super::now().saturating_sub(secs);
    match age {
        0..60 => "刚刚".into(),
        60..3600 => format!("{} 分钟前", age / 60),
        3600..86400 => format!("{} 小时前", age / 3600),
        _ => format!("{} 天前", age / 86400),
    }
}

/// The group a list puts a session under.
pub fn group(secs: u64) -> &'static str {
    match days_ago(secs) {
        ..=0 => "今天",
        1 => "昨天",
        2..=6 => "最近 7 天",
        _ => "更早",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn recent_times_are_relative_and_grouped() {
        let now = super::super::now();
        assert_eq!(relative(now), "刚刚");
        assert_eq!(relative(now - 300), "5 分钟前");
        assert_eq!(group(now), "今天");
        assert_eq!(group(now - 30 * 86400), "更早");
        assert_eq!(ago(now - 7200), "2 小时前");
        assert_eq!(stamp(now).len(), "2026-11-18 13:47".len());
    }

    #[test]
    fn stamp_is_local_time() {
        for secs in [0, 1_700_000_000, 1_795_000_000, super::super::now()] {
            let expected = Local
                .timestamp_opt(secs as i64, 0)
                .unwrap()
                .format("%Y-%m-%d %H:%M")
                .to_string();
            assert_eq!(stamp(secs), expected);
        }
    }
}
