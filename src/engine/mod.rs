//! Execution kernel: the agent loop, context, turn hooks, prompt assembly and tool-output management.

pub mod agent;
pub mod context;
pub mod prompt;
pub mod subdir_hints;
pub mod tool_output;
pub mod turn;
