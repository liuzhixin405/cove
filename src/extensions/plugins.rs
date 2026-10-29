//! Plugins in `<data_dir>/plugins/<name>/`: `manifest.json` (or Claude's
//! `.claude-plugin/plugin.json`), `skills/` and `commands/*.md` slash commands. Installed by
//! `git clone --depth 1` with links checked out as plain files. Nothing in a plugin executes:
//! its commands are prompt text and its skills are instruction data.

use std::{
    collections::BTreeMap,
    path::{Component, Path, PathBuf},
    time::Duration,
};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    extensions::skills::{clip, read_text},
    extensions::{Registry, identifier},
};

pub const MARKETPLACE_URL: &str = "https://raw.githubusercontent.com/anthropics/claude-plugins-official/main/.claude-plugin/marketplace.json";
pub const MARKETPLACE_REPO: &str = "https://github.com/anthropics/claude-plugins-official.git";
const MAX_PLUGINS: usize = 64;

#[derive(Clone, Debug, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct Manifest {
    pub name: String,
    pub version: String,
    pub description: String,
    #[serde(deserialize_with = "author", skip_serializing_if = "String::is_empty")]
    pub author: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub commands: Vec<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub skills: Vec<String>,
}

/// `manifest.json` writes `"author": "x"`; Claude's plugin.json writes `"author": {"name": "x"}`.
fn author<'de, D: serde::Deserializer<'de>>(d: D) -> std::result::Result<String, D::Error> {
    Ok(match Value::deserialize(d)? {
        Value::String(name) => name,
        Value::Object(object) => object
            .get("name")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_owned(),
        _ => String::new(),
    })
}

#[derive(Clone, Debug)]
pub struct Plugin {
    pub name: String,
    pub dir: PathBuf,
    pub manifest: Manifest,
    pub git: bool,
}

#[derive(Clone, Debug)]
pub struct Command {
    pub plugin: String,
    pub description: String,
    pub prompt: String,
}

/// Reads `manifest.json`, else `.claude-plugin/plugin.json`.
fn read_manifest(dir: &Path) -> Result<Manifest> {
    for file in ["manifest.json", ".claude-plugin/plugin.json"] {
        let path = dir.join(file);
        if std::fs::symlink_metadata(&path).is_ok_and(|m| m.is_file()) {
            let text = read_text(&path, 256 * 1024)?;
            return serde_json::from_str(text.trim_start_matches('\u{feff}'))
                .map_err(|err| Error::Tool(format!("parse {}: {err}", path.display())));
        }
    }
    Err(Error::Tool(
        "no manifest.json or .claude-plugin/plugin.json".into(),
    ))
}

#[derive(Default)]
pub struct Plugins {
    pub root: PathBuf,
    pub list: Vec<Plugin>,
    pub warnings: Vec<String>,
}

impl Plugins {
    pub fn scan(data_dir: &Path) -> Self {
        let root = data_dir.join("plugins");
        let mut plugins = Self {
            root: root.clone(),
            ..Self::default()
        };
        let Ok(entries) = std::fs::read_dir(&root) else {
            return plugins;
        };
        let mut dirs: Vec<_> = entries
            .flatten()
            .filter(|e| e.file_type().is_ok_and(|t| t.is_dir()))
            .map(|e| e.path())
            .filter(|p| {
                p.file_name()
                    .and_then(|n| n.to_str())
                    .is_some_and(|n| !n.starts_with('.') && !n.ends_with(".disabled"))
            })
            .collect();
        dirs.sort();
        for dir in dirs.into_iter().take(MAX_PLUGINS) {
            let dir_name = dir
                .file_name()
                .map(|n| n.to_string_lossy().into_owned())
                .unwrap_or_default();
            match read_manifest(&dir) {
                Ok(mut manifest) => {
                    if manifest.name.trim().is_empty() {
                        manifest.name = dir_name.clone();
                    }
                    if !identifier(&manifest.name) {
                        plugins
                            .warnings
                            .push(format!("插件 {dir_name}：名称无效 {:?}", manifest.name));
                        continue;
                    }
                    if plugins.list.iter().any(|p| p.name == manifest.name) {
                        plugins
                            .warnings
                            .push(format!("插件 {dir_name}：名称重复 {}", manifest.name));
                        continue;
                    }
                    plugins.list.push(Plugin {
                        name: manifest.name.clone(),
                        git: dir.join(".git").exists(),
                        dir,
                        manifest,
                    });
                }
                Err(err) => plugins.warnings.push(format!("插件 {dir_name}：{err}")),
            }
        }
        plugins
    }

