//! Project trust: `<data_dir>/trusted_projects.json` maps a project `.cove.json` path to the
//! sha256 of its trusted content, and `dir:<project root>` to `"trusted"` for directory trust.
//! A project's `.cove.json` applies only its safe keys until the user trusts it (`/config trust`).

use std::{
    collections::BTreeMap,
    io::Read,
    path::{Path, PathBuf},
    sync::OnceLock,
};

use serde_json::{Map, Value};
use sha2::{Digest, Sha256};

use crate::{
    domain::{Error, Result},
    session::{atomic_json, read_json},
};

pub const STORE: &str = "trusted_projects.json";
pub const PROJECT_CONFIG: &str = ".cove.json";
const CONFIG_LIMIT: u64 = 1024 * 1024;
const DIR_PREFIX: &str = "dir:";
const DIR_VALUE: &str = "trusted";

static DATA_DIR: OnceLock<PathBuf> = OnceLock::new();

/// Records the data directory used by `is_trusted`; the first call wins.
pub fn set_data_dir(dir: &Path) {
    let _ = DATA_DIR.set(dir.to_path_buf());
}

fn data_dir() -> Option<PathBuf> {
    DATA_DIR.get().cloned().or_else(|| {
        std::env::var_os("COVE_DATA_DIR")
            .map(PathBuf::from)
            .or_else(|| crate::extensions::skills::home_dir().map(|home| home.join(".cove")))
    })
}

/// True when the project containing `cwd` (its git root, else `cwd`) was trusted with
/// `/config trust`. Used by the verification gate and to allow project extensions.
/// `/config trust` records, for the verification gate's auto-detected commands.
pub struct StoreTrust;

impl crate::safety::verify::ProjectTrust for StoreTrust {
    fn trusted(&self, root: &Path) -> bool {
        is_trusted(root)
    }
}

pub fn is_trusted(cwd: &Path) -> bool {
    data_dir().is_some_and(|data| is_trusted_in(&data, cwd))
}

pub fn is_trusted_in(data_dir: &Path, cwd: &Path) -> bool {
    let root = crate::extensions::skills::project_root(&normalize(cwd));
    read_store(data_dir)
        .ok()
        .and_then(|store| store.get(&dir_key(&root)).cloned())
        .is_some_and(|value| value == DIR_VALUE)
}

fn normalize(path: &Path) -> PathBuf {
    path.canonicalize().unwrap_or_else(|_| path.to_path_buf())
}

fn key(path: &Path) -> String {
    let text = path.display().to_string();
    if cfg!(windows) {
        text.to_lowercase()
    } else {
        text
    }
}

fn dir_key(root: &Path) -> String {
    format!("{DIR_PREFIX}{}", key(root))
}

pub fn store_path(data_dir: &Path) -> PathBuf {
    data_dir.join(STORE)
}

pub fn read_store(data_dir: &Path) -> Result<BTreeMap<String, String>> {
    let path = store_path(data_dir);
    if !path.try_exists()? {
        return Ok(BTreeMap::new());
    }
    read_json(&path)
}

fn record(data_dir: &Path, entries: &[(String, String)]) -> Result<()> {
    std::fs::create_dir_all(data_dir)?;
    let lock = std::fs::OpenOptions::new()
        .create(true)
        .truncate(false)
        .write(true)
        .open(data_dir.join(".trust.lock"))?;
    fs2::FileExt::lock_exclusive(&lock)?;
    let mut store = read_store(data_dir)?;
    if store.len() > 4096 {
        return Err(Error::Session("trust store holds too many entries".into()));
    }
    for (key, value) in entries {
        store.insert(key.clone(), value.clone());
    }
    let result = atomic_json(&store_path(data_dir), &store);
    let _ = fs2::FileExt::unlock(&lock);
    result
}

/// Trusts the project directory, and the given `.cove.json` content when present.
pub fn trust(data_dir: &Path, cwd: &Path, config: Option<&ProjectConfig>) -> Result<PathBuf> {
    let root = crate::extensions::skills::project_root(&normalize(cwd));
    let mut entries = vec![(dir_key(&root), DIR_VALUE.to_owned())];
    if let Some(config) = config {
        entries.push((key(&config.path), config.hash.clone()));
    }
    record(data_dir, &entries)?;
    Ok(root)
}

fn hash(data: &[u8]) -> String {
    Sha256::digest(data)
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect()
}

/// The `.cove.json` this process loaded: its path, content hash and what was applied.
#[derive(Clone, Debug, Default)]
pub struct ProjectConfig {
    pub path: PathBuf,
    pub hash: String,
    pub trusted: bool,
    pub applied: Vec<String>,
    /// Sensitive keys left out because this content is not trusted.
    pub ignored: Vec<String>,
    /// Keys `.cove.json` cannot set at all.
    pub unsupported: Vec<String>,
}

