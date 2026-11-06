//! Every integration test in one binary: one link of the cove library instead of
//! one per test file, which dominated `cargo test` time on slow disks.

mod common;

mod agents;
mod cli;
// Not `mod core`: that would shadow the `core` crate at this root.
#[path = "core.rs"]
mod core_tests;
mod evaluation;
mod extensions;
mod integrations;
mod model;
mod persistence;
mod project;
mod safety;
mod sessions;
mod tools;
mod workflows;