    pub fn get(&self, name: &str) -> Option<&Plugin> {
        self.list.iter().find(|p| p.name == name)
    }

    /// Enabled plugins' `commands/*.md`; on a clash the plugin sorting first wins.
    pub fn commands(&self, registry: &Registry) -> BTreeMap<String, Command> {
        let mut out = BTreeMap::new();
        let mut plugins: Vec<_> = self.list.iter().collect();
        plugins.sort_by(|a, b| a.name.cmp(&b.name));
        for plugin in plugins {
            if !registry
                .active("plugin", &plugin.name, None)
                .unwrap_or(false)
            {
                continue;
            }
            let Ok(entries) = std::fs::read_dir(plugin.dir.join("commands")) else {
                continue;
            };
            let mut files: Vec<_> = entries.flatten().map(|e| e.path()).collect();
            files.sort();
            for file in files.into_iter().take(256) {
                let Some(name) = file
                    .extension()
                    .filter(|e| e.eq_ignore_ascii_case("md"))
                    .and_then(|_| file.file_stem())
                    .and_then(|s| s.to_str())
                    .map(str::to_owned)
                else {
                    continue;
                };
                // A link could pull a file from outside the plugin into the prompt.
                if !identifier(&name)
                    || out.contains_key(&name)
                    || !std::fs::symlink_metadata(&file).is_ok_and(|m| m.is_file())
                {
                    continue;
                }
                let Ok(text) = read_text(&file, 64 * 1024) else {
                    continue;
                };
                let (description, prompt) = parse_command(&text);
                out.insert(
                    name,
                    Command {
                        plugin: plugin.name.clone(),
                        description,
                        prompt,
                    },
                );
            }
        }
        out
    }
}

/// Frontmatter `description:` (else the first content line, clipped) and the prompt body.
pub fn parse_command(content: &str) -> (String, String) {
    let parsed = crate::extensions::skills::parse(content).unwrap_or_else(|_| {
        crate::extensions::skills::Parsed {
            name: None,
            description: String::new(),
            paths: Vec::new(),
            allowed_tools: Vec::new(),
            steps: Vec::new(),
            body: content.to_owned(),
            has_frontmatter: false,
        }
    });
    (clip(&parsed.description, 60), parsed.body.trim().to_owned())
}

/// Substitutes `$ARGUMENTS`; without the placeholder, arguments are appended.
pub fn expand(prompt: &str, arguments: &str) -> String {
    let arguments = arguments.trim();
    if prompt.contains("$ARGUMENTS") {
        prompt.replace("$ARGUMENTS", arguments)
    } else if arguments.is_empty() {
        prompt.to_owned()
    } else {
        format!("{prompt}\n\nArguments: {arguments}")
    }
}

/// https, ssh:// or scp-like `git@host:path`; never options, local paths or plain http.
pub fn is_git_url(text: &str) -> bool {
    let text = text.trim();
    if text.starts_with('-') || text.contains(char::is_whitespace) || text.contains("..") {
        return false;
    }
    if let Some(rest) = text
        .strip_prefix("https://")
        .or_else(|| text.strip_prefix("ssh://"))
    {
        return rest.contains('/') && !rest.starts_with('/');
    }
    text.strip_prefix("git@")
        .and_then(|rest| rest.split_once(':'))
        .is_some_and(|(host, path)| !host.is_empty() && !path.is_empty() && !host.contains('/'))
}

pub fn name_from_url(url: &str) -> String {
    let last = url
        .trim_end_matches('/')
        .trim_end_matches(".git")
        .rsplit(['/', ':'])
        .next()
        .unwrap_or_default();
    last.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '_' {
                c
            } else {
                '_'
            }
        })
        .take(64)
        .collect()
}

/// Bounded, non-interactive git: no terminal or credential-manager prompts.
pub async fn git(args: &[&str], cwd: &Path, cancel: &CancellationToken) -> Result<String> {
    let mut command = tokio::process::Command::new("git");
    command
        .args(args)
        .current_dir(cwd)
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GCM_INTERACTIVE", "never")
        .stdin(std::process::Stdio::null())
        .kill_on_drop(true);
    let output = tokio::select! {
        _ = cancel.cancelled() => return Err(Error::Cancelled),
        output = tokio::time::timeout(Duration::from_secs(120), command.output()) => output
            .map_err(|_| Error::Tool("git timed out after 120 s".into()))?
            .map_err(|err| Error::Tool(format!("cannot run git: {err}")))?,
    };
    let text = format!(
        "{}{}",
        String::from_utf8_lossy(&output.stdout),
        String::from_utf8_lossy(&output.stderr)
    );
    if !output.status.success() {
        return Err(Error::Tool(format!(
            "git {} failed: {}",
            args.first().unwrap_or(&""),
            clip(text.trim(), 2000)
        )));
    }
    Ok(text)
}

