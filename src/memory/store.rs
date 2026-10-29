use std::{
    io::Read,
    path::{Path, PathBuf},
    sync::Arc,
};

use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use tokio_util::sync::CancellationToken;

use crate::{
    domain::{Error, Result},
    extensions::identifier,
    safety::permission::{Authorizer, Effect},
    session::{atomic_json, new_id},
};

fn text(path: &Path, limit: usize) -> Result<String> {
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

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Scope {
    Project,
    Global,
}

impl Scope {
    pub fn name(self) -> &'static str {
        match self {
            Self::Project => "project",
            Self::Global => "global",
        }
    }
}

#[derive(Deserialize)]
#[serde(tag = "action", rename_all = "snake_case", deny_unknown_fields)]
pub enum MemoryArgs {
    List {
        scope: Scope,
    },
    Search {
        scope: Scope,
        query: String,
    },
    Read {
        scope: Scope,
        id: String,
    },
    Add {
        scope: Scope,
        title: String,
        content: String,
        source: String,
    },
    Update {
        scope: Scope,
        id: String,
        title: String,
        content: String,
        source: String,
    },
    Delete {
        scope: Scope,
        id: String,
    },
}

/// Largest memory content.
pub const ENTRY_LIMIT: usize = 25 * 1024;
/// Largest total content of one scope.
pub const TOTAL_LIMIT: usize = 300 * 1024;
const ENTRIES_LIMIT: usize = 256;
const SOURCES_KEPT: usize = 8;

/// What produced one revision of a memory: inputs, not proof of truth.
#[derive(Clone, Debug, Default, Deserialize, Serialize, PartialEq)]
pub struct ProvenanceSource {
    /// "tool", "operator", "extraction" or "dream".
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub session: String,
    /// 1-based inclusive message range of the source transcript window.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub messages: Option<(usize, usize)>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub transcript_hash: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub evidence: String,
    #[serde(default)]
    pub at: u64,
    /// Hash of the content this source produced; differs after an external edit.
    #[serde(default)]
    pub content_hash: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Entry {
    pub version: u32,
    pub id: String,
    pub title: String,
    pub content: String,
    pub source: String,
    #[serde(default)]
    pub created: u64,
    #[serde(default)]
    pub updated: u64,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub provenance: Vec<ProvenanceSource>,
}

pub fn content_hash(content: &str) -> String {
    format!("{:x}", Sha256::digest(content.as_bytes()))
}

/// Refuses content that looks like a credential; memories are injected into prompts.
pub fn screen(content: &str) -> Result<()> {
    static SECRET: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    let pattern = SECRET.get_or_init(|| {
        regex::Regex::new(
            r"-----BEGIN [A-Z ]*PRIVATE KEY-----|\bsk-[A-Za-z0-9_-]{20,}|\bAKIA[0-9A-Z]{16}\b|\bgh[pousr]_[A-Za-z0-9]{30,}|\bxox[abprs]-[A-Za-z0-9-]{10,}|\bAIza[0-9A-Za-z_-]{30,}",
        )
        .expect("valid secret pattern")
    });
    if pattern.is_match(content) {
        return Err(Error::Tool(
            "memory content looks like a credential; secrets are never stored".into(),
        ));
    }
    Ok(())
}

pub struct Memory {
    project: PathBuf,
    global: PathBuf,
    authorizer: Arc<dyn Authorizer>,
}

impl Memory {
    pub fn new(data: &Path, root: &Path, authorizer: Arc<dyn Authorizer>) -> Result<Self> {
        let root = root.canonicalize()?;
        if !root.is_dir() {
            return Err(Error::Tool("memory workspace must be a directory".into()));
        }
        let hash = format!("{:x}", Sha256::digest(root.as_os_str().as_encoded_bytes()));
        let memory = Self {
            project: data.join("memory").join(hash),
            global: data.join("memory").join("global"),
            authorizer,
        };
        std::fs::create_dir_all(&memory.project)?;
        std::fs::create_dir_all(&memory.global)?;
        Ok(memory)
    }

    /// Same directories, a different authorizer (the dream run's memory-only one).
    pub fn with_authorizer(&self, authorizer: Arc<dyn Authorizer>) -> Self {
        Self {
            project: self.project.clone(),
            global: self.global.clone(),
            authorizer,
        }
    }

