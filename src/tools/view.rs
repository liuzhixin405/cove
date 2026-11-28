//! Where the file tools read and write: the disk, or an
//! overlay stack whose top layer takes the writes. Paths are absolute and already
//! resolved inside the workspace (`LocalTools::resolve`).

use std::{
    path::{Path, PathBuf},
    sync::Arc,
};

use crate::session::overlay::{self, Kind, OverlayStack};

use super::search::Shadow;

#[derive(Clone, Default)]
pub enum FileView {
    /// The main workspace on disk (normal mode).
    #[default]
    Disk,
    /// An overlay sub-task's layers above the main workspace.
    Overlay(Arc<OverlayStack>),
}

impl FileView {
    pub fn overlay(&self) -> Option<&Arc<OverlayStack>> {
        match self {
            Self::Disk => None,
            Self::Overlay(stack) => Some(stack),
        }
    }

    /// `path` relative to the stack's workspace.
    fn relative<'a>(stack: &OverlayStack, path: &'a Path) -> std::io::Result<&'a Path> {
        path.strip_prefix(stack.root())
            .map_err(|_| std::io::Error::other("path is outside the overlay's workspace"))
    }

    pub fn stat(&self, path: &Path) -> std::io::Result<Kind> {
        match self {
            Self::Disk => {
                let meta = std::fs::metadata(path)?;
                Ok(if meta.is_dir() {
                    Kind::Dir
                } else {
                    Kind::File(meta.len())
                })
            }
            Self::Overlay(stack) => stack.stat(Self::relative(stack, path)?),
        }
    }

    pub fn read(&self, path: &Path) -> std::io::Result<Vec<u8>> {
        match self {
            Self::Disk => std::fs::read(path),
            Self::Overlay(stack) => stack.read(Self::relative(stack, path)?),
        }
    }

    /// Entries directly in the directory `path`: (name, is a directory), unordered.
    pub fn list(&self, path: &Path) -> std::io::Result<Vec<(String, bool)>> {
        match self {
            Self::Disk => Ok(std::fs::read_dir(path)?
                .flatten()
                .map(|entry| {
                    (
                        entry.file_name().to_string_lossy().into_owned(),
                        entry.file_type().is_ok_and(|kind| kind.is_dir()),
                    )
                })
                .collect()),
            Self::Overlay(stack) => stack.list(Self::relative(stack, path)?),
        }
    }

    /// Whether the layers changed or deleted `path` (always `false` on disk).
    pub fn shadows(&self, path: &Path) -> bool {
        match self {
            Self::Disk => false,
            Self::Overlay(stack) => {
                Self::relative(stack, path).is_ok_and(|relative| stack.lookup(relative).is_some())
            }
        }
    }

    /// What the layers change for glob, grep and repo_map; `None` on disk.
    pub fn shadow(&self) -> Option<Shadow> {
        let stack = self.overlay()?;
        let hidden = stack
            .shadowed()
            .iter()
            .map(|path| path.to_string_lossy().replace('\\', "/"))
            .collect();
        let files: Vec<(PathBuf, Arc<Vec<u8>>)> = stack
            .files()
            .into_iter()
            .map(|(path, bytes)| (stack.root().join(path), bytes))
            .collect();
        Some(Shadow::new(hidden, files))
    }
}

/// Whether `a` and `b` name the same workspace.
pub fn same_root(a: &Path, b: &Path) -> bool {
    overlay::key(a) == overlay::key(b)
}
