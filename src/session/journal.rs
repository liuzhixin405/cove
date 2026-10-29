//! Session persistence as a snapshot plus an append-only journal.
//!
//! `sessions/<id>.json` is a complete session (the format other readers know)
//! with the `journal_seq` it includes; `journals/<id>.log` holds one JSON record
//! per later save: the messages appended since the previous record and every
//! other field. A save appends a record (no new file, no rename: on Windows,
//! with a scanner hooked into file creation, that is ~10 µs instead of 8-20 ms)
//! and is flushed to disk before returning, like the old full rewrite. A
//! rewritten history (another [`History`] epoch), a long journal or a flush
//! writes a new snapshot and starts the journal over.
//!
//! Replaying is idempotent: a record says where its messages start and carries
//! a sequence number; records the snapshot already includes are skipped, and a
//! torn last record (a crash while writing it) ends the replay.

use std::{
    collections::HashMap,
    fs::File,
    io::{Read, Write},
    path::{Path, PathBuf},
    sync::{
        LazyLock, Mutex,
        atomic::{AtomicU64, Ordering},
    },
};

use serde::{Deserialize, Serialize};

use crate::{
    domain::{Error, Message, Result},
    extensions::hooks::HookRun,
    session::{
        Accounting, PendingCall, STATE_LIMIT, Session, State, atomic_json, history::History,
        io_context, now,
    },
};

/// A journal past this size is folded into a new snapshot.
const JOURNAL_LIMIT: u64 = 8 * 1024 * 1024;
/// Most journal bytes read back.
const JOURNAL_READ_LIMIT: u64 = 4 * STATE_LIMIT;

/// Every field of a session but its messages, borrowed.
#[derive(Serialize)]
struct Head<'a> {
    hook_runs: &'a [HookRun],
    version: u32,
    id: &'a str,
    cwd: &'a Path,
    protocol: &'a str,
    model: &'a str,
    created: u64,
    updated: u64,
    current_request: &'a str,
    state: &'a State,
    pending: &'a [PendingCall],
    accounting: &'a Accounting,
    prices: Option<(f64, f64)>,
    checkpoints: &'a [String],
    current_checkpoint: Option<&'a str>,
    compactions: u32,
    task_failures: u32,
    title: Option<&'a str>,
}

impl<'a> Head<'a> {
    /// The destructuring names every field: a field added to `Session` and not
    /// journaled here does not compile.
    fn of(session: &'a Session, updated: u64) -> Self {
        let Session {
            hook_runs,
            version,
            id,
            cwd,
            protocol,
            model,
            created,
            updated: _,
            messages: _,
            current_request,
            state,
            pending,
            accounting,
            prices,
            checkpoints,
            current_checkpoint,
            compactions,
            task_failures,
            title,
        } = session;
        Self {
            hook_runs,
            version: *version,
            id,
            cwd,
            protocol,
            model,
            created: *created,
            updated,
            current_request,
            state,
            pending,
            accounting,
            prices: *prices,
            checkpoints,
            current_checkpoint: current_checkpoint.as_deref(),
            compactions: *compactions,
            task_failures: *task_failures,
            title: title.as_deref(),
        }
    }
}

#[derive(Serialize)]
struct SnapshotRef<'a> {
    #[serde(flatten)]
    head: Head<'a>,
    messages: &'a [Message],
    journal_seq: u64,
}

#[derive(Deserialize)]
struct SnapshotSeq {
    #[serde(default)]
    journal_seq: u64,
}

#[derive(Serialize)]
struct RecordRef<'a> {
    seq: u64,
    /// Index of the first appended message.
    at: usize,
    append: &'a [Message],
    head: Head<'a>,
}

#[derive(Deserialize)]
struct Record {
    seq: u64,
    at: usize,
    append: Vec<Message>,
    /// A session without messages.
    head: Session,
}

/// The open journal of a session this process saves.
struct Journal {
    file: File,
    epoch: u64,
    /// Messages the snapshot and the journal hold.
    saved: usize,
    seq: u64,
    bytes: u64,
}

/// Open journals by path; process-wide because every `Store` value of a data
/// directory writes the same files.
static JOURNALS: LazyLock<Mutex<HashMap<PathBuf, Journal>>> =
    LazyLock::new(|| Mutex::new(HashMap::new()));

static LAST_SEQ: AtomicU64 = AtomicU64::new(0);