    pub fn directory(&self, scope: Scope) -> &Path {
        match scope {
            Scope::Project => &self.project,
            Scope::Global => &self.global,
        }
    }

    pub async fn authorize(&self, args: &MemoryArgs, cancel: CancellationToken) -> Result<()> {
        let effect = match args {
            MemoryArgs::Add { scope, .. }
            | MemoryArgs::Update { scope, .. }
            | MemoryArgs::Delete { scope, .. } => Effect::Write(self.directory(*scope).into()),
            _ => Effect::Read,
        };
        self.authorizer.authorize(effect, cancel).await
    }

    /// Authorizes an operator or background write into `scope`.
    pub async fn authorize_write(&self, scope: Scope, cancel: CancellationToken) -> Result<()> {
        self.authorizer
            .authorize(Effect::Write(self.directory(scope).into()), cancel)
            .await
    }

    fn path(&self, scope: Scope, id: &str) -> Result<PathBuf> {
        if !identifier(id) {
            return Err(Error::Tool("invalid memory ID".into()));
        }
        let path = self.directory(scope).join(format!("{id}.json"));
        if let Ok(meta) = std::fs::symlink_metadata(&path)
            && (!meta.is_file() || meta.file_type().is_symlink())
        {
            return Err(Error::Tool("memory must be a regular file".into()));
        }
        Ok(path)
    }

    fn read(&self, scope: Scope, id: &str) -> Result<Entry> {
        let path = self.path(scope, id)?;
        let mut entry: Entry = serde_json::from_str(&text(&path, 128 * 1024)?)?;
        if entry.version != 1 || entry.id != id {
            return Err(Error::Tool("memory identity/version mismatch".into()));
        }
        if entry.updated == 0 {
            entry.updated = std::fs::metadata(&path)
                .and_then(|meta| meta.modified())
                .ok()
                .and_then(|time| time.duration_since(std::time::UNIX_EPOCH).ok())
                .map_or(0, |time| time.as_secs());
        }
        Ok(entry)
    }

    /// Cross-process write lock of one scope; waits up to five seconds.
    fn lock(&self, scope: Scope) -> Result<std::fs::File> {
        let lock = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .create(true)
            .truncate(false)
            .open(self.directory(scope).join(".lock"))?;
        let deadline = std::time::Instant::now() + std::time::Duration::from_secs(5);
        loop {
            match fs2::FileExt::try_lock_exclusive(&lock) {
                Ok(()) => return Ok(lock),
                Err(err) if std::time::Instant::now() >= deadline => {
                    return Err(Error::Tool(format!(
                        "memory scope is busy or cannot be locked: {err}"
                    )));
                }
                Err(_) => std::thread::sleep(std::time::Duration::from_millis(50)),
            }
        }
    }

    /// Every readable entry of `scope`, sorted by ID; unreadable files are skipped.
    pub fn entries(&self, scope: Scope) -> Result<Vec<Entry>> {
        let mut entries = Vec::new();
        for (scanned, file) in std::fs::read_dir(self.directory(scope))?.enumerate() {
            if scanned >= 512 {
                return Err(Error::Tool("memory directory exceeds 512 files".into()));
            }
            let path = file?.path();
            if path.extension().is_none_or(|e| e != "json") {
                continue;
            }
            let Some(id) = path.file_stem().and_then(|s| s.to_str()) else {
                continue;
            };
            if let Ok(entry) = self.read(scope, id) {
                entries.push(entry);
            }
            if entries.len() > ENTRIES_LIMIT {
                return Err(Error::Tool("memory scope exceeds 256 entries".into()));
            }
        }
        entries.sort_by(|a, b| a.id.cmp(&b.id));
        Ok(entries)
    }

    /// Both scopes, project first.
    pub fn all(&self) -> Result<Vec<(Scope, Entry)>> {
        let mut all: Vec<(Scope, Entry)> = self
            .entries(Scope::Project)?
            .into_iter()
            .map(|entry| (Scope::Project, entry))
            .collect();
        all.extend(
            self.entries(Scope::Global)?
                .into_iter()
                .map(|entry| (Scope::Global, entry)),
        );
        Ok(all)
    }

