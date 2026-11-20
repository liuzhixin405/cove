//! Text that looks like it carries a credential, kept out of places that persist
//! or share text (the input history, skill files).

use std::sync::LazyLock;

use regex::Regex;

static SECRET: LazyLock<Regex> = LazyLock::new(|| {
    Regex::new(r"(?i)(api[_-]?key|secret|passw(or)?d|token|bearer)\s*[:=]\s*\S{8,}|\bsk-[A-Za-z0-9_-]{16,}|\bAKIA[0-9A-Z]{16}\b|-----BEGIN [A-Z ]*PRIVATE KEY-----|\bgh[pousr]_[A-Za-z0-9]{30,}|\bxox[abp]-[A-Za-z0-9-]{10,}")
        .unwrap()
});

/// True when text looks like it carries a credential.
pub fn contains_secret(text: &str) -> bool {
    SECRET.is_match(text)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn credentials_are_recognized_and_plain_text_is_not() {
        assert!(contains_secret("api_key = abcdefgh12345"));
        assert!(contains_secret("token: sk-abcdefghijklmnop1234"));
        assert!(contains_secret("-----BEGIN RSA PRIVATE KEY-----"));
        assert!(!contains_secret("修复登录问题"));
        assert!(!contains_secret("token 太长了"));
    }
}
