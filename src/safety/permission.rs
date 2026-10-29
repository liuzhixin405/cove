//! Permission modes and the central authorizer.
//!
//! Order of a decision: catastrophic shell commands are blocked in every
//! mode → deny rules → plan mode (only read-only effects) → bypass → allow/ask rules by
//! priority → the mode's default. A headless "ask" is a denial with guidance.

pub mod classify;
pub mod policy;
pub mod safety;
pub mod shell;

use std::{
    path::{Path, PathBuf},
    sync::{Arc, RwLock},
};

use futures_util::future::BoxFuture;
use tokio_util::sync::CancellationToken;

use crate::domain::{Error, Result, UserInput};
use policy::{PolicyRule, RuleDecision};
use shell::Dialect;

#[derive(Clone, Copy, Debug, PartialEq, Eq, clap::ValueEnum)]
pub enum Mode {
    /// Asks for writes, network and shell lines that are not read-only.
    #[value(alias = "ask")]
    Default,
    /// Read-only: file reading/search and read-only shell lines; everything else denied.
    #[value(alias = "readonly")]
    Plan,
    /// Also allows build/test shell lines, network tools and edits inside the project.
    Auto,
    /// Allows everything except deny rules and catastrophic commands.
    Bypass,
}

#[allow(non_upper_case_globals)]
impl Mode {
    /// Former names, kept for code written against the first permission modes.
    pub const Ask: Mode = Mode::Default;
    pub const Readonly: Mode = Mode::Plan;

    pub fn name(self) -> &'static str {
        match self {
            Self::Default => "default",
            Self::Plan => "plan",
            Self::Auto => "auto",
            Self::Bypass => "bypass",
        }
    }

    /// Current names plus older aliases.
    pub fn parse(raw: &str) -> Option<Self> {
        Some(match raw.trim().to_ascii_lowercase().as_str() {
            "default" | "ask" => Self::Default,
            "plan" | "readonly" => Self::Plan,
            "auto" => Self::Auto,
            "bypass" => Self::Bypass,
            _ => return None,
        })
    }
}

#[derive(Clone, Debug)]
pub enum Effect {
    Read,
    Write(PathBuf),
    Shell(String),
    /// Outbound request whose URL or query leaves this machine.
    Network(String),
    /// A user-configured extension process (MCP server, hook); also needs --trust-extensions.
    Process(String),
}

impl Effect {
    pub fn describe(&self) -> String {
        match self {
            Effect::Write(path) => format!("写入 {}", path.display()),
            Effect::Shell(command) => format!("运行 shell 命令（无沙箱）：{command}"),
            Effect::Network(request) => format!("访问网络：{request}"),
            Effect::Process(process) => format!("运行扩展进程（无沙箱）：{process}"),
            Effect::Read => "读取".into(),
        }
    }
}

pub trait Authorizer: Send + Sync {
    fn authorize(&self, effect: Effect, cancel: CancellationToken) -> BoxFuture<'_, Result<()>>;
}

#[derive(Debug, PartialEq, Eq)]
pub enum Decision {
    Allow,
    Ask,
    Deny(String),
}

fn inside(roots: &[PathBuf], path: &Path) -> bool {
    roots.iter().any(|root| path.starts_with(root))
}

/// The mode's own decision, before rules. `roots == None` means every write is already
/// confined by the caller (file tools resolve paths inside the workspace).
pub fn mode_default(
    mode: Mode,
    effect: &Effect,
    roots: Option<&[PathBuf]>,
    dialect: Dialect,
) -> Decision {
    match (mode, effect) {
        (_, Effect::Read) | (Mode::Bypass, _) => Decision::Allow,
        (Mode::Plan, Effect::Shell(command)) if classify::read_only(command, dialect) => {
            Decision::Allow
        }
        (Mode::Plan, _) => Decision::Deny("plan mode: only read operations are allowed".into()),
        (Mode::Default, Effect::Shell(command)) if classify::read_only(command, dialect) => {
            Decision::Allow
        }
        (Mode::Auto, Effect::Shell(command)) if classify::auto_approvable(command, dialect) => {
            Decision::Allow
        }
        (Mode::Auto, Effect::Write(path)) if roots.is_none_or(|roots| inside(roots, path)) => {
            Decision::Allow
        }
        (Mode::Auto, Effect::Network(_) | Effect::Process(_)) => Decision::Allow,
        _ => Decision::Ask,
    }
}

/// Who put the session in plan mode; the model may only leave a plan mode it entered.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PlanOrigin {
    User,
    Model,
    /// The review/once workflow; leaving it needs the user's approval of a plan.
    Workflow,
}

#[derive(Clone, Copy, Debug)]
struct ModeState {
    mode: Mode,
    plan: Option<(PlanOrigin, Mode)>,
}

