//! Skills: layered discovery (built-in < plugin < user < configured < project), frontmatter
//! (`paths`, `allowed_tools`, `steps`), rendering for `/<skill>` and the `skill` tool, and
//! file-type hints. Skill text is instruction *data*: it never grants permissions.

use std::{
    collections::{BTreeMap, BTreeSet, HashSet},
    io::Read,
    path::{Component, Path, PathBuf},
    sync::{Arc, Mutex, RwLock},
    time::{Duration, Instant, SystemTime},
};

use futures_util::future::BoxFuture;
use serde::Deserialize;
use serde_json::{Value, json};

use crate::{
    domain::{Error, Result},
    engine::turn::TurnHook,
    extensions::{Registry, Sourced, identifier},
    session::Session,
    tools::ToolOutput,
};

/// Built-in skills compiled into the binary; never copied to disk, so they update with it.
const BUILTIN: &[(&str, &str)] = &[
    (
        "commit-messages",
        include_str!("../../assets/skills/commit-messages/SKILL.md"),
    ),
    (
        "executing-plans",
        include_str!("../../assets/skills/executing-plans/SKILL.md"),
    ),
    (
        "github-code-review",
        include_str!("../../assets/skills/github-code-review/SKILL.md"),
    ),
    (
        "github-pr-workflow",
        include_str!("../../assets/skills/github-pr-workflow/SKILL.md"),
    ),
    (
        "karpathy-guidelines",
        include_str!("../../assets/skills/karpathy-guidelines/SKILL.md"),
    ),
    (
        "performance-optimization",
        include_str!("../../assets/skills/performance-optimization/SKILL.md"),
    ),
    ("plan", include_str!("../../assets/skills/plan/SKILL.md")),
    (
        "requesting-code-review",
        include_str!("../../assets/skills/requesting-code-review/SKILL.md"),
    ),
    (
        "safe-refactoring",
        include_str!("../../assets/skills/safe-refactoring/SKILL.md"),
    ),
    ("spike", include_str!("../../assets/skills/spike/SKILL.md")),
    (
        "systematic-debugging",
        include_str!("../../assets/skills/systematic-debugging/SKILL.md"),
    ),
    (
        "test-driven-development",
        include_str!("../../assets/skills/test-driven-development/SKILL.md"),
    ),
];

/// The built-in skill marketplace: `marketplace/` of this repository, embedded so
/// it works offline. `skills_registry` in the config selects another one.
pub const MARKETPLACE_REGISTRY: &str = include_str!("../../marketplace/registry.json");
/// The marketplace's skill files by their path relative to `registry.json`.
pub const MARKETPLACE_SKILLS: &[(&str, &str)] = &[
    (
        "skills/security-audit/SKILL.md",
        include_str!("../../marketplace/skills/security-audit/SKILL.md"),
    ),
    (
        "skills/api-design/SKILL.md",
        include_str!("../../marketplace/skills/api-design/SKILL.md"),
    ),
    (
        "skills/dockerize/SKILL.md",
        include_str!("../../marketplace/skills/dockerize/SKILL.md"),
    ),
    (
        "skills/i18n/SKILL.md",
        include_str!("../../marketplace/skills/i18n/SKILL.md"),
    ),
    (
        "skills/ci-cd/SKILL.md",
        include_str!("../../marketplace/skills/ci-cd/SKILL.md"),
    ),
];

const FILE_LIMIT: usize = 64 * 1024;
const MAX_SKILLS: usize = 256;
pub const DESCRIPTION_LIMIT: usize = 120;
pub const AUTO_PREFIX: &str = "auto-";

pub(crate) fn read_text(path: &Path, limit: usize) -> Result<String> {
    let mut bytes = Vec::new();
    std::fs::File::open(path)?
        .take(limit as u64 + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(Error::Tool(format!(
            "{} exceeds {limit} bytes",
            path.display()
        )));
    }
    String::from_utf8(bytes).map_err(|_| Error::Tool("extension data must be UTF-8".into()))
}

/// At most `limit` characters, with an ellipsis when clipped.
pub fn clip(text: &str, limit: usize) -> String {
    if text.chars().count() <= limit {
        return text.to_owned();
    }
    let mut out: String = text.chars().take(limit.saturating_sub(1)).collect();
    out.push('…');
    out
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Origin {
    Builtin,
    Plugin,
    User,
    Configured,
    Project,
    Auto,
}

impl Origin {
    pub fn label(&self) -> &'static str {
        match self {
            Self::Builtin => "builtin",
            Self::Plugin => "plugin",
            Self::User => "user",
            Self::Configured => "configured",
            Self::Project => "project",
            Self::Auto => "auto",
        }
    }
}