/// Increasing within the process and, being microseconds since the epoch, above
/// whatever an earlier process wrote.
fn next_seq() -> u64 {
    let clock = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |time| time.as_micros() as u64);
    let mut last = LAST_SEQ.load(Ordering::Relaxed);
    loop {
        let next = clock.max(last + 1);
        match LAST_SEQ.compare_exchange(last, next, Ordering::Relaxed, Ordering::Relaxed) {
            Ok(_) => return next,
            Err(current) => last = current,
        }
    }
}

fn journals() -> std::sync::MutexGuard<'static, HashMap<PathBuf, Journal>> {
    JOURNALS.lock().unwrap_or_else(|poison| poison.into_inner())
}

/// Saves `session`: a journal record when the history only grew since the last
/// save of this process, else a snapshot.
pub fn save(snapshot: &Path, log: &Path, session: &Session) -> Result<()> {
    let updated = now();
    let mut journals = journals();
    if let Some(journal) = journals.get_mut(log)
        && journal.epoch == session.messages.epoch()
        && journal.saved <= session.messages.len()
        && journal.bytes < JOURNAL_LIMIT
    {
        let seq = next_seq();
        let record = RecordRef {
            seq,
            at: journal.saved,
            append: &session.messages[journal.saved..],
            head: Head::of(session, updated),
        };
        let mut line = serde_json::to_vec(&record)?;
        line.push(b'\n');
        if line.len() as u64 <= STATE_LIMIT
            && journal.file.write_all(&line).is_ok()
            && journal.file.sync_data().is_ok()
        {
            journal.saved = session.messages.len();
            journal.seq = seq;
            journal.bytes += line.len() as u64;
            return Ok(());
        }
        // A record that failed half-written would hide every later one: start over.
    }
    write_snapshot(&mut journals, snapshot, log, session, updated)
}

/// Writes a snapshot and starts the journal over (task ends, exits).
pub fn flush(snapshot: &Path, log: &Path, session: &Session) -> Result<()> {
    write_snapshot(&mut journals(), snapshot, log, session, now())
}

fn write_snapshot(
    journals: &mut HashMap<PathBuf, Journal>,
    snapshot: &Path,
    log: &Path,
    session: &Session,
    updated: u64,
) -> Result<()> {
    // The open handle goes first: Windows cannot delete a file still open.
    journals.remove(log);
    let seq = next_seq();
    atomic_json(
        snapshot,
        &SnapshotRef {
            head: Head::of(session, updated),
            messages: &session.messages,
            journal_seq: seq,
        },
    )?;
    // The snapshot holds every record now; a crash before this removal only
    // leaves records that replay skips.
    match std::fs::remove_file(log) {
        Ok(()) => {}
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => {}
        Err(err) => return Err(io_context("remove journal", log, err)),
    }
    let file = std::fs::OpenOptions::new()
        .create(true)
        .append(true)
        .open(log)
        .map_err(|err| io_context("open journal", log, err))?;
    journals.insert(
        log.to_owned(),
        Journal {
            file,
            epoch: session.messages.epoch(),
            saved: session.messages.len(),
            seq,
            bytes: 0,
        },
    );
    Ok(())
}

/// Reads a session: the snapshot, then the journal records it does not include.
pub fn load(snapshot: &Path, log: &Path) -> Result<Session> {
    let data = read_limited(snapshot, STATE_LIMIT)?;
    let mut session: Session = serde_json::from_slice(&data)?;
    let mut seq = serde_json::from_slice::<SnapshotSeq>(&data)?.journal_seq;
    let journal = match read_limited(log, JOURNAL_READ_LIMIT) {
        Ok(journal) => journal,
        Err(Error::Io(err)) if err.kind() == std::io::ErrorKind::NotFound => return Ok(session),
        Err(err) => return Err(err),
    };
    for line in journal.split(|byte| *byte == b'\n') {
        if line.is_empty() {
            continue;
        }
        // A torn record ends the journal: nothing after it was confirmed.
        let Ok(record) = serde_json::from_slice::<Record>(line) else {
            break;
        };
        if record.seq <= seq {
            continue;
        }
        if record.at > session.messages.len() {
            break;
        }
        let mut messages = std::mem::take(&mut session.messages).into_vec();
        messages.truncate(record.at);
        messages.extend(record.append);
        session = record.head;
        session.messages = History::from(messages);
        seq = record.seq;
    }
    Ok(session)
}