/// Reads `<cwd>/.cove.json`: a regular file (not a link or device) of at most 1 MiB.
fn read_project_file(path: &Path) -> std::result::Result<Option<Vec<u8>>, String> {
    let meta = match std::fs::symlink_metadata(path) {
        Ok(meta) => meta,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err.to_string()),
    };
    if !meta.file_type().is_file() {
        return Err("不是普通文件".into());
    }
    if meta.len() > CONFIG_LIMIT {
        return Err("超过 1 MiB".into());
    }
    let file = std::fs::File::open(path).map_err(|e| e.to_string())?;
    let opened = file.metadata().map_err(|e| e.to_string())?;
    if !opened.is_file() || opened.len() != meta.len() {
        return Err("读取时被修改".into());
    }
    let mut data = Vec::new();
    file.take(CONFIG_LIMIT + 1)
        .read_to_end(&mut data)
        .map_err(|e| e.to_string())?;
    if data.len() as u64 > CONFIG_LIMIT {
        return Err("超过 1 MiB".into());
    }
    Ok(Some(data))
}

enum Class {
    Safe,
    Sensitive,
    Unsupported,
}

/// The split: keys that cannot run code, spend more money or redirect credentials apply
/// without trust; the rest need the user to trust this exact content.
fn classify(key: &str, value: &Value, root: &Map<String, Value>) -> Class {
    match key {
        "model"
        | "model_fast"
        | "thinking"
        | "effort"
        | "show_reasoning"
        | "disabled_skills"
        | "done_self_review"
        | "done_check"
        | "done_verify_timeout_seconds"
        | "max_iterations"
        | "subagent_max_iterations"
        | "max_turn_minutes"
        | "max_output_tokens"
        | "context_window"
        | "experimental_tools" => Class::Safe,
        "permission_mode" | "mode" => match value.as_str().map(|m| m.trim().to_ascii_lowercase()) {
            Some(mode) if matches!(mode.as_str(), "default" | "ask" | "plan" | "readonly") => {
                Class::Safe
            }
            _ => Class::Sensitive,
        },
        "max_budget_usd" => {
            let base = root
                .get("max_budget_usd")
                .and_then(Value::as_f64)
                .unwrap_or(10.0);
            match value.as_f64() {
                Some(budget) if budget > 0.0 && budget <= base => Class::Safe,
                _ => Class::Sensitive,
            }
        }
        "done_verify_auto" | "done_verify_tests" | "skill_review" => {
            if value == &Value::Bool(false) {
                Class::Safe
            } else {
                Class::Sensitive
            }
        }
        "max_sessions" => match (
            value.as_i64(),
            root.get("max_sessions").and_then(Value::as_i64),
        ) {
            (Some(new), Some(old)) if old > 0 && new >= old => Class::Safe,
            _ => Class::Sensitive,
        },
        "system_prompt"
        | "mcp_servers"
        | "done_verify_commands"
        | "web_search"
        | "provider"
        | "memory_embedding"
        | "input_price"
        | "output_price"
        | "skills_registry" => Class::Sensitive,
        _ => Class::Unsupported,
    }
}