/// Mode, rules and prompting shared by every tool; `/mode` changes it live.
pub struct Permissions {
    state: RwLock<ModeState>,
    rules: RwLock<Vec<PolicyRule>>,
    store: Option<PathBuf>,
    project: PathBuf,
    roots: Vec<PathBuf>,
    input: Option<Arc<dyn UserInput>>,
    dialect: Dialect,
}

impl Permissions {
    /// `project` scopes remembered rules; `roots` are where `auto` allows writes.
    pub fn new(
        mode: Mode,
        project: PathBuf,
        roots: Vec<PathBuf>,
        input: Option<Arc<dyn UserInput>>,
    ) -> Self {
        Self {
            state: RwLock::new(ModeState {
                mode,
                plan: (mode == Mode::Plan).then_some((PlanOrigin::User, Mode::Default)),
            }),
            rules: RwLock::new(Vec::new()),
            store: None,
            project,
            roots,
            input,
            dialect: Dialect::host(),
        }
    }

    /// Loads `policies.json`; "remember for this project" appends to it.
    pub fn with_policies(mut self, path: PathBuf) -> Result<Self> {
        *self.rules.write().map_err(|_| poisoned())? = policy::load(&path)?;
        self.store = Some(path);
        Ok(self)
    }

    pub fn with_rules(self, rules: Vec<PolicyRule>) -> Self {
        if let Ok(mut current) = self.rules.write() {
            *current = rules;
        }
        self
    }

    pub fn rules(&self) -> Vec<PolicyRule> {
        self.rules
            .read()
            .map(|rules| rules.clone())
            .unwrap_or_default()
    }

    pub fn policy_path(&self) -> Option<&Path> {
        self.store.as_deref()
    }

    pub fn mode(&self) -> Mode {
        self.state.read().map_or(Mode::Plan, |state| state.mode)
    }

    pub fn plan_origin(&self) -> Option<PlanOrigin> {
        self.state
            .read()
            .ok()
            .and_then(|state| state.plan.map(|(origin, _)| origin))
    }

    pub fn interactive(&self) -> bool {
        self.input.is_some()
    }

    /// The user's choice; replaces any plan mode the model or workflow entered.
    pub fn set_mode(&self, mode: Mode) {
        if let Ok(mut state) = self.state.write() {
            let previous = match state.plan {
                Some((_, previous)) => previous,
                None => state.mode,
            };
            state.plan = (mode == Mode::Plan).then_some((
                PlanOrigin::User,
                if previous == Mode::Plan {
                    Mode::Default
                } else {
                    previous
                },
            ));
            state.mode = mode;
        }
    }

    /// Enters plan mode; returns false when already in it.
    pub fn enter_plan(&self, origin: PlanOrigin) -> bool {
        let Ok(mut state) = self.state.write() else {
            return false;
        };
        if state.mode == Mode::Plan {
            return false;
        }
        state.plan = Some((origin, state.mode));
        state.mode = Mode::Plan;
        true
    }

    /// Leaves a plan mode entered by `origin` (a user plan mode only by the user).
    pub fn exit_plan(&self, origin: PlanOrigin) -> std::result::Result<Mode, String> {
        let mut state = self
            .state
            .write()
            .map_err(|_| "permission state poisoned".to_owned())?;
        match state.plan {
            Some((entered, previous))
                if entered == origin
                    || origin == PlanOrigin::User
                    || (entered == PlanOrigin::Model && origin == PlanOrigin::Workflow) =>
            {
                state.mode = previous;
                state.plan = None;
                Ok(previous)
            }
            Some((PlanOrigin::User, _)) => Err(
                "the user selected plan mode; only the user can leave it (/mode default)".into(),
            ),
            Some(_) => Err(
                "plan mode is held by the review workflow; submit the plan with exit_plan_mode"
                    .into(),
            ),
            None => Err("not in plan mode".into()),
        }
    }

    pub fn decide(&self, effect: &Effect) -> Decision {
        let mode = self.mode();
        let rules = self
            .rules
            .read()
            .map(|rules| rules.clone())
            .unwrap_or_default();
        decide(
            mode,
            effect,
            &rules,
            &self.project,
            Some(&self.roots),
            self.dialect,
        )
    }

