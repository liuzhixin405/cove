//! Which build of cove this is (0033). `BUILD` is `<version>+<source hash>`; the
//! hash comes from `build.rs` and changes with any file that decides runtime
//! behaviour, so records written by different builds can be told apart even
//! though the version number stays the same. Traces, acceptance reports and
//! sessions store `BUILD`; `--version`, `/status` and the support report show
//! `label()`.

use std::sync::OnceLock;

/// `<version>+<12 hex digits>`, e.g. `0.7.0+3fa9c1d2e4b7`.
pub const BUILD: &str = concat!(env!("CARGO_PKG_VERSION"), "+", env!("COVE_SOURCE"));

/// Build time in Unix seconds (UTC), set by `build.rs`.
const BUILT: &str = env!("COVE_BUILT");

/// `BUILD` with the build time in local time:
/// `0.7.0+3fa9c1d2e4b7（构建于 2026-11-24 10:00 本地时间）`.
pub fn label() -> &'static str {
    static LABEL: OnceLock<String> = OnceLock::new();
    LABEL.get_or_init(|| label_at(BUILT))
}

fn label_at(built: &str) -> String {
    match built
        .parse::<i64>()
        .ok()
        .and_then(|secs| chrono::DateTime::from_timestamp(secs, 0))
    {
        Some(time) => format!(
            "{BUILD}（构建于 {} 本地时间）",
            time.with_timezone(&chrono::Local).format("%Y-%m-%d %H:%M")
        ),
        None => BUILD.to_owned(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn build_is_version_plus_twelve_hex_digits() {
        let (version, hash) = BUILD.split_once('+').unwrap();
        assert_eq!(version, env!("CARGO_PKG_VERSION"));
        assert_eq!(hash.len(), 12);
        assert!(
            hash.bytes()
                .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
        );
    }

    #[test]
    fn label_shows_local_build_time() {
        let label = label();
        assert!(label.starts_with(&format!("{BUILD}（构建于 ")), "{label}");
        assert!(label.ends_with(" 本地时间）"), "{label}");
        let expected = chrono::DateTime::from_timestamp(0, 0)
            .unwrap()
            .with_timezone(&chrono::Local)
            .format("%Y-%m-%d %H:%M")
            .to_string();
        assert!(label_at("0").contains(&expected));
        assert_eq!(label_at("not a number"), BUILD);
    }
}