#[derive(Clone)]
enum Body {
    Embedded(&'static str),
    File(PathBuf),
}

#[derive(Clone)]
struct Skill {
    name: String,
    description: String,
    body: Body,
    plugin: Option<String>,
    origin: Origin,
    paths: Vec<String>,
    allowed_tools: Vec<String>,
    steps: Vec<String>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum List {
    One(String),
    Many(Vec<String>),
}

impl List {
    fn items(self) -> Vec<String> {
        let raw = match self {
            Self::One(text) => text
                .trim()
                .trim_start_matches('[')
                .trim_end_matches(']')
                .split(',')
                .map(str::to_owned)
                .collect(),
            Self::Many(items) => items,
        };
        raw.into_iter()
            .map(|item| item.trim().trim_matches(['"', '\'']).trim().to_owned())
            .filter(|item| !item.is_empty())
            .collect()
    }
}

#[derive(Deserialize, Default)]
struct Front {
    name: Option<String>,
    description: Option<String>,
    paths: Option<List>,
    #[serde(alias = "allowed-tools")]
    allowed_tools: Option<List>,
    steps: Option<List>,
}

/// Parsed skill file: metadata plus the instruction body (LF line endings).
pub struct Parsed {
    pub name: Option<String>,
    pub description: String,
    pub paths: Vec<String>,
    pub allowed_tools: Vec<String>,
    pub steps: Vec<String>,
    pub body: String,
    pub has_frontmatter: bool,
}

/// Splits optional YAML frontmatter (BOM and CRLF tolerant) from the body.
pub fn parse(content: &str) -> Result<Parsed> {
    let content = content
        .strip_prefix('\u{feff}')
        .unwrap_or(content)
        .replace("\r\n", "\n");
    let (front, body) = match content.strip_prefix("---\n") {
        Some(rest) if rest.starts_with("---\n") || rest == "---" => (
            Some(String::new()),
            rest.trim_start_matches("---")
                .trim_start_matches('\n')
                .to_owned(),
        ),
        Some(rest) => match rest.split_once("\n---\n") {
            Some((front, body)) => (Some(front.to_owned()), body.to_owned()),
            None => match rest.strip_suffix("\n---") {
                Some(front) => (Some(front.to_owned()), String::new()),
                None => (None, content.clone()),
            },
        },
        None => (None, content.clone()),
    };
    let has_frontmatter = front.is_some();
    let front: Front = match front {
        Some(text) if text.len() > 4096 => {
            return Err(Error::Tool("skill metadata exceeds 4 KiB".into()));
        }
        Some(text) if text.trim().is_empty() => Front::default(),
        // The line parser accepts `description: a: b`, which strict YAML rejects.
        Some(text) => match serde_yaml_ng::from_str(&text) {
            Ok(front) => front,
            Err(err) => lenient(&text)
                .ok_or_else(|| Error::Tool(format!("invalid skill metadata: {err}")))?,
        },
        // A skill written as one YAML document without `---` delimiters (`name:`,
        // `description: >`, steps ...): take its name and description from that document.
        None => whole_yaml(&content).unwrap_or_default(),
    };
    let description = front
        .description
        .map(|text| text.trim().to_owned())
        .filter(|text| !text.is_empty())
        .or_else(|| {
            body.lines()
                .map(|line| line.trim().trim_start_matches('#').trim())
                .find(|line| !line.is_empty())
                .map(str::to_owned)
        })
        .unwrap_or_default();
    Ok(Parsed {
        name: front
            .name
            .map(|n| n.trim().to_owned())
            .filter(|n| !n.is_empty()),
        description,
        paths: front.paths.map(List::items).unwrap_or_default(),
        allowed_tools: front.allowed_tools.map(List::items).unwrap_or_default(),
        steps: front.steps.map(List::items).unwrap_or_default(),
        body,
        has_frontmatter,
    })
}

/// Name and description of a file that is itself a YAML mapping starting with `name:`
/// or `description:`; `None` for ordinary Markdown.
fn whole_yaml(content: &str) -> Option<Front> {
    let first = content
        .lines()
        .map(str::trim)
        .find(|line| !line.is_empty())?;
    if !(first.starts_with("name:") || first.starts_with("description:")) {
        return None;
    }
    let value: serde_yaml_ng::Value = serde_yaml_ng::from_str(content).ok()?;
    let text = |key: &str| {
        value
            .get(key)
            .and_then(serde_yaml_ng::Value::as_str)
            .map(|text| text.trim().to_owned())
            .filter(|text| !text.is_empty())
    };
    let front = Front {
        name: text("name"),
        description: text("description"),
        ..Front::default()
    };
    (front.name.is_some() || front.description.is_some()).then_some(front)
}

/// `key: value` lines and `- item` block lists, read leniently. `None` when no
/// line has a known key (so arbitrary text is still rejected).
fn lenient(text: &str) -> Option<Front> {
    let mut front = Front::default();
    let mut lists: BTreeMap<&str, Vec<String>> = BTreeMap::new();
    let mut current: Option<&str> = None;
    let mut known = false;
    for line in text.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        if let Some(item) = line.strip_prefix('-') {
            if let Some(key) = current {
                lists.entry(key).or_default().push(item.trim().to_owned());
            }
            continue;
        }
        current = None;
        let Some((key, value)) = line.split_once(':') else {
            continue;
        };
        let value = value.trim();
        let unquoted = value
            .strip_prefix('"')
            .and_then(|v| v.strip_suffix('"'))
            .or_else(|| value.strip_prefix('\'').and_then(|v| v.strip_suffix('\'')))
            .unwrap_or(value)
            .to_owned();
        match key.trim() {
            "name" => front.name = Some(unquoted),
            "description" => front.description = Some(unquoted),
            key @ ("paths" | "allowed_tools" | "allowed-tools" | "steps") => {
                let key = if key == "allowed-tools" {
                    "allowed_tools"
                } else {
                    key
                };
                if value.is_empty() {
                    current = Some(key);
                } else {
                    lists
                        .entry(key)
                        .or_default()
                        .extend(List::One(unquoted).items());
                }
            }
            _ => continue,
        }
        known = true;
    }
    for (key, items) in lists {
        let list = Some(List::Many(items));
        match key {
            "paths" => front.paths = list,
            "allowed_tools" => front.allowed_tools = list,
            _ => front.steps = list,
        }
    }
    known.then_some(front)
}

/// Where skills come from, lowest precedence first (see `Skills::load`).
#[derive(Clone, Default)]
pub struct SkillSources {
    pub data_dir: PathBuf,
    pub cwd: PathBuf,
    pub home: Option<PathBuf>,
    /// Roots from config `extensions.skills` and plugin manifests (strict: errors fail startup).
    pub configured: Vec<Sourced<PathBuf>>,
    /// Plugins: (plugin name, plugin directory); `<dir>/skills` is scanned.
    pub plugins: Vec<(String, PathBuf)>,
    pub disabled: Vec<String>,
    pub builtin: bool,
}

pub fn home_dir() -> Option<PathBuf> {
    std::env::var_os(if cfg!(windows) { "USERPROFILE" } else { "HOME" })
        .filter(|home| !home.is_empty())
        .map(PathBuf::from)
}

/// The repository root (nearest ancestor holding `.git`) or `cwd` itself.
pub fn project_root(cwd: &Path) -> PathBuf {
    cwd.ancestors()
        .find(|dir| dir.join(".git").exists())
        .unwrap_or(cwd)
        .to_path_buf()
}

pub struct Skills {
    entries: RwLock<BTreeMap<String, Skill>>,
    registry: Arc<Registry>,
    disabled: BTreeSet<String>,
    data_dir: Option<PathBuf>,
    project_root: Option<PathBuf>,
    pub warnings: Vec<String>,
    /// What was loaded from, for [`refresh`](Self::refresh).
    sources: SkillSources,
    /// When the directories were last looked at, and what they looked like.
    seen: Mutex<(Instant, Vec<Stamp>)>,
}

/// A scanned directory or skill file and its modification time.
type Stamp = (PathBuf, Option<SystemTime>);

/// Directories are looked at again at most this often.
const REFRESH_INTERVAL: Duration = Duration::from_secs(1);

fn modified(path: &Path) -> Option<SystemTime> {
    std::fs::metadata(path)
        .and_then(|meta| meta.modified())
        .ok()
}

/// The skill directories `Skills::load` scans (configured roots aside).
fn skill_dirs(sources: &SkillSources) -> Vec<PathBuf> {
    let mut dirs: Vec<PathBuf> = sources
        .plugins
        .iter()
        .map(|(_, dir)| dir.join("skills"))
        .collect();
    if let Some(home) = &sources.home {
        dirs.push(home.join(".claude").join("skills"));
    }
    if !sources.data_dir.as_os_str().is_empty() {
        dirs.push(sources.data_dir.join("skills"));
    }
    if let Ok(cwd) = sources.cwd.canonicalize()
        && crate::safety::protected::too_wide(&cwd).is_none()
    {
        let root = project_root(&cwd);
        for dir in cwd.ancestors().take_while(|dir| dir.starts_with(&root)) {
            for sub in [".claude", ".cove"] {
                dirs.push(dir.join(sub).join("skills"));
            }
        }
    }
    dirs
}

/// Modification times of everything a reload would read: each skill directory,
/// its `*.md` files and `<name>/SKILL.md` files, and the configured roots' trees.
/// A new skill directory changes its parent; a new or edited file changes itself.
fn stamps(sources: &SkillSources) -> Vec<Stamp> {
    let mut stamps = Vec::new();
    for dir in skill_dirs(sources) {
        stamps.push((dir.clone(), modified(&dir)));
        let Ok(read) = std::fs::read_dir(&dir) else {
            continue;
        };
        for entry in read.flatten().take(1024) {
            let path = entry.path();
            let file = if path.is_dir() {
                path.join("SKILL.md")
            } else {
                path
            };
            let time = modified(&file);
            stamps.push((file, time));
        }
    }
    for root in &sources.configured {
        let mut walk = ignore::WalkBuilder::new(&root.value);
        walk.hidden(false)
            .follow_links(false)
            .max_depth(Some(4))
            .require_git(false);
        walk.filter_entry(|entry| entry.file_name() != ".git");
        for entry in walk.build().flatten().take(4096) {
            let path = entry.path();
            if entry.file_type().is_some_and(|kind| kind.is_dir())
                || entry.file_name() == "SKILL.md"
            {
                stamps.push((path.to_owned(), modified(path)));
            }
        }
    }
    stamps
}

#[derive(Deserialize)]
#[serde(tag = "action", rename_all = "snake_case", deny_unknown_fields)]
pub enum SkillArgs {
    List,
    Load { name: String },
    Read { name: String, path: PathBuf },
}

impl Skills {
    /// Strict discovery of configured roots only (no built-in or user layers).
    pub fn discover(roots: &[Sourced<PathBuf>], registry: Arc<Registry>) -> Result<Self> {
        Self::load(
            SkillSources {
                configured: roots.to_vec(),
                ..SkillSources::default()
            },
            registry,
        )
    }