    async fn ask(&self, effect: Effect, cancel: CancellationToken) -> Result<()> {
        let description = effect.describe();
        // The heading, what would happen and the warnings are shown above the
        // `是否允许？` line the answer is typed on.
        let mut head = "需要批准".to_owned();
        let mut warnings = String::new();
        if let Effect::Shell(command) = &effect {
            let category = classify::classify(command, self.dialect);
            head.push_str(&format!(" · 命令类型：{}", category.label()));
            for warning in safety::risky(command) {
                warnings.push_str(&format!("\n警告：{warning}"));
            }
        }
        let Some(input) = &self.input else {
            return Err(Error::Approval(format!(
                "{description}；非交互模式无法询问（non-interactive）。请明确指定 --permission auto（允许项目内的构建/测试命令和编辑）或 --permission bypass，或在 policies.json 中添加 allow 规则"
            )));
        };
        let remember = self
            .store
            .as_ref()
            .and_then(|_| policy::remember_rule(&effect, &self.project, self.dialect));
        let choices = if remember.is_some() {
            "[y/N/a=always in this project]"
        } else {
            "[y/N]"
        };
        let answer = input
            .answer(
                format!("{head}\n{description}{warnings}\n是否允许？{choices} "),
                cancel,
            )
            .await?;
        match reply(&answer) {
            Reply::Yes => Ok(()),
            Reply::Always if remember.is_some() => {
                if let (Some(rule), Some(path)) = (remember, &self.store) {
                    let rules = policy::append(path, &rule)?;
                    if let Ok(mut current) = self.rules.write() {
                        *current = rules;
                    }
                }
                Ok(())
            }
            Reply::No(Some(reason)) => {
                Err(Error::Denied(format!("{description}；用户回复：{reason}")))
            }
            _ => Err(Error::Denied(description)),
        }
    }
}

enum Reply {
    Yes,
    Always,
    /// Denied, with the reason typed after `n`.
    No(Option<String>),
}

/// An approval answer: y/yes/是/允许, a/always/总是, anything else denies; text
/// after `n`/`no`/否/拒绝 is the reason the model is given.
fn reply(answer: &str) -> Reply {
    let answer = answer.trim();
    match answer.to_lowercase().as_str() {
        "y" | "yes" | "是" | "允许" => return Reply::Yes,
        "a" | "always" | "总是" => return Reply::Always,
        _ => {}
    }
    for word in ["no", "n", "否", "拒绝"] {
        if let Some(rest) = answer.strip_prefix(word)
            && (rest.is_empty() || rest.starts_with([' ', ',', ':', '，', '：']))
        {
            let reason = rest.trim_start_matches([' ', ',', ':', '，', '：']).trim();
            return Reply::No((!reason.is_empty()).then(|| reason.to_owned()));
        }
    }
    Reply::No(None)
}

fn poisoned() -> Error {
    Error::Session("permission state lock poisoned".into())
}

pub fn decide(
    mode: Mode,
    effect: &Effect,
    rules: &[PolicyRule],
    project: &Path,
    roots: Option<&[PathBuf]>,
    dialect: Dialect,
) -> Decision {
    if matches!(effect, Effect::Read) {
        return Decision::Allow;
    }
    if let Effect::Shell(command) = effect
        && let Some(why) = safety::catastrophic(command)
    {
        return Decision::Deny(format!("blocked in every mode: {why}"));
    }
    if policy::deny(rules, effect, project, dialect) {
        return Decision::Deny("denied by policy rule".into());
    }
    if mode == Mode::Plan || mode == Mode::Bypass {
        return mode_default(mode, effect, roots, dialect);
    }
    match policy::allow_or_ask(rules, effect, project, dialect) {
        RuleDecision::Allow => Decision::Allow,
        RuleDecision::Ask => Decision::Ask,
        RuleDecision::Deny | RuleDecision::None => mode_default(mode, effect, roots, dialect),
    }
}

async fn authorize_with(
    decision: Decision,
    effect: Effect,
    cancel: &CancellationToken,
) -> Result<Option<Effect>> {
    if cancel.is_cancelled() {
        return Err(Error::Cancelled);
    }
    match decision {
        Decision::Allow => Ok(None),
        Decision::Deny(reason) => Err(Error::Approval(format!("{}: {reason}", effect.describe()))),
        Decision::Ask => Ok(Some(effect)),
    }
}

impl Authorizer for Permissions {
    fn authorize(&self, effect: Effect, cancel: CancellationToken) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            let decision = self.decide(&effect);
            match authorize_with(decision, effect, &cancel).await? {
                None => Ok(()),
                Some(effect) => self.ask(effect, cancel).await,
            }
        })
    }
}

/// A fixed mode without rules, for sub-agents and tests.
pub struct LocalAuthorizer {
    pub mode: Mode,
    pub input: Option<Arc<dyn UserInput>>,
}

