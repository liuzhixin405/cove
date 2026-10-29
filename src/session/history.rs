//! The conversation of a session. Reading is a plain slice; changing it is
//! either an append or an explicit rewrite, so the session journal can tell the
//! two apart: appends are journaled as they are, a rewrite (compaction, masking,
//! a replaced history) starts a new epoch and the next save writes a snapshot.

use std::{
    ops::Deref,
    sync::atomic::{AtomicU64, Ordering},
};

use serde::{Deserialize, Deserializer, Serialize, Serializer};

use crate::domain::Message;

static EPOCHS: AtomicU64 = AtomicU64::new(1);

fn next_epoch() -> u64 {
    EPOCHS.fetch_add(1, Ordering::Relaxed)
}

/// Messages oldest first. There is deliberately no `DerefMut`: in-place changes
/// go through [`rewrite`](Self::rewrite), which the compiler makes the only way.
#[derive(Debug)]
pub struct History {
    messages: Vec<Message>,
    /// Changes whenever the messages stop being an extension of what an earlier
    /// save saw; never serialized.
    epoch: u64,
}

impl History {
    pub fn new() -> Self {
        Self::from(Vec::new())
    }

    pub fn push(&mut self, message: Message) {
        self.messages.push(message);
    }

    pub fn extend(&mut self, messages: impl IntoIterator<Item = Message>) {
        self.messages.extend(messages);
    }

    /// Mutable access for changes other than appending; starts a new epoch.
    pub fn rewrite(&mut self) -> &mut Vec<Message> {
        self.epoch = next_epoch();
        &mut self.messages
    }

    /// Identifies this sequence of appends; see the module documentation.
    pub fn epoch(&self) -> u64 {
        self.epoch
    }

    pub fn into_vec(self) -> Vec<Message> {
        self.messages
    }
}

impl Default for History {
    fn default() -> Self {
        Self::new()
    }
}

/// A copy can grow apart from the original, so it never shares the epoch.
impl Clone for History {
    fn clone(&self) -> Self {
        Self::from(self.messages.clone())
    }
}

impl From<Vec<Message>> for History {
    fn from(messages: Vec<Message>) -> Self {
        Self {
            messages,
            epoch: next_epoch(),
        }
    }
}

impl FromIterator<Message> for History {
    fn from_iter<I: IntoIterator<Item = Message>>(iter: I) -> Self {
        Self::from(iter.into_iter().collect::<Vec<_>>())
    }
}

impl Deref for History {
    type Target = [Message];

    fn deref(&self) -> &[Message] {
        &self.messages
    }
}

impl<'a> IntoIterator for &'a History {
    type Item = &'a Message;
    type IntoIter = std::slice::Iter<'a, Message>;

    fn into_iter(self) -> Self::IntoIter {
        self.messages.iter()
    }
}

impl Serialize for History {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        self.messages.serialize(serializer)
    }
}

impl<'de> Deserialize<'de> for History {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        Vec::<Message>::deserialize(deserializer).map(Self::from)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn appends_keep_the_epoch_and_everything_else_changes_it() {
        let mut history = History::new();
        let epoch = history.epoch();
        history.push(Message::user("a"));
        history.extend([Message::user("b")]);
        assert_eq!(history.epoch(), epoch);
        assert_eq!(history.len(), 2);
        let copy = history.clone();
        assert_ne!(copy.epoch(), epoch);
        history.rewrite().remove(0);
        assert_ne!(history.epoch(), epoch);
        let json = serde_json::to_string(&history).unwrap();
        let back: History = serde_json::from_str(&json).unwrap();
        assert_eq!(back.len(), 1);
        assert_ne!(back.epoch(), history.epoch());
    }
}