    /// Layers, later wins on a name clash: built-in, plugin `skills/`, `~/.claude/skills`,
    /// `<data_dir>/skills`, configured roots, then `.claude/skills` and `.cove/skills` in each
    /// directory from the repository root down to cwd (innermost last). Subdirectories below
    /// cwd are never scanned, and a project skill resolving outside the project is refused.
    pub fn load(sources: SkillSources, registry: Arc<Registry>) -> Result<Self> {
        let seen = stamps(&sources);
        let mut skills = Self {
            entries: RwLock::new(BTreeMap::new()),
            registry,
            sources: sources.clone(),
            seen: Mutex::new((Instant::now(), seen)),
            disabled: sources
                .disabled
                .iter()
                .map(|name| name.trim().to_owned())
                .collect(),
            data_dir: (!sources.data_dir.as_os_str().is_empty()).then(|| sources.data_dir.clone()),
            project_root: None,
            warnings: Vec::new(),
        };
        let mut entries = BTreeMap::new();
        if sources.builtin {
            for (name, content) in BUILTIN {
                let parsed = parse(content)?;
                entries.insert(
                    (*name).to_owned(),
                    skill(name, parsed, Body::Embedded(content), None, Origin::Builtin),
                );
            }
        }
        for (plugin, dir) in &sources.plugins {
            skills.scan(
                &mut entries,
                &dir.join("skills"),
                Origin::Plugin,
                Some(plugin),
                None,
            );
        }
        if let Some(home) = &sources.home {
            skills.scan(
                &mut entries,
                &home.join(".claude").join("skills"),
                Origin::User,
                None,
                None,
            );
        }
        if let Some(data) = &skills.data_dir.clone() {
            skills.scan(&mut entries, &data.join("skills"), Origin::User, None, None);
        }
        let mut strict = BTreeSet::new();
        let mut scanned = 0;
        for source in &sources.configured {
            discover_root(source, &mut entries, &mut strict, &mut scanned)?;
        }
        // A home directory or a drive is no project: its `.cove`/`.claude` folders
        // belong to applications, not to a repository.
        if !sources.cwd.as_os_str().is_empty()
            && crate::safety::protected::too_wide(&sources.cwd).is_none()
        {
            let cwd = sources.cwd.canonicalize()?;
            let root = project_root(&cwd);
            let mut dirs: Vec<&Path> = cwd
                .ancestors()
                .take_while(|d| d.starts_with(&root))
                .collect();
            dirs.reverse();
            for dir in dirs {
                for sub in [".claude", ".cove"] {
                    skills.scan(
                        &mut entries,
                        &dir.join(sub).join("skills"),
                        Origin::Project,
                        None,
                        Some(&root),
                    );
                }
            }
            skills.project_root = Some(root);
        }
        for name in &skills.disabled {
            entries.remove(name);
        }
        if entries.len() > MAX_SKILLS {
            return Err(Error::Tool(format!("at most {MAX_SKILLS} skills")));
        }
        skills.entries = RwLock::new(entries);
        Ok(skills)
    }

