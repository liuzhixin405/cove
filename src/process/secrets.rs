//! Environment variables that must never reach the programs the model runs: the API key
//! cove itself uses. `COVE_API_KEY` always; at startup the variable the key actually came
//! from (`DEEPSEEK_API_KEY` and the like) is added. Other providers' variables are left
//! alone: the user's own scripts and tests may need them.

use std::sync::OnceLock;

const ALWAYS: &str = "COVE_API_KEY";

static REGISTERED: OnceLock<Vec<String>> = OnceLock::new();

/// Registers where the key in use came from (`Settings::key_source`): an environment
/// variable name, or `config` (a key in the config file is no variable to remove).
/// Only the first registration counts.
pub fn protect_key_source(source: &str) {
    let names = registered_from(source);
    let _ = REGISTERED.set(names);
}

fn registered_from(source: &str) -> Vec<String> {
    let source = source.trim();
    let is_variable = !source.is_empty()
        && source != "config"
        && source != ALWAYS
        && source
            .chars()
            .all(|c| c.is_ascii_uppercase() || c.is_ascii_digit() || c == '_');
    if is_variable {
        vec![source.to_owned()]
    } else {
        Vec::new()
    }
}

/// The variables to remove from a child's environment.
pub fn protected() -> Vec<String> {
    let mut names = vec![ALWAYS.to_owned()];
    if let Some(registered) = REGISTERED.get() {
        names.extend(registered.iter().cloned());
    }
    names
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_key_in_use_is_protected_and_a_config_key_adds_nothing() {
        assert_eq!(registered_from("DEEPSEEK_API_KEY"), ["DEEPSEEK_API_KEY"]);
        assert!(registered_from("config").is_empty());
        assert!(registered_from("COVE_API_KEY").is_empty());
        assert!(registered_from("").is_empty());
        assert!(registered_from("not a variable").is_empty());
        assert!(protected().contains(&"COVE_API_KEY".to_owned()));
    }
}
