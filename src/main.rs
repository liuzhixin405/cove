use std::io::IsTerminal;

use clap::Parser;
use cove::{
    app::launch::{self, EXIT_FAILURE, EXIT_INTERRUPTED, EXIT_USAGE, Piped},
    app::setup,
    config::{Cli, NeedsSetup, Settings, data_dir},
};

/// Exit codes: 0 ok, 1 failure, 2 bad arguments, 130 interrupted by Ctrl+C.
fn main() {
    let runtime = match tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
    {
        Ok(runtime) => runtime,
        Err(err) => {
            eprintln!("错误：无法启动异步运行时：{err}；请重试，或运行 cove --doctor 检查环境");
            std::process::exit(1);
        }
    };
    let code = runtime.block_on(run());
    use std::io::Write;
    let _ = std::io::stdout().flush();
    let _ = std::io::stderr().flush();
    // Exit now: dropping the runtime would wait for blocking tasks that may never
    // finish (a console read waiting for input, a hung child pipe).
    std::process::exit(i32::from(code));
}

async fn run() -> u8 {
    if std::env::args_os()
        .nth(1)
        .is_some_and(|arg| arg == "--internal-stdio-host")
    {
        return match cove::process::stdio_host() {
            Ok(0) => 0,
            // Argv callers (verifiers) record the real exit code.
            Ok(code) => u8::try_from(code).unwrap_or(1).max(1),
            Err(err) => {
                eprintln!("{}", cove::app::ui::visible(&err.to_string()));
                EXIT_FAILURE
            }
        };
    }
    if std::env::args_os()
        .nth(1)
        .is_some_and(|arg| arg == "--automation")
    {
        let args: Vec<String> = std::env::args().skip(2).collect();
        let code = cove::workflows::automation::cli(args).await;
        return u8::try_from(code).unwrap_or(1);
    }
    // Started by double-click: no arguments and a console window of its own.
    let double_clicked = std::env::args_os().len() == 1 && setup::own_console();
    match start(double_clicked).await {
        Ok(code) => code,
        Err(error) => {
            eprintln!(
                "\n错误：{}",
                cove::app::ui::visible(&format!("{error:#}"))
            );
            if double_clicked {
                setup::pause();
            }
            let interrupted = error
                .downcast_ref::<cove::domain::Error>()
                .is_some_and(|err| matches!(err, cove::domain::Error::Cancelled));
            if interrupted {
                EXIT_INTERRUPTED
            } else {
                EXIT_FAILURE
            }
        }
    }
}

fn usage_error(message: &str) -> anyhow::Result<u8> {
    eprintln!("错误：{}", cove::app::ui::visible(message));
    Ok(EXIT_USAGE)
}

async fn start(double_clicked: bool) -> anyhow::Result<u8> {
    let mut cli = Cli::parse();
    if cli.doctor {
        return Ok(if cove::app::diagnostics::cli_doctor(cli).await? {
            0
        } else {
            EXIT_FAILURE
        });
    }
    if let Some(scope) = &cli.list_sessions {
        anyhow::ensure!(
            matches!(scope.as_str(), "project" | "all"),
            "--list-sessions 只接受 all 或不带值：cove --list-sessions 列出本项目的会话，cove --list-sessions all 列出全部"
        );
        let text = cove::session::list_sessions(&data_dir(&cli)?, &cli.cwd, scope == "all")?;
        print!("{}", cove::app::ui::visible(&text));
        return Ok(0);
    }
    if double_clicked {
        cli.cwd = setup::ask_workspace(&std::env::current_dir()?)?;
    }
    if let Some(prompt) = cli.print.take() {
        let mut prompt = prompt;
        if !std::io::stdin().is_terminal() {
            let wait =
                launch::first_byte_wait(launch::stdin_is_file(), std::io::stderr().is_terminal());
            match launch::read_piped(std::io::stdin(), wait, launch::STDIN_LIMIT)? {
                Piped::Text { text, truncated } => {
                    if truncated {
                        eprintln!("警告：标准输入超过 8 MiB，只发送前 8 MiB");
                    }
                    prompt = launch::combine(&prompt, &text);
                }
                Piped::NoData => eprintln!(
                    "警告：{} 秒内标准输入没有数据，已忽略管道输入",
                    launch::STDIN_FIRST_BYTE.as_secs()
                ),
            }
        }
        if prompt.trim().is_empty() {
            return usage_error(
                "-p 需要提示词：cove -p \"提示词\"，或通过管道传入：cat app.log | cove -p \"解释一下\"",
            );
        }
        // Images travel as `@"path"` attachment tokens, checked and resized like `/attach`.
        for image in &cli.images {
            let path = if image.is_absolute() {
                image.clone()
            } else {
                cli.cwd.join(image)
            };
            if let Err(err) = cove::model::images::attachment(&path) {
                return usage_error(&format!("--image {}: {err}", image.display()));
            }
            prompt = format!("{prompt} @\"{}\"", path.display());
        }
        prompt = match launch::attach_files(&prompt, &cli.files) {
            Ok(prompt) => prompt,
            Err(err) => return usage_error(&format!("{err:#}")),
        };
        cli.print = Some(prompt);
    }
    let interactive = cli.print.is_none()
        && cli.prompt.is_none()
        && !cli.continue_task
        // COVE_NO_SETUP=1 (scripts, CI) reports missing settings instead of asking.
        && std::env::var("COVE_NO_SETUP").map_or(true, |value| value != "1")
        && std::io::stdin().is_terminal()
        && std::io::stderr().is_terminal();
    let settings = match Settings::load(cli.clone()) {
        Err(err) if interactive && err.downcast_ref::<NeedsSetup>().is_some() => {
            let path = match &cli.config {
                Some(path) => path.clone(),
                None => data_dir(&cli)?.join("config.json"),
            };
            setup::run(&path, &err.to_string()).await?;
            Settings::load(cli.clone())?
        }
        Err(err) => {
            if let Ok(dir) = data_dir(&cli) {
                cove::app::diagnostics::Diagnostics::new(&dir)
                    .error("startup", &format!("{err:#}"));
            }
            return Err(err);
        }
        Ok(settings) => settings,
    };
    for warning in &settings.warnings {
        eprintln!("警告：{}", cove::app::ui::visible(warning));
    }
    cove::app::run(settings).await?;
    match launch::take_restart() {
        Some(args) => launch::restart(&args),
        None => Ok(0),
    }
}
