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
        (Mode::Default | Mode::Auto, Effect::Shell(command))
            if classify::local_git(command, dialect) =>
        {
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
    /// "Don't ask again this session" rules: like remembered ones, never written.
    session: RwLock<Vec<PolicyRule>>,
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
            session: RwLock::new(Vec::new()),
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
        let mut rules = self
            .rules
            .read()
            .map(|rules| rules.clone())
            .unwrap_or_default();
        if let Ok(session) = self.session.read() {
            rules.extend(session.iter().cloned());
        }
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
        // Choices in the manner of Claude Code and Codex: once, this session, always in
        // this project (naming what the rule will cover), or no with a reason.
        let rule = policy::remember_rule(&effect, &self.project, self.dialect);
        let lasting = self.store.is_some();
        let mut options = vec!["1. 允许这一次 (y)".to_owned()];
        let mut keys = vec!["y"];
        if let Some(rule) = &rule {
            let covers = scope_label(rule);
            options.push(format!("2. 本次会话不再询问 (s) - {covers}"));
            keys.push("s");
            if lasting {
                options.push(format!("3. 本项目以后都不再询问 (a) - {covers}"));
                keys.push("a");
            }
        }
        options.push(format!(
            "{}. 拒绝 (n) - 或直接输入理由，告诉模型该怎么做",
            options.len() + 1
        ));
        keys.push("N");
        let answer = input
            .answer(
                format!(
                    "{head}\n{description}{warnings}\n{}\n是否允许？[{}] ",
                    options.join("\n"),
                    keys.join("/")
                ),
                cancel,
            )
            .await?;
        let deny = options.len().to_string();
        let answer = match answer.trim() {
            "1" => "y",
            "2" if rule.is_some() => "s",
            "3" if rule.is_some() && lasting => "a",
            number if number == deny => "n",
            other => other,
        }
        .to_owned();
        match reply(&answer) {
            Reply::Yes => Ok(()),
            Reply::Session if rule.is_some() => {
                if let (Some(rule), Ok(mut session)) = (rule, self.session.write()) {
                    session.push(rule);
                }
                Ok(())
            }
            Reply::Always if rule.is_some() && lasting => {
                if let (Some(rule), Some(path)) = (rule, &self.store) {
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

/// What "don't ask again" will let through, in the user's words.
fn scope_label(rule: &PolicyRule) -> String {
    if rule.command_group == "git" {
        "git 常规操作（push、pull、merge 等；--force、reset、clean 仍会询问）".into()
    } else if !rule.command_group.is_empty() {
        format!("{} 的常规命令（build、test 等）", rule.command_group)
    } else if !rule.command_prefix.is_empty() {
        format!("以 `{}` 开头的命令", rule.command_prefix)
    } else if rule.tool_pattern == "write" {
        "本项目内的文件修改".into()
    } else if rule.tool_pattern.starts_with("web") {
        "网页工具的网络访问".into()
    } else {
        rule.description.clone()
    }
}

enum Reply {
    Yes,
    Session,
    Always,
    /// Denied, with the reason the model is told.
    No(Option<String>),
}

/// An approval answer: y/yes/是/允许, s/session（本次会话）, a/always/总是; n/no/否/拒绝
/// (with the reason after it) or any other text denies, the text being the reason.
fn reply(answer: &str) -> Reply {
    let answer = answer.trim();
    match answer.to_lowercase().as_str() {
        "y" | "yes" | "是" | "允许" => return Reply::Yes,
        "s" | "session" | "本次会话" => return Reply::Session,
        "a" | "always" | "总是" => return Reply::Always,
        "" => return Reply::No(None),
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
    // Anything else typed is what the user wants instead.
    Reply::No(Some(answer.to_owned()))
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
        assert!(matches!(reply("s"), Reply::Session));
        // Text that is no choice is what the user wants instead.
        assert!(matches!(reply("先别推送"), Reply::No(Some(reason)) if reason == "先别推送"));
        assert!(
            matches!(reply("n use the test script instead"), Reply::No(Some(reason)) if reason == "use the test script instead")
        );
        assert!(matches!(reply("拒绝：先备份"), Reply::No(Some(reason)) if reason == "先备份"));
    }

    struct Answers(std::sync::Mutex<Vec<String>>, std::sync::Mutex<Vec<String>>);

    impl UserInput for Answers {
        fn answer(
            &self,
            prompt: String,
            _: CancellationToken,
        ) -> futures_util::future::BoxFuture<'_, Result<String>> {
            self.1.lock().unwrap().push(prompt);
            let answer = self.0.lock().unwrap().remove(0);
            Box::pin(async move { Ok(answer) })
        }
    }

    #[tokio::test]
    async fn this_session_choice_stops_asking_and_names_what_it_covers() {
        let input = Arc::new(Answers(
            std::sync::Mutex::new(vec!["s".into()]),
            std::sync::Mutex::default(),
        ));
        let permissions = Permissions::new(
            Mode::Default,
            std::env::temp_dir(),
            Vec::new(),
            Some(input.clone()),
        );
        let push = || Effect::Shell("git push origin main".into());
        permissions
            .authorize(push(), CancellationToken::new())
            .await
            .unwrap();
        // Asked once; the second push goes through on the session rule.
        permissions
            .authorize(push(), CancellationToken::new())
            .await
            .unwrap();
        let prompts = input.1.lock().unwrap();
        assert_eq!(prompts.len(), 1);
        assert!(prompts[0].contains("1. 允许这一次 (y)"), "{}", prompts[0]);
        assert!(
            prompts[0].contains("2. 本次会话不再询问 (s) - git 常规操作"),
            "{}",
            prompts[0]
        );
        // No policies file: nothing can be remembered beyond the session.
        assert!(
            !prompts[0].contains("本项目以后都不再询问"),
            "{}",
            prompts[0]
        );
        assert!(prompts[0].trim_end().ends_with("[y/s/N]"), "{}", prompts[0]);
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