    /// Layout: `<dir>/<name>/SKILL.md` and `<dir>/*.md`. Invalid files are warnings.
    fn scan(
        &mut self,
        entries: &mut BTreeMap<String, Skill>,
        dir: &Path,
        origin: Origin,
        plugin: Option<&String>,
        project: Option<&Path>,
    ) {
        let Ok(read) = std::fs::read_dir(dir) else {
            return;
        };
        let mut files = Vec::new();
        for entry in read.flatten().take(1024) {
            let path = entry.path();
            let name = entry.file_name().to_string_lossy().into_owned();
            if path.is_dir() {
                if !name.starts_with('.') && path.join("SKILL.md").is_file() {
                    files.push(path.join("SKILL.md"));
                }
            } else if name.ends_with(".md") && path.is_file() {
                files.push(path);
            }
        }
        files.sort();
        for file in files {
            if let Err(err) = self.load_file(entries, &file, origin.clone(), plugin, project) {
                self.warnings
                    .push(format!("skills: {} not loaded: {err}", file.display()));
            }
        }
    }

    fn load_file(
        &self,
        entries: &mut BTreeMap<String, Skill>,
        file: &Path,
        mut origin: Origin,
        plugin: Option<&String>,
        project: Option<&Path>,
    ) -> Result<()> {
        let real = file.canonicalize()?;
        if let Some(root) = project
            && !real.starts_with(root.canonicalize()?)
        {
            return Err(Error::Tool("points outside the project".into()));
        }
        let parsed = parse(&read_text(&real, FILE_LIMIT)?)?;
        let fallback = match file.file_stem().and_then(|s| s.to_str()) {
            Some("SKILL" | "skill") => file
                .parent()
                .and_then(|p| p.file_name())
                .and_then(|n| n.to_str())
                .unwrap_or_default()
                .to_owned(),
            Some(stem) => stem.to_owned(),
            None => String::new(),
        };
        let name = parsed.name.clone().unwrap_or(fallback);
        if !identifier(&name) {
            return Err(Error::Tool(format!("invalid skill name {name:?}")));
        }
        if origin == Origin::User
            && file
                .parent()
                .and_then(|p| p.file_name())
                .and_then(|n| n.to_str())
                .is_some_and(|dir| dir.starts_with(AUTO_PREFIX))
        {
            origin = Origin::Auto;
        }
        entries.insert(
            name.clone(),
            skill(&name, parsed, Body::File(real), plugin.cloned(), origin),
        );
        Ok(())
    }

    fn read(&self) -> std::sync::RwLockReadGuard<'_, BTreeMap<String, Skill>> {
        self.entries.read().unwrap_or_else(|e| e.into_inner())
    }

    fn enabled(&self, skill: &Skill) -> Result<bool> {
        self.registry
            .active("skill", &skill.name, skill.plugin.as_deref())
    }

    pub fn contains(&self, name: &str) -> bool {
        self.read().contains_key(name)
    }

    /// Enabled skill names.
    pub fn names(&self) -> Vec<String> {
        self.read()
            .values()
            .filter(|skill| self.enabled(skill).unwrap_or(false))
            .map(|skill| skill.name.clone())
            .collect()
    }

    /// Enabled skills as (name, description), for the system prompt index.
    pub fn index(&self) -> Result<Vec<(String, String)>> {
        let mut list = Vec::new();
        for skill in self.read().values() {
            if self.enabled(skill)? {
                let mut description = clip(&skill.description, DESCRIPTION_LIMIT);
                if !skill.paths.is_empty() {
                    description.push_str(&format!(" (files: {})", skill.paths.join(",")));
                }
                list.push((skill.name.clone(), description));
            }
        }
        Ok(list)
    }

    /// Operator listing with sources and conditions.
    pub fn listing(&self) -> Result<Value> {
        let mut list = Vec::new();
        for skill in self.read().values() {
            list.push(
                json!({"name":skill.name,"origin":skill.origin.label(),"plugin":skill.plugin,
                "description":clip(&skill.description, DESCRIPTION_LIMIT),"paths":skill.paths,
                "allowed_tools":skill.allowed_tools,"steps":skill.steps.len(),
                "source":source(&skill.body),"enabled":self.enabled(skill)?}),
            );
        }
        Ok(json!(list))
    }