fn read_limited(path: &Path, limit: u64) -> Result<Vec<u8>> {
    let mut data = Vec::new();
    File::open(path)
        .map_err(|err| io_context("open", path, err))?
        .take(limit + 1)
        .read_to_end(&mut data)
        .map_err(|err| io_context("read", path, err))?;
    if data.len() as u64 > limit {
        return Err(Error::Session(format!(
            "{} exceeds {} MiB",
            path.display(),
            limit / (1024 * 1024)
        )));
    }
    Ok(data)
}

/// Forgets the open journal of a session whose files are removed or moved.
pub fn forget(log: &Path) {
    journals().remove(log);
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::Message;

    fn paths(dir: &Path) -> (PathBuf, PathBuf) {
        (dir.join("s.json"), dir.join("s.log"))
    }

    fn session() -> Session {
        Session::new("/p".into(), "p".into(), "m".into(), None)
    }

    #[test]
    fn appends_are_journaled_and_replayed() {
        let dir = tempfile::tempdir().unwrap();
        let (snapshot, log) = paths(dir.path());
        let mut session = session();
        save(&snapshot, &log, &session).unwrap();
        assert_eq!(std::fs::metadata(&log).unwrap().len(), 0);
        session.messages.push(Message::user("one"));
        session.current_request = "one".into();
        save(&snapshot, &log, &session).unwrap();
        session.messages.push(Message::user("two"));
        session.compactions = 3;
        save(&snapshot, &log, &session).unwrap();
        assert_eq!(std::fs::read_to_string(&log).unwrap().lines().count(), 2);
        let loaded = load(&snapshot, &log).unwrap();
        assert_eq!(loaded.messages.len(), 2);
        assert_eq!(loaded.current_request, "one");
        assert_eq!(loaded.compactions, 3);
        // The snapshot alone is still the session as it was first saved.
        let plain: Session = serde_json::from_slice(&std::fs::read(&snapshot).unwrap()).unwrap();
        assert!(plain.messages.is_empty());
    }

    #[test]
    fn rewrites_and_flushes_write_snapshots_and_torn_records_are_ignored() {
        let dir = tempfile::tempdir().unwrap();
        let (snapshot, log) = paths(dir.path());
        let mut session = session();
        session.messages.push(Message::user("a"));
        session.messages.push(Message::user("b"));
        save(&snapshot, &log, &session).unwrap();
        session.messages.rewrite().remove(0);
        save(&snapshot, &log, &session).unwrap();
        assert_eq!(std::fs::metadata(&log).unwrap().len(), 0);
        session.messages.push(Message::user("c"));
        save(&snapshot, &log, &session).unwrap();
        // A crash in the middle of the next record.
        std::fs::OpenOptions::new()
            .append(true)
            .open(&log)
            .unwrap()
            .write_all(b"{\"seq\":99999999999999999,\"at\":0,\"app")
            .unwrap();
        let loaded = load(&snapshot, &log).unwrap();
        let texts: Vec<String> = loaded
            .messages
            .iter()
            .map(|message| serde_json::to_string(message).unwrap())
            .collect();
        assert_eq!(texts.len(), 2);
        assert!(texts[0].contains("\"b\"") && texts[1].contains("\"c\""));
        flush(&snapshot, &log, &session).unwrap();
        assert_eq!(std::fs::metadata(&log).unwrap().len(), 0);
        let plain: Session = serde_json::from_slice(&std::fs::read(&snapshot).unwrap()).unwrap();
        assert_eq!(plain.messages.len(), 2);
    }

    #[test]
    fn records_the_snapshot_includes_are_skipped() {
        let dir = tempfile::tempdir().unwrap();
        let (snapshot, log) = paths(dir.path());
        let mut session = session();
        save(&snapshot, &log, &session).unwrap();
        session.messages.push(Message::user("a"));
        save(&snapshot, &log, &session).unwrap();
        let stale = std::fs::read(&log).unwrap();
        // A crash after a newer snapshot was written but before the journal was cleared.
        session.title = Some("newer".into());
        flush(&snapshot, &log, &session).unwrap();
        forget(&log);
        std::fs::write(&log, stale).unwrap();
        let loaded = load(&snapshot, &log).unwrap();
        assert_eq!(loaded.title.as_deref(), Some("newer"));
        assert_eq!(loaded.messages.len(), 1);
    }
}