fn relative(path: &str) -> Option<PathBuf> {
    let path = PathBuf::from(path.trim_start_matches("./"));
    (!path.as_os_str().is_empty() && path.components().all(|c| matches!(c, Component::Normal(_))))
        .then_some(path)
}

/// Clones `url` (optionally keeping only `subdir`) into `<root>/<name>`, staged first so a
/// failed install leaves nothing behind. Claude-format plugins get a generated manifest.json.
pub async fn install_git(
    root: &Path,
    url: &str,
    subdir: Option<&str>,
    name: Option<&str>,
    cancel: &CancellationToken,
) -> Result<Plugin> {
    if !is_git_url(url) {
        return Err(Error::Tool(
            "plugin source must be an https://, ssh:// or git@host:path URL".into(),
        ));
    }
    std::fs::create_dir_all(root)?;
    let staging = root.join(format!(".staging-{}", uuid::Uuid::new_v4().simple()));
    let result = async {
        let target = staging.to_string_lossy().into_owned();
        git(
            &[
                "clone",
                "--depth=1",
                "--quiet",
                "--config",
                "core.symlinks=false",
                "--",
                url,
                &target,
            ],
            root,
            cancel,
        )
        .await?;
        let source = match subdir {
            Some(subdir) => staging.join(
                relative(subdir)
                    .ok_or_else(|| Error::Tool("invalid plugin path in index".into()))?,
            ),
            None => staging.clone(),
        };
        let mut manifest = read_manifest(&source)?;
        if manifest.name.trim().is_empty() {
            manifest.name = name.map_or_else(|| name_from_url(url), str::to_owned);
        }
        if !identifier(&manifest.name) {
            return Err(Error::Tool(format!(
                "invalid plugin name {:?}",
                manifest.name
            )));
        }
        if !source.join("manifest.json").exists() {
            let mut generated = manifest.clone();
            if generated.version.is_empty() {
                generated.version = "0.0.0".into();
            }
            std::fs::write(
                source.join("manifest.json"),
                serde_json::to_vec_pretty(&generated)?,
            )?;
        }
        let dest = root.join(&manifest.name);
        if dest.exists() || root.join(format!("{}.disabled", manifest.name)).exists() {
            return Err(Error::Tool(format!(
                "plugin {} already installed",
                manifest.name
            )));
        }
        std::fs::rename(&source, &dest)?;
        Ok(Plugin {
            name: manifest.name.clone(),
            git: dest.join(".git").exists(),
            dir: dest,
            manifest,
        })
    }
    .await;
    if staging.exists() {
        let _ = std::fs::remove_dir_all(&staging);
    }
    result
}

/// Marketplace entry from Claude's `.claude-plugin/marketplace.json`.
#[derive(Clone, Debug)]
pub struct Entry {
    pub name: String,
    pub description: String,
    pub author: String,
    /// A git URL, or a path inside the marketplace repository.
    pub url: Option<String>,
    pub path: Option<String>,
}