    fn get(&self, name: &str) -> Result<Skill> {
        let skill = self
            .read()
            .get(name)
            .cloned()
            .ok_or_else(|| Error::Tool("unknown skill".into()))?;
        if !self.enabled(&skill)? {
            return Err(Error::Tool("skill is disabled".into()));
        }
        if let Body::File(path) = &skill.body
            && path.canonicalize()? != *path
        {
            return Err(Error::Tool(
                "skill path replaced by a link; reload configuration".into(),
            ));
        }
        Ok(skill)
    }

    fn body(skill: &Skill) -> Result<String> {
        match &skill.body {
            Body::Embedded(text) => Ok(parse(text)?.body),
            Body::File(path) => Ok(parse(&read_text(path, FILE_LIMIT)?)?.body),
        }
    }

    /// The text a skill invocation hands the model: ordered steps, a tool allowlist
    /// directive, the body and, for `/<skill> args`, the user's arguments.
    pub fn render(&self, name: &str, arguments: Option<&str>) -> Result<String> {
        let skill = self.get(name)?;
        let mut out = format!("[Skill: {}]\n\n", skill.name);
        if !skill.steps.is_empty() {
            out.push_str("Follow these steps in order. Do not skip ahead or reorder them; treat each one as a checkpoint to complete (and verify) before starting the next:\n");
            for (index, step) in skill.steps.iter().enumerate() {
                out.push_str(&format!("{}. {step}\n", index + 1));
            }
            out.push('\n');
        }
        if !skill.allowed_tools.is_empty() {
            out.push_str(&format!("While executing this skill, only use these tools: {}. If the task genuinely requires a different tool, explain why before using it. (This narrows tool choice; it never grants permissions.)\n\n", skill.allowed_tools.join(", ")));
        }
        out.push_str(Self::body(&skill)?.trim_end());
        out.push_str("\n\nFollow these instructions to complete the task.");
        if let Some(arguments) = arguments.map(str::trim).filter(|a| !a.is_empty()) {
            out.push_str(&format!("\n\nUser arguments: {arguments}"));
        }
        Ok(out)
    }

    /// Full file text and source for `/config skills show`.
    pub fn show(&self, name: &str) -> Result<Value> {
        let skill = self.get(name)?;
        let text = match &skill.body {
            Body::Embedded(text) => (*text).to_owned(),
            Body::File(path) => read_text(path, FILE_LIMIT)?,
        };
        Ok(
            json!({"name":skill.name,"origin":skill.origin.label(),"source":source(&skill.body),"content":text}),
        )
    }

    pub fn execute(&self, args: SkillArgs) -> Result<Value> {
        match args {
            SkillArgs::List => {
                let mut list = Vec::new();
                for skill in self.read().values() {
                    if self.enabled(skill)? {
                        list.push(json!({"name":skill.name,"description":clip(&skill.description, DESCRIPTION_LIMIT),
                            "source":source(&skill.body),"paths":skill.paths}));
                    }
                }
                Ok(json!(list))
            }
            SkillArgs::Load { name } => {
                let skill = self.get(&name)?;
                Ok(json!({"name":name,"source":source(&skill.body),
                    "untrusted_instruction_data":self.render(&name, None)?}))
            }
            SkillArgs::Read { name, path } => {
                let skill = self.get(&name)?;
                if path.is_absolute()
                    || path
                        .components()
                        .any(|c| !matches!(c, Component::Normal(_)))
                {
                    return Err(Error::Tool(
                        "skill resource must be a relative path without traversal".into(),
                    ));
                }
                let Body::File(file) = &skill.body else {
                    return Err(Error::Tool("built-in skills have no resources".into()));
                };
                let root = file
                    .parent()
                    .ok_or_else(|| Error::Tool("skill has no directory".into()))?;
                let target = root.join(path).canonicalize()?;
                if !target.starts_with(root) || !target.is_file() {
                    return Err(Error::Tool("skill resource escaped its directory".into()));
                }
                Ok(
                    json!({"source":target,"untrusted_resource_data":read_text(&target, FILE_LIMIT)?}),
                )
            }
        }
    }

    /// Enabled conditional skills whose `paths` match `path` (relative to cwd or absolute).
    pub fn matching(&self, path: &str, cwd: &Path) -> Vec<(String, String)> {
        let path = path.replace('\\', "/");
        let absolute = if Path::new(&path).is_absolute() {
            PathBuf::from(&path)
        } else {
            cwd.join(&path)
        };
        let mut relatives = Vec::new();
        for root in [self.project_root.as_deref(), Some(cwd)]
            .into_iter()
            .flatten()
        {
            if let Ok(rel) = absolute.strip_prefix(root) {
                let rel = rel.to_string_lossy().replace('\\', "/");
                if !relatives.contains(&rel) {
                    relatives.push(rel);
                }
            }
        }
        let base = absolute
            .file_name()
            .map(|n| n.to_string_lossy().into_owned())
            .unwrap_or_default();
        let mut out = Vec::new();
        for skill in self.read().values() {
            if skill.paths.is_empty() || !self.enabled(skill).unwrap_or(false) {
                continue;
            }
            let hit = skill.paths.iter().any(|pattern| {
                let pattern = pattern.replace('\\', "/");
                let pattern = pattern.trim_start_matches("./").trim_start_matches('/');
                let Ok(glob) = globset::GlobBuilder::new(pattern)
                    .literal_separator(true)
                    .build()
                else {
                    return false;
                };
                let matcher = glob.compile_matcher();
                if pattern.contains('/') {
                    relatives.iter().any(|rel| matcher.is_match(rel))
                } else {
                    matcher.is_match(&base)
                }
            });
            if hit {
                out.push((
                    skill.name.clone(),
                    clip(&skill.description, DESCRIPTION_LIMIT),
                ));
            }
        }
        out
    }