    /// BM25 (CJK-aware) with recency decay over `scopes`; best first.
    pub fn search(
        &self,
        scopes: &[Scope],
        query: &str,
        limit: usize,
    ) -> Result<Vec<(Scope, Entry, f64)>> {
        let mut candidates = Vec::new();
        for scope in scopes {
            candidates.extend(self.entries(*scope)?.into_iter().map(|e| (*scope, e)));
        }
        let documents: Vec<(String, u64)> = candidates
            .iter()
            .map(|(_, entry)| {
                (
                    format!("{}\n{}\n{}", entry.title, entry.content, entry.source),
                    entry.updated,
                )
            })
            .collect();
        let ranked = crate::memory::recall::rank(&documents, query, crate::session::now());
        let mut slots: Vec<Option<(Scope, Entry)>> = candidates.into_iter().map(Some).collect();
        Ok(ranked
            .into_iter()
            .take(limit)
            .filter_map(|(index, score)| {
                slots[index]
                    .take()
                    .map(|(scope, entry)| (scope, entry, score))
            })
            .collect())
    }

    /// Resolves a full ID or a unique prefix (at least 4 characters) in either scope.
    pub fn find(&self, reference: &str) -> Result<(Scope, Entry)> {
        if reference.len() < 4 {
            return Err(Error::Tool(
                "memory ID prefix needs at least 4 characters".into(),
            ));
        }
        let matches: Vec<_> = self
            .all()?
            .into_iter()
            .filter(|(_, entry)| entry.id.starts_with(reference))
            .collect();
        match <[_; 1]>::try_from(matches) {
            Ok([one]) => Ok(one),
            Err(matches) if matches.is_empty() => Err(Error::Tool("no memory with that ID".into())),
            Err(_) => Err(Error::Tool("ambiguous memory ID prefix".into())),
        }
    }

    fn list(&self, scope: Scope, query: Option<&str>, cancel: &CancellationToken) -> Result<Value> {
        if query.is_some_and(|q| q.trim().is_empty() || q.len() > 256) {
            return Err(Error::Tool("memory query must be 1..256 bytes".into()));
        }
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        Ok(match query {
            None => json!(
                self.entries(scope)?
                    .into_iter()
                    .map(|entry| json!({"id":entry.id,"title":entry.title,"source":entry.source}))
                    .collect::<Vec<_>>()
            ),
            Some(query) => json!(
                self.search(&[scope], query, 20)?
                    .into_iter()
                    .map(|(_, entry, score)| json!({"id":entry.id,"title":entry.title,"source":entry.source,
                        "score":(score * 1000.0).round() / 1000.0}))
                    .collect::<Vec<_>>()
            ),
        })
    }

    pub fn execute(&self, args: MemoryArgs, cancel: &CancellationToken) -> Result<Value> {
        self.execute_as(args, cancel, "tool")
    }

    /// `execute` with the provenance kind of writes ("tool", "dream", ...).
    pub fn execute_as(
        &self,
        args: MemoryArgs,
        cancel: &CancellationToken,
        kind: &str,
    ) -> Result<Value> {
        if cancel.is_cancelled() {
            return Err(Error::Cancelled);
        }
        let tool = |source: &str| ProvenanceSource {
            kind: kind.into(),
            evidence: source.chars().take(256).collect(),
            ..ProvenanceSource::default()
        };
        match args {
            MemoryArgs::List { scope } => self.list(scope, None, cancel),
            MemoryArgs::Search { scope, query } => self.list(scope, Some(&query), cancel),
            MemoryArgs::Read { scope, id } => Ok(serde_json::to_value(self.read(scope, &id)?)?),
            MemoryArgs::Delete { scope, id } => self.remove(scope, &id),
            MemoryArgs::Add {
                scope,
                title,
                content,
                source,
            } => {
                let provenance = tool(&source);
                self.save(scope, None, title, content, source, provenance)
            }
            MemoryArgs::Update {
                scope,
                id,
                title,
                content,
                source,
            } => {
                let provenance = tool(&source);
                self.save(scope, Some(id), title, content, source, provenance)
            }
        }
    }

    pub fn remove(&self, scope: Scope, id: &str) -> Result<Value> {
        let _lock = self.lock(scope)?;
        self.read(scope, id)?;
        std::fs::remove_file(self.path(scope, id)?)?;
        Ok(json!({"deleted":id}))
    }