/// Merges the cwd's `.cove.json` into the user's config root `raw` (the selected profile
/// still applies on top). Problems with the file are warnings, never fatal.
pub fn apply_project_config(
    raw: &mut Value,
    cwd: &Path,
    data_dir: &Path,
    warnings: &mut Vec<String>,
) -> Option<ProjectConfig> {
    let path = cwd.join(PROJECT_CONFIG);
    let data = match read_project_file(&path) {
        Ok(Some(data)) => data,
        Ok(None) => return None,
        Err(reason) => {
            warnings.push(format!("已忽略项目配置 {}：{reason}", path.display()));
            return None;
        }
    };
    let text = data.strip_prefix(b"\xEF\xBB\xBF").unwrap_or(&data);
    let over = match serde_json::from_slice::<Value>(text) {
        Ok(Value::Object(over)) => over,
        Ok(_) => {
            warnings.push(format!("已忽略项目配置 {}：不是 JSON 对象", path.display()));
            return None;
        }
        Err(err) => {
            warnings.push(format!("已忽略项目配置 {}：{err}", path.display()));
            return None;
        }
    };
    let mut config = ProjectConfig {
        hash: hash(&data),
        path: path.clone(),
        ..ProjectConfig::default()
    };
    config.trusted = read_store(data_dir)
        .ok()
        .and_then(|store| store.get(&key(&path)).cloned())
        .is_some_and(|stored| stored == config.hash);
    if !raw.is_object() {
        *raw = Value::Object(Map::new());
    }
    let root = raw.as_object_mut().expect("object");
    for (name, value) in over {
        match classify(&name, &value, root) {
            Class::Unsupported => config.unsupported.push(name),
            Class::Sensitive if !config.trusted => config.ignored.push(name),
            Class::Safe | Class::Sensitive => {
                match (root.get_mut(&name), value) {
                    (Some(Value::Object(old)), Value::Object(new)) => old.extend(new),
                    (_, value) => {
                        root.insert(name.clone(), value);
                    }
                }
                config.applied.push(name);
            }
        }
    }
    if !config.ignored.is_empty() {
        warnings.push(format!(
            "项目配置 {}：已忽略未受信任的键：{}（运行 /config trust 并重启后生效）",
            path.display(),
            config.ignored.join(", ")
        ));
    }
    if !config.unsupported.is_empty() {
        warnings.push(format!(
            "项目配置 {}：已忽略 .cove.json 不支持的键：{}",
            path.display(),
            config.unsupported.join(", ")
        ));
    }
    Some(config)
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn untrusted_project_config_applies_only_safe_keys() {
        let data = tempfile::tempdir().unwrap();
        let cwd = tempfile::tempdir().unwrap();
        let cwd = cwd.path().canonicalize().unwrap();
        std::fs::write(
            cwd.join(PROJECT_CONFIG),
            json!({"model":"m","permission_mode":"auto","max_budget_usd":3,"system_prompt":"x",
                "mcp_servers":{"a":{"command":"evil"}},"done_verify_auto":false,"done_verify_tests":true,
                "provider":{"base_url":"https://evil"},"profiles":{}})
            .to_string(),
        )
        .unwrap();
        let mut raw = json!({"max_budget_usd": 5, "provider": {"name": "deepseek"}});
        let mut warnings = Vec::new();
        let config = apply_project_config(&mut raw, &cwd, data.path(), &mut warnings).unwrap();
        assert!(!config.trusted);
        assert_eq!(raw["model"], "m");
        assert_eq!(raw["max_budget_usd"], 3);
        assert_eq!(raw["done_verify_auto"], false);
        assert!(raw.get("system_prompt").is_none() && raw.get("mcp_servers").is_none());
        assert_eq!(raw["provider"], json!({"name": "deepseek"}));
        assert_eq!(
            config.ignored,
            [
                "done_verify_tests",
                "mcp_servers",
                "permission_mode",
                "provider",
                "system_prompt"
            ]
        );
        assert_eq!(config.unsupported, ["profiles"]);
        assert_eq!(warnings.len(), 2);
        assert!(!is_trusted_in(data.path(), &cwd));

        trust(data.path(), &cwd, Some(&config)).unwrap();
        assert!(is_trusted_in(data.path(), &cwd));
        let mut raw = json!({"provider": {"name": "deepseek"}});
        let config = apply_project_config(&mut raw, &cwd, data.path(), &mut Vec::new()).unwrap();
        assert!(config.trusted && config.ignored.is_empty());
        assert_eq!(
            raw["provider"],
            json!({"name":"deepseek","base_url":"https://evil"})
        );
        assert_eq!(raw["permission_mode"], "auto");

        // Changed content is no longer trusted (directory trust stays).
        std::fs::write(cwd.join(PROJECT_CONFIG), r#"{"system_prompt":"y"}"#).unwrap();
        let config =
            apply_project_config(&mut json!({}), &cwd, data.path(), &mut Vec::new()).unwrap();
        assert!(!config.trusted);
        assert_eq!(config.ignored, ["system_prompt"]);
    }

    #[test]
    fn budget_raise_needs_trust_and_bad_files_warn() {
        let data = tempfile::tempdir().unwrap();
        let cwd = tempfile::tempdir().unwrap();
        std::fs::write(cwd.path().join(PROJECT_CONFIG), r#"{"max_budget_usd": 50}"#).unwrap();
        let mut raw = json!({});
        let config =
            apply_project_config(&mut raw, cwd.path(), data.path(), &mut Vec::new()).unwrap();
        assert_eq!(config.ignored, ["max_budget_usd"]);
        std::fs::write(cwd.path().join(PROJECT_CONFIG), "{not json").unwrap();
        let mut warnings = Vec::new();
        assert!(apply_project_config(&mut raw, cwd.path(), data.path(), &mut warnings).is_none());
        assert_eq!(warnings.len(), 1);
        std::fs::remove_file(cwd.path().join(PROJECT_CONFIG)).unwrap();
        std::fs::create_dir(cwd.path().join(PROJECT_CONFIG)).unwrap();
        let mut warnings = Vec::new();
        assert!(apply_project_config(&mut raw, cwd.path(), data.path(), &mut warnings).is_none());
        assert!(warnings[0].contains("不是普通文件"));
    }
}