impl Authorizer for LocalAuthorizer {
    fn authorize(&self, effect: Effect, cancel: CancellationToken) -> BoxFuture<'_, Result<()>> {
        Box::pin(async move {
            let decision = decide(
                self.mode,
                &effect,
                &[],
                Path::new(""),
                None,
                Dialect::host(),
            );
            let Some(effect) = authorize_with(decision, effect, &cancel).await? else {
                return Ok(());
            };
            let description = effect.describe();
            // Concurrent read-only calls (web tools) must not interleave prompts.
            static PROMPT: tokio::sync::Mutex<()> = tokio::sync::Mutex::const_new(());
            let _turn = PROMPT.lock().await;
            let Some(input) = &self.input else {
                return Err(Error::Approval(format!(
                    "{description}；非交互模式无法询问（non-interactive）。请明确指定 --permission auto 或 bypass 以允许写入和 shell"
                )));
            };
            let answer = input
                .answer(format!("需要批准\n{description}\n是否允许？[y/N] "), cancel)
                .await?;
            match reply(&answer) {
                Reply::Yes => Ok(()),
                Reply::No(Some(reason)) => {
                    Err(Error::Denied(format!("{description}；用户回复：{reason}")))
                }
                _ => Err(Error::Denied(description)),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn approval_replies_take_chinese_words_and_denial_reasons() {
        assert!(matches!(reply(" Y "), Reply::Yes));
        assert!(matches!(reply("允许"), Reply::Yes));
        assert!(matches!(reply("总是"), Reply::Always));
        assert!(matches!(reply(""), Reply::No(None)));
        assert!(matches!(reply("nope"), Reply::No(None)));
        assert!(
            matches!(reply("n use the test script instead"), Reply::No(Some(reason)) if reason == "use the test script instead")
        );
        assert!(matches!(reply("拒绝：先备份"), Reply::No(Some(reason)) if reason == "先备份"));
    }

    fn decide_posix(mode: Mode, effect: Effect) -> Decision {
        decide(
            mode,
            &effect,
            &[],
            Path::new("/p"),
            Some(&[PathBuf::from("/p")]),
            Dialect::Posix,
        )
    }

    #[test]
    fn modes_follow_the_go_order() {
        let shell = |c: &str| Effect::Shell(c.into());
        assert_eq!(
            decide_posix(Mode::Default, shell("git status")),
            Decision::Allow
        );
        assert_eq!(
            decide_posix(Mode::Default, shell("cargo test")),
            Decision::Ask
        );
        assert_eq!(
            decide_posix(Mode::Auto, shell("cargo test")),
            Decision::Allow
        );
        assert_eq!(
            decide_posix(Mode::Auto, shell("cargo install x")),
            Decision::Ask
        );
        assert_eq!(
            decide_posix(Mode::Auto, Effect::Write("/p/a".into())),
            Decision::Allow
        );
        assert_eq!(
            decide_posix(Mode::Auto, Effect::Write("/etc/a".into())),
            Decision::Ask
        );
        assert!(matches!(
            decide_posix(Mode::Plan, shell("cargo test")),
            Decision::Deny(_)
        ));
        assert_eq!(decide_posix(Mode::Plan, shell("ls")), Decision::Allow);
        assert!(matches!(
            decide_posix(Mode::Plan, Effect::Write("/p/a".into())),
            Decision::Deny(_)
        ));
        assert_eq!(
            decide_posix(Mode::Bypass, shell("anything --at-all")),
            Decision::Allow
        );
        assert!(matches!(
            decide_posix(Mode::Bypass, shell("rm -rf ~")),
            Decision::Deny(_)
        ));
        assert_eq!(Mode::parse("readonly"), Some(Mode::Plan));
        assert_eq!(Mode::Ask, Mode::Default);
    }

    #[test]
    fn deny_rules_win_over_bypass_and_allow_rules_over_ask() {
        let rules: Vec<PolicyRule> = serde_json::from_value(serde_json::json!([
            {"tool_pattern":"shell","action":"deny","command_prefix":"git push"},
            {"tool_pattern":"shell","action":"allow","command_prefix":"make"}
        ]))
        .unwrap();
        let d = |mode, c: &str| {
            decide(
                mode,
                &Effect::Shell(c.into()),
                &rules,
                Path::new("/p"),
                None,
                Dialect::Posix,
            )
        };
        assert!(matches!(d(Mode::Bypass, "git push"), Decision::Deny(_)));
        assert_eq!(d(Mode::Default, "make all"), Decision::Allow);
        assert!(matches!(d(Mode::Plan, "make all"), Decision::Deny(_)));
    }

    #[test]
    fn plan_origins() {
        let permissions = Permissions::new(Mode::Auto, "/p".into(), vec![], None);
        assert!(permissions.enter_plan(PlanOrigin::Model));
        assert_eq!(permissions.mode(), Mode::Plan);
        assert_eq!(permissions.exit_plan(PlanOrigin::Model), Ok(Mode::Auto));
        permissions.set_mode(Mode::Plan);
        assert!(permissions.exit_plan(PlanOrigin::Model).is_err());
        assert!(!permissions.enter_plan(PlanOrigin::Model));
        assert_eq!(permissions.exit_plan(PlanOrigin::User), Ok(Mode::Auto));
        assert!(permissions.enter_plan(PlanOrigin::Workflow));
        assert!(permissions.exit_plan(PlanOrigin::Model).is_err());
    }
}