    /// Loads the skills again when a skill directory or file changed since the
    /// last look (at most once per [`REFRESH_INTERVAL`]): a `SKILL.md` dropped in
    /// while the REPL runs shows up without a restart. True when reloaded; on an
    /// error the skills loaded before stay.
    pub fn refresh(&self) -> Result<bool> {
        {
            let mut seen = self.seen.lock().unwrap_or_else(|e| e.into_inner());
            if seen.0.elapsed() < REFRESH_INTERVAL {
                return Ok(false);
            }
            let now = stamps(&self.sources);
            seen.0 = Instant::now();
            if now == seen.1 {
                return Ok(false);
            }
            // Taken before loading: a broken file is reported once, not every second.
            seen.1 = now;
        }
        let fresh = Self::load(self.sources.clone(), self.registry.clone())?;
        let entries = fresh
            .entries
            .into_inner()
            .unwrap_or_else(|e| e.into_inner());
        *self.entries.write().unwrap_or_else(|e| e.into_inner()) = entries;
        Ok(true)
    }

    /// Registers a user skill file written at runtime (`/config skills install|create`).
    pub fn add(&self, file: &Path) -> Result<String> {
        let mut entries = self.entries.write().unwrap_or_else(|e| e.into_inner());
        let before: BTreeSet<String> = entries.keys().cloned().collect();
        self.load_file(&mut entries, file, Origin::User, None, None)?;
        Ok(entries
            .keys()
            .find(|name| !before.contains(*name))
            .cloned()
            .unwrap_or_default())
    }

    /// True when `name` is an existing skill that is not a learned (auto) one.
    pub fn is_curated(&self, name: &str) -> bool {
        self.read()
            .get(name)
            .is_some_and(|skill| skill.origin != Origin::Auto)
    }

    /// Writes a learned skill to `<data_dir>/skills/auto-<slug>/SKILL.md` and registers it.
    /// Never shadows a curated skill and never replaces a file it did not generate.
    pub fn save_auto(
        &self,
        name: &str,
        description: &str,
        body: &str,
        session: &str,
    ) -> Result<PathBuf> {
        if !identifier(name) {
            return Err(Error::Tool(format!("invalid learned skill name {name:?}")));
        }
        if self.is_curated(name) || self.disabled.contains(name) {
            return Err(Error::Tool(format!(
                "skill {name} already exists; not replaced"
            )));
        }
        let data = self
            .data_dir
            .as_ref()
            .ok_or_else(|| Error::Tool("no skill directory".into()))?;
        let dir = data
            .join("skills")
            .join(format!("{AUTO_PREFIX}{}", slug(name)));
        let path = dir.join("SKILL.md");
        if path.exists() {
            let existing = read_text(&path, FILE_LIMIT)?;
            let generated = existing
                .replace("\r\n", "\n")
                .lines()
                .any(|line| line.starts_with("generated: "));
            if !generated || parse(&existing)?.name.as_deref() != Some(name) {
                return Err(Error::Tool(format!(
                    "{} exists and was not generated for {name}; left unchanged",
                    path.display()
                )));
            }
        }
        std::fs::create_dir_all(&dir)?;
        let one_line = |text: &str| text.split_whitespace().collect::<Vec<_>>().join(" ");
        let description = one_line(description).replace(['"', '\\'], "'");
        let text = format!(
            "---\nname: {name}\ndescription: \"{description}\"\ngenerated: \"{}\"\nsource_session: \"{}\"\n---\n{}\n",
            unix_now(),
            one_line(session),
            body.trim()
        );
        let parsed = parse(&text)?;
        let temp = dir.join(format!(".SKILL.md.{}", uuid::Uuid::new_v4()));
        std::fs::write(&temp, &text)?;
        std::fs::rename(&temp, &path)?;
        let real = path.canonicalize()?;
        self.entries
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .insert(
                name.to_owned(),
                skill(name, parsed, Body::File(real), None, Origin::Auto),
            );
        Ok(path)
    }
}

fn unix_now() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| d.as_secs())
}

fn source(body: &Body) -> Value {
    match body {
        Body::Embedded(_) => json!("builtin"),
        Body::File(path) => json!(path),
    }
}

fn skill(name: &str, parsed: Parsed, body: Body, plugin: Option<String>, origin: Origin) -> Skill {
    Skill {
        name: name.to_owned(),
        description: parsed.description,
        body,
        plugin,
        origin,
        paths: parsed.paths,
        allowed_tools: parsed.allowed_tools,
        steps: parsed.steps,
    }
}

/// Lower-case ASCII slug plus a short hash of the exact name, so similar names never collide.
pub fn slug(name: &str) -> String {
    use sha2::{Digest, Sha256};
    let mut out = String::new();
    for c in name.to_lowercase().chars() {
        if c.is_ascii_alphanumeric() {
            out.push(c);
        } else if !out.is_empty() && !out.ends_with('-') {
            out.push('-');
        }
    }
    let mut slug = out.trim_end_matches('-').to_owned();
    slug.truncate(40);
    let slug = slug.trim_end_matches('-');
    let hash: String = Sha256::digest(name.as_bytes())[..3]
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect();
    if slug.is_empty() {
        hash
    } else {
        format!("{slug}-{hash}")
    }
}

