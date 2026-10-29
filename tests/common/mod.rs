//! Launching the binary under test.
#![allow(dead_code)]

use std::{
    path::Path,
    process::{Output, Stdio},
};

pub fn exe() -> &'static Path {
    Path::new(env!("CARGO_BIN_EXE_cove"))
}

/// `output()` with stdin closed. tokio's `output()` inherits stdin, so every
/// launch saw whatever the test runner's stdin was (an idle pipe under an IDE
/// or a background shell) and `-p` waited on it; tests must not depend on that.
pub trait Launch {
    async fn launch(&mut self) -> std::io::Result<Output>;
}

impl Launch for tokio::process::Command {
    async fn launch(&mut self) -> std::io::Result<Output> {
        self.stdin(Stdio::null()).output().await
    }
}