pub fn parse_marketplace(text: &str) -> Result<Vec<Entry>> {
    let value: Value = serde_json::from_str(text)?;
    let plugins = value
        .get("plugins")
        .and_then(Value::as_array)
        .ok_or_else(|| Error::Tool("marketplace index has no plugins".into()))?;
    let mut out = Vec::new();
    for plugin in plugins.iter().take(2000) {
        let Some(name) = plugin.get("name").and_then(Value::as_str) else {
            continue;
        };
        if !identifier(name) {
            continue;
        }
        let (url, path) = match plugin.get("source") {
            Some(Value::String(path)) => (None, relative(path).map(|_| path.clone())),
            Some(Value::Object(source)) => {
                let url = source
                    .get("url")
                    .and_then(Value::as_str)
                    .map(str::to_owned)
                    .or_else(|| {
                        source
                            .get("repo")
                            .and_then(Value::as_str)
                            .map(|repo| format!("https://github.com/{repo}.git"))
                    });
                (url.filter(|u| is_git_url(u)), None)
            }
            _ => (None, None),
        };
        if url.is_none() && path.is_none() {
            continue;
        }
        out.push(Entry {
            name: name.to_owned(),
            description: plugin
                .get("description")
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            author: plugin
                .pointer("/author/name")
                .or_else(|| plugin.get("author"))
                .and_then(Value::as_str)
                .unwrap_or_default()
                .to_owned(),
            url,
            path,
        });
    }
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn urls_names_and_commands() {
        assert!(is_git_url("https://github.com/a/b.git"));
        assert!(is_git_url("git@github.com:a/b.git"));
        assert!(is_git_url("ssh://git@host/a/b"));
        assert!(!is_git_url("http://github.com/a/b"));
        assert!(!is_git_url("--upload-pack=x"));
        assert!(!is_git_url("file:///etc"));
        assert!(!is_git_url("/local/path"));
        assert_eq!(
            name_from_url("https://github.com/a/my.plugin.git"),
            "my_plugin"
        );
        let (description, prompt) =
            parse_command("\u{feff}---\ndescription: \"Review it\"\n---\nReview $ARGUMENTS now\n");
        assert_eq!(description, "Review it");
        assert_eq!(expand(&prompt, " src/a.rs "), "Review src/a.rs now");
        assert_eq!(expand("Plain", "x"), "Plain\n\nArguments: x");
        assert_eq!(parse_command("# Title\nbody").0, "Title");
    }

    #[test]
    fn scans_manifests_and_commands_first_plugin_wins() {
        let data = tempfile::tempdir().unwrap();
        let root = data.path().join("plugins");
        for (dir, manifest) in [
            ("b", r#"{"name":"b","version":"1"}"#),
            ("a", r#"{"version":"1"}"#),
            ("bad", r#"{"name":"../x"}"#),
        ] {
            std::fs::create_dir_all(root.join(dir).join("commands")).unwrap();
            std::fs::write(root.join(dir).join("manifest.json"), manifest).unwrap();
            std::fs::write(
                root.join(dir).join("commands/review.md"),
                format!("from {dir}"),
            )
            .unwrap();
        }
        std::fs::create_dir_all(root.join("c/.claude-plugin")).unwrap();
        std::fs::write(
            root.join("c/.claude-plugin/plugin.json"),
            r#"{"name":"c","author":{"name":"Ann"}}"#,
        )
        .unwrap();
        let plugins = Plugins::scan(data.path());
        assert_eq!(plugins.list.len(), 3);
        assert_eq!(plugins.get("c").unwrap().manifest.author, "Ann");
        assert_eq!(plugins.warnings.len(), 1);
        let registry = Registry::open(data.path()).unwrap();
        let commands = plugins.commands(&registry);
        assert_eq!(commands["review"].plugin, "a");
        registry
            .change(|s| {
                s.disabled.insert("plugin:a".into());
                Ok(())
            })
            .unwrap();
        assert_eq!(plugins.commands(&registry)["review"].plugin, "b");
    }

    #[test]
    fn marketplace_index_entries() {
        let entries = parse_marketplace(
            r#"{"plugins":[{"name":"a","source":"./plugins/a","description":"A","author":{"name":"x"}},
            {"name":"b","source":{"source":"github","repo":"o/b"}},{"name":"c","source":"../escape"},
            {"name":"../d","source":"./d"}]}"#,
        )
        .unwrap();
        assert_eq!(entries.len(), 2);
        assert_eq!(entries[0].path.as_deref(), Some("./plugins/a"));
        assert_eq!(
            entries[1].url.as_deref(),
            Some("https://github.com/o/b.git")
        );
    }

    #[tokio::test]
    async fn installs_from_a_local_git_repository_without_links() {
        let cancel = CancellationToken::new();
        let source = tempfile::tempdir().unwrap();
        if git(&["init", "--quiet"], source.path(), &cancel)
            .await
            .is_err()
        {
            return; // git not available
        }
        std::fs::write(
            source.path().join("manifest.json"),
            r#"{"name":"demo","version":"1.0"}"#,
        )
        .unwrap();
        let _ = git(&["add", "."], source.path(), &cancel).await;
        if git(
            &[
                "-c",
                "user.name=t",
                "-c",
                "user.email=t@t",
                "commit",
                "--quiet",
                "-m",
                "x",
            ],
            source.path(),
            &cancel,
        )
        .await
        .is_err()
        {
            return;
        }
        let data = tempfile::tempdir().unwrap();
        // Local paths are refused as sources.
        assert!(
            install_git(
                &data.path().join("plugins"),
                &source.path().display().to_string(),
                None,
                None,
                &cancel
            )
            .await
            .is_err()
        );
        assert!(
            std::fs::read_dir(data.path().join("plugins")).map_or(true, |mut d| d.next().is_none())
        );
    }
}
