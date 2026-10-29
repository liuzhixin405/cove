//! A child `cove -p` run in a worktree, as used by race and automations.
//!
//! The child gets a private data directory and a minimal config (provider,
//! model, prices, budget share; no profiles, extensions, MCP, hooks or
//! automatic verification), runs with `--permission auto`, and refuses to start
//! further race/automation workers. The API key travels only in the child's
//! environment, never on disk.

use std::{path::Path, time::Duration};

use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio_util::sync::CancellationToken;

use crate::{
    config::Settings,
    domain::{Error, Result},
    process::{Argv, run_argv},
    workflows::changes::{NESTED_ENV, plain, tail},
};

/// Prompts above this go through stdin semantics poorly on Windows command lines.
pub const PROMPT_LIMIT: usize = 24 * 1024;

/// What a worker needs from the parent's settings; owns no secrets on disk.
#[derive(Clone)]
pub struct WorkerConfig {
    pub provider: String,
    pub base_url: String,
    pub model: String,
    pub api_key: String,
    pub prices: Option<(f64, f64)>,
    pub context_window: usize,
    pub max_output_tokens: u32,
    pub instructions: Option<String>,
}

impl WorkerConfig {
    pub fn from_settings(settings: &Settings) -> Self {
        Self {
            provider: settings.provider.clone(),
            base_url: settings.base_url.clone(),
            model: settings.model.clone(),
            api_key: settings.api_key.clone(),
            prices: settings.prices,
            context_window: settings.context_window,
            max_output_tokens: settings.max_output_tokens,
            instructions: settings.instructions.clone(),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RunStatus {
    Completed,
    Failed,
    Timeout,
    Cancelled,
    StartError,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Execution {
    pub status: RunStatus,
    pub exit_code: Option<i32>,
    pub duration_ms: u64,
    pub output: String,
    /// The child's own estimate from its summary line; `None` is unverified.
    pub cost_usd: Option<f64>,
}

pub struct Job<'a> {
    pub worktree: &'a Path,
    pub prompt: &'a str,
    pub budget: Option<f64>,
    pub max_turns: u32,
    pub timeout: Duration,
}

/// Reads the estimated cost from the child's `[summary]` line.
fn reported_cost(text: &str) -> Option<f64> {
    let line = text.lines().rev().find(|line| line.contains("[summary]"))?;
    // The summary line names it "预估费用 $x" (formerly "estimated $x").
    let (at, marker) = ["预估费用 $", "estimated $"]
        .iter()
        .find_map(|marker| line.find(marker).map(|at| (at, *marker)))?;
    let start = at + marker.len();
    let number: String = line[start..]
        .chars()
        .take_while(|c| c.is_ascii_digit() || *c == '.')
        .collect();
    number.parse().ok()
}

pub async fn run(config: &WorkerConfig, job: Job<'_>, cancel: &CancellationToken) -> Execution {
    let started = std::time::Instant::now();
    let fail = |status, output: String| Execution {
        status,
        exit_code: None,
        duration_ms: started.elapsed().as_millis() as u64,
        output,
        cost_usd: None,
    };
    if job.prompt.trim().is_empty() || job.prompt.len() > PROMPT_LIMIT {
        return fail(
            RunStatus::StartError,
            "prompt must be 1..24576 bytes".into(),
        );
    }
    let private = match tempfile::Builder::new().prefix("cove-worker-").tempdir() {
        Ok(dir) => dir,
        Err(err) => return fail(RunStatus::StartError, err.to_string()),
    };
    match spawn(config, &job, private.path(), cancel).await {
        Ok(mut execution) => {
            execution.duration_ms = started.elapsed().as_millis() as u64;
            execution
        }
        Err(Error::Cancelled) => fail(RunStatus::Cancelled, "cancelled".into()),
        Err(err) => fail(RunStatus::StartError, err.to_string()),
    }
}

async fn spawn(
    config: &WorkerConfig,
    job: &Job<'_>,
    private: &Path,
    cancel: &CancellationToken,
) -> Result<Execution> {
    let mut file = json!({
        "provider": {"name": config.provider, "base_url": config.base_url},
        "model": config.model,
        "permission_mode": "auto",
        "max_iterations": job.max_turns,
        "context_window": config.context_window,
        "max_output_tokens": config.max_output_tokens,
        "done_verify_auto": false,
    });
    if let Some((input, output)) = config.prices {
        file["input_price"] = json!(input);
        file["output_price"] = json!(output);
        if let Some(budget) = job.budget {
            file["max_budget_usd"] = json!(budget);
        }
    }
    if let Some(instructions) = &config.instructions {
        file["system_prompt"] = json!(instructions);
    }
    let config_path = private.join("config.json");
    std::fs::write(&config_path, serde_json::to_vec_pretty(&file)?)?;
    let data = private.join("data");
    std::fs::create_dir_all(&data)?;
    let seconds = job.timeout.as_secs().max(1);
    let mut argv = Argv::new(plain(job.worktree), std::env::current_exe()?)
        .args(["-p", job.prompt, "--permission", "auto"])
        .args(["--max-turns".to_owned(), job.max_turns.to_string()])
        .args(["--timeout".to_owned(), seconds.to_string()])
        .arg_path("--cwd", &plain(job.worktree))
        .arg_path("--data-dir", &plain(&data))
        .arg_path("--config", &plain(&config_path))
        .env("COVE_API_KEY", &config.api_key)
        .env(NESTED_ENV, "1")
        // The child's own deadline plus room for its cleanup.
        .timeout(job.timeout + Duration::from_secs(20))
        .limit(256 * 1024);
    argv.env_remove = [
        "COVE_CONFIG",
        "COVE_PROFILE",
        "COVE_PROVIDER",
        "COVE_MODEL",
        "COVE_BASE_URL",
        "COVE_DATA_DIR",
    ]
    .into_iter()
    .map(Into::into)
    .collect();
    let output = run_argv(argv, cancel).await?;
    let text = format!("{}{}", output.stdout_text(), output.stderr_text());
    let status = if output.timed_out {
        RunStatus::Timeout
    } else if output.code == Some(0) {
        RunStatus::Completed
    } else {
        RunStatus::Failed
    };
    Ok(Execution {
        status,
        exit_code: output.code,
        duration_ms: 0,
        cost_usd: reported_cost(&text),
        output: tail(&text, 16 * 1024),
    })
}

trait ArgPath {
    fn arg_path(self, flag: &str, path: &Path) -> Self;
}

impl ArgPath for Argv {
    fn arg_path(mut self, flag: &str, path: &Path) -> Self {
        self.args.push(flag.into());
        self.args.push(path.as_os_str().to_owned());
        self
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cost_comes_only_from_the_summary_line() {
        let text = "noise estimated $9\n[summary] calls=1 tools=0 failed=0 input_tokens=1 output_tokens=1 estimated $0.012500; no automatic verification\n";
        assert_eq!(reported_cost(text), Some(0.0125));
        assert_eq!(reported_cost("[summary] cost unknown"), None);
        // The summary line as the terminal front end now writes it.
        let chinese = "[summary] calls=1 tools=0 预估费用 $0.250000；未自动验证
";
        assert_eq!(reported_cost(chinese), Some(0.25));
    }
}