/// Configured roots keep the strict pre-existing semantics: deep walk, frontmatter
/// required, duplicates inside this layer are errors, plugin skills are `plugin/name`.
fn discover_root(
    source: &Sourced<PathBuf>,
    entries: &mut BTreeMap<String, Skill>,
    strict: &mut BTreeSet<String>,
    scanned: &mut usize,
) -> Result<()> {
    let mut walk = ignore::WalkBuilder::new(&source.value);
    walk.hidden(false)
        .follow_links(false)
        .max_depth(Some(4))
        .require_git(false);
    walk.filter_entry(|entry| entry.file_name() != ".git");
    for entry in walk.build() {
        *scanned += 1;
        if *scanned > 4096 {
            return Err(Error::Tool("skill scan exceeds 4096 entries".into()));
        }
        let entry = entry.map_err(|err| Error::Tool(format!("skill discovery: {err}")))?;
        if entry.file_name() != "SKILL.md" || !entry.file_type().is_some_and(|t| t.is_file()) {
            continue;
        }
        let path = entry.path().canonicalize()?;
        if !path.starts_with(&source.value) {
            return Err(Error::Tool("skill path escaped configured root".into()));
        }
        let parsed = parse(&read_text(&path, FILE_LIMIT)?)?;
        if !parsed.has_frontmatter {
            return Err(Error::Tool(format!(
                "skill requires YAML frontmatter: {}",
                path.display()
            )));
        }
        let meta_name = parsed.name.clone().unwrap_or_default();
        if !identifier(&meta_name)
            || parsed.description.trim().is_empty()
            || parsed.description.len() > 1024
        {
            return Err(Error::Tool("invalid skill name/description".into()));
        }
        let name = source
            .plugin
            .as_ref()
            .map_or(meta_name.clone(), |p| format!("{p}/{meta_name}"));
        if !strict.insert(name.clone()) {
            return Err(Error::Tool(format!("duplicate skill {name}")));
        }
        let origin = if source.plugin.is_some() {
            Origin::Plugin
        } else {
            Origin::Configured
        };
        entries.insert(
            name.clone(),
            skill(
                &name,
                parsed,
                Body::File(path),
                source.plugin.clone(),
                origin,
            ),
        );
        if strict.len() > 128 {
            return Err(Error::Tool("at most 128 skills".into()));
        }
    }
    Ok(())
}

/// `TurnHook` that appends `<relevant_skills>` once per session when a read/write/edit
/// touches a file matching a conditional skill's `paths`.
pub struct SkillNotes {
    skills: Arc<Skills>,
    cwd: PathBuf,
    state: Mutex<(String, HashSet<String>)>,
}

impl SkillNotes {
    pub fn new(skills: Arc<Skills>, cwd: PathBuf) -> Self {
        Self {
            skills,
            cwd,
            state: Mutex::new((String::new(), HashSet::new())),
        }
    }
}

impl TurnHook for SkillNotes {
    fn task_note<'a>(&'a self, session: &'a Session) -> BoxFuture<'a, Option<String>> {
        if let Ok(mut state) = self.state.lock()
            && state.0 != session.id
        {
            *state = (session.id.clone(), HashSet::new());
        }
        Box::pin(async { None })
    }