    /// Adds a new entry (`id` = `None`) or replaces one, under the scope lock.
    pub fn save(
        &self,
        scope: Scope,
        id: Option<String>,
        title: String,
        content: String,
        source: String,
        provenance: ProvenanceSource,
    ) -> Result<Value> {
        let _lock = self.lock(scope)?;
        let existing = self.entries(scope)?;
        let previous = match &id {
            Some(id) => Some(self.read(scope, id)?),
            None if existing.len() >= ENTRIES_LIMIT => {
                return Err(Error::Tool("memory scope is full".into()));
            }
            None => None,
        };
        let id = id.unwrap_or_else(new_id);
        let others: usize = existing
            .iter()
            .filter(|entry| entry.id != id)
            .map(|entry| entry.content.len())
            .sum();
        self.write(
            scope, previous, id, title, content, source, provenance, others,
        )
    }

    /// Append-only write used by automatic extraction: content for an existing title
    /// is appended to it (never replacing it); a new title becomes a new entry.
    /// Returns `None` when the content is already present.
    pub fn append(
        &self,
        scope: Scope,
        title: &str,
        content: &str,
        provenance: ProvenanceSource,
    ) -> Result<Option<String>> {
        let _lock = self.lock(scope)?;
        let existing = self.entries(scope)?;
        let others = |id: &str| -> usize {
            existing
                .iter()
                .filter(|entry| entry.id != id)
                .map(|entry| entry.content.len())
                .sum()
        };
        let source = format!("automatic {}", provenance.kind);
        if let Some(entry) = existing
            .iter()
            .find(|entry| entry.title.trim().eq_ignore_ascii_case(title.trim()))
        {
            if entry.content.contains(content.trim()) {
                return Ok(None);
            }
            let merged = format!("{}\n\n{}", entry.content.trim_end(), content.trim());
            let id = entry.id.clone();
            let total = others(&id);
            self.write(
                scope,
                Some(entry.clone()),
                id.clone(),
                entry.title.clone(),
                merged,
                entry.source.clone(),
                provenance,
                total,
            )?;
            return Ok(Some(id));
        }
        if existing.len() >= ENTRIES_LIMIT {
            return Err(Error::Tool("memory scope is full".into()));
        }
        let id = new_id();
        let total = others(&id);
        self.write(
            scope,
            None,
            id.clone(),
            title.into(),
            content.trim().into(),
            source,
            provenance,
            total,
        )?;
        Ok(Some(id))
    }

    #[allow(clippy::too_many_arguments)]
    fn write(
        &self,
        scope: Scope,
        previous: Option<Entry>,
        id: String,
        title: String,
        content: String,
        source: String,
        mut provenance: ProvenanceSource,
        others: usize,
    ) -> Result<Value> {
        if title.trim().is_empty()
            || title.len() > 256
            || content.trim().is_empty()
            || content.len() > ENTRY_LIMIT
            || source.trim().is_empty()
            || source.len() > 1024
        {
            return Err(Error::Tool(
                "memory requires title (<=256), content (<=25 KiB), and source (<=1024)".into(),
            ));
        }
        if others + content.len() > TOTAL_LIMIT {
            return Err(Error::Tool(
                "memory scope would exceed 300 KiB; remove or consolidate entries first".into(),
            ));
        }
        screen(&title)?;
        screen(&content)?;
        let now = crate::session::now();
        provenance.at = now;
        provenance.content_hash = content_hash(&content);
        provenance.evidence = crate::memory::recall::clip(&provenance.evidence, 4096);
        let (created, mut sources) = previous
            .map(|entry| (entry.created, entry.provenance))
            .unwrap_or((now, Vec::new()));
        sources.push(provenance);
        if sources.len() > SOURCES_KEPT {
            sources.drain(..sources.len() - SOURCES_KEPT);
        }
        let entry = Entry {
            version: 1,
            id,
            title,
            content,
            source,
            created: if created == 0 { now } else { created },
            updated: now,
            provenance: sources,
        };
        atomic_json(&self.path(scope, &entry.id)?, &entry)?;
        Ok(json!({"saved":entry.id,"source":entry.source}))
    }
}