    fn tool_note(&self, name: &str, input: &Value, output: &ToolOutput) -> Option<String> {
        if output.failed || !matches!(name, "read" | "write" | "edit") {
            return None;
        }
        let path = input.get("path").and_then(Value::as_str)?;
        let matches = self.skills.matching(path, &self.cwd);
        let mut state = self.state.lock().ok()?;
        let fresh: Vec<_> = matches
            .into_iter()
            .filter(|(skill, _)| state.1.insert(skill.clone()))
            .collect();
        if fresh.is_empty() {
            return None;
        }
        let mut note = String::from(
            "<relevant_skills>\nThese skills apply to files like this one; load with skill action=load if useful:\n",
        );
        for (skill, description) in fresh {
            note.push_str(&format!("- {skill}: {description}\n"));
        }
        note.push_str("</relevant_skills>");
        Some(note)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn registry(dir: &Path) -> Arc<Registry> {
        Arc::new(Registry::open(dir).unwrap())
    }

    fn write(path: &Path, text: &str) {
        std::fs::create_dir_all(path.parent().unwrap()).unwrap();
        std::fs::write(path, text).unwrap();
    }

    #[test]
    fn a_whole_yaml_skill_without_delimiters_keeps_its_description() {
        let parsed = parse(
            "name: commit\r\ndescription: >\r\n  生成规范的 Git commit message。\r\n  读取 staged 变更。\r\nversion: 1.0\r\n\r\nexecution_flow:\r\n  - step: 1\r\n",
        )
        .unwrap();
        assert_eq!(parsed.name.as_deref(), Some("commit"));
        assert_eq!(
            parsed.description,
            "生成规范的 Git commit message。 读取 staged 变更。"
        );
        // Ordinary Markdown still takes its first line.
        assert_eq!(parse("# Title\nbody").unwrap().description, "Title");
    }

    #[test]
    fn frontmatter_lists_bom_crlf_and_fallbacks() {
        let parsed = parse("\u{feff}---\r\nname: go\r\ndescription: \"Go help\"\r\npaths: \"*.go, internal/**/*.go\"\r\nallowed-tools: [read, grep]\r\nsteps:\r\n  - Read\r\n  - Fix\r\n---\r\nBody\r\n").unwrap();
        assert_eq!(parsed.name.as_deref(), Some("go"));
        assert_eq!(parsed.paths, ["*.go", "internal/**/*.go"]);
        assert_eq!(parsed.allowed_tools, ["read", "grep"]);
        assert_eq!(parsed.steps, ["Read", "Fix"]);
        assert_eq!(parsed.body, "Body\n");
        let bare = parse("# Title here\nmore").unwrap();
        assert!(!bare.has_frontmatter);
        assert_eq!(bare.description, "Title here");
    }

    #[test]
    fn layers_precedence_disabled_and_project_escape() {
        let data = tempfile::tempdir().unwrap();
        let home = tempfile::tempdir().unwrap();
        let project = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        std::fs::create_dir(project.path().join(".git")).unwrap();
        let cwd = project.path().join("sub");
        std::fs::create_dir_all(&cwd).unwrap();
        write(
            &home.path().join(".claude/skills/plan/SKILL.md"),
            "---\ndescription: user plan\n---\nuser body\n",
        );
        write(
            &data.path().join("skills/tidy.md"),
            "---\ndescription: tidy\n---\ntidy\n",
        );
        write(
            &project.path().join(".cove/skills/plan/SKILL.md"),
            "---\ndescription: root plan\n---\nroot\n",
        );
        write(
            &cwd.join(".claude/skills/plan/SKILL.md"),
            "---\ndescription: inner plan\n---\ninner\n",
        );
        write(
            &cwd.join(".cove/skills/go/SKILL.md"),
            "---\ndescription: go\npaths: \"*.go\"\n---\ngo\n",
        );
        write(
            &outside.path().join("SKILL.md"),
            "---\ndescription: secret\n---\nsecret\n",
        );
        let link_ok = {
            #[cfg(unix)]
            {
                std::os::unix::fs::symlink(outside.path(), cwd.join(".cove/skills/leak")).is_ok()
            }
            #[cfg(windows)]
            {
                std::os::windows::fs::symlink_dir(outside.path(), cwd.join(".cove/skills/leak"))
                    .is_ok()
            }
        };
        let skills = Skills::load(
            SkillSources {
                data_dir: data.path().into(),
                cwd: cwd.clone(),
                home: Some(home.path().into()),
                disabled: vec!["spike".into()],
                builtin: true,
                ..SkillSources::default()
            },
            registry(data.path()),
        )
        .unwrap();
        assert!(
            skills
                .render("plan", Some("x"))
                .unwrap()
                .contains("inner\n")
        );
        assert!(
            skills
                .render("plan", Some("x"))
                .unwrap()
                .ends_with("User arguments: x")
        );
        assert!(skills.contains("tidy") && skills.contains("commit-messages"));
        assert!(!skills.contains("spike"));
        if link_ok {
            assert!(!skills.contains("leak"));
            assert!(
                skills
                    .warnings
                    .iter()
                    .any(|w| w.contains("outside the project"))
            );
        }
        let cwd = cwd.canonicalize().unwrap();
        assert_eq!(skills.matching("main.go", &cwd)[0].0, "go");
        assert!(skills.matching("main.rs", &cwd).is_empty());
    }

    #[test]
    fn steps_and_tools_render_and_notes_fire_once() {
        let data = tempfile::tempdir().unwrap();
        let cwd = tempfile::tempdir().unwrap();
        write(
            &cwd.path().join(".cove/skills/ts.md"),
            "---\nname: ts\ndescription: ts\npaths: [\"web/**/*.ts\"]\nallowed_tools: read\nsteps: [Read, Test]\n---\nUse strict mode.\n",
        );
        let skills = Arc::new(
            Skills::load(
                SkillSources {
                    data_dir: data.path().into(),
                    cwd: cwd.path().into(),
                    ..SkillSources::default()
                },
                registry(data.path()),
            )
            .unwrap(),
        );
        let text = skills.render("ts", None).unwrap();
        assert!(text.contains("1. Read\n2. Test") && text.contains("only use these tools: read"));
        let notes = SkillNotes::new(skills.clone(), cwd.path().canonicalize().unwrap());
        let ok = ToolOutput {
            content: String::new(),
            failed: false,
            ..Default::default()
        };
        let input = json!({"path":"web/src/a.ts"});
        assert!(
            notes
                .tool_note("read", &input, &ok)
                .unwrap()
                .contains("<relevant_skills>")
        );
        assert!(notes.tool_note("read", &input, &ok).is_none());
        assert!(
            notes
                .tool_note("read", &json!({"path":"a.ts"}), &ok)
                .is_none()
        );
    }

    #[test]
    fn a_skill_dropped_into_a_directory_appears_on_refresh() {
        let data = tempfile::tempdir().unwrap();
        let skills = Skills::load(
            SkillSources {
                data_dir: data.path().into(),
                builtin: true,
                ..SkillSources::default()
            },
            registry(data.path()),
        )
        .unwrap();
        let rewind = |skills: &Skills| {
            skills.seen.lock().unwrap().0 = Instant::now() - REFRESH_INTERVAL * 2;
        };
        assert!(!skills.contains("deploy"));
        // Within the interval the directories are not looked at.
        write(
            &data.path().join("skills/deploy/SKILL.md"),
            "---\nname: deploy\ndescription: Ship it\n---\nsteps\n",
        );
        assert!(!skills.refresh().unwrap());
        rewind(&skills);
        assert!(skills.refresh().unwrap());
        assert!(skills.contains("deploy") && skills.contains("plan"));
        rewind(&skills);
        assert!(!skills.refresh().unwrap(), "nothing changed");
        // An edit is seen too (a later modification time than the first write).
        std::thread::sleep(Duration::from_millis(20));
        write(
            &data.path().join("skills/deploy/SKILL.md"),
            "---\nname: deploy\ndescription: Ship it carefully\n---\nsteps\n",
        );
        rewind(&skills);
        assert!(skills.refresh().unwrap());
        let index = skills.index().unwrap();
        assert!(
            index
                .iter()
                .any(|(name, text)| name == "deploy" && text == "Ship it carefully")
        );
    }

    #[test]
    fn learned_skills_never_shadow_curated_ones() {
        let data = tempfile::tempdir().unwrap();
        let skills = Skills::load(
            SkillSources {
                data_dir: data.path().into(),
                builtin: true,
                ..SkillSources::default()
            },
            registry(data.path()),
        )
        .unwrap();
        assert!(skills.save_auto("plan", "d", "b", "s").is_err());
        let path = skills
            .save_auto("deploy-check", "Check deploys", "1. run", "s1")
            .unwrap();
        assert!(
            path.parent()
                .unwrap()
                .file_name()
                .unwrap()
                .to_string_lossy()
                .starts_with("auto-deploy-check-")
        );
        assert!(
            skills
                .render("deploy-check", None)
                .unwrap()
                .contains("1. run")
        );
        skills
            .save_auto("deploy-check", "Check deploys", "2. again", "s2")
            .unwrap();
        assert_ne!(slug("K8s Deploy"), slug("k8s-deploy"));
    }
}
