//! Risk categories of shell command lines, judged on the tokenized line.

use super::{
    safety,
    shell::{self, Dialect, Line, Simple, Word},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Category {
    /// Read-only; never changes anything.
    Safe,
    /// Build or test; runs project code and may write build artifacts.
    Build,
    /// Git operation that may change the repository.
    Git,
    /// Package installation; may change the system.
    Install,
    /// Not recognized or not statically judgeable.
    Unknown,
    /// Irreversible damage outside the project; always blocked.
    Dangerous,
}

impl Category {
    pub fn label(self) -> &'static str {
        match self {
            Self::Safe => "只读",
            Self::Build => "构建/测试",
            Self::Git => "git（可能修改仓库）",
            Self::Install => "安装依赖包",
            Self::Unknown => "未识别",
            Self::Dangerous => "危险",
        }
    }
}

/// Riskiest category of all simple commands in `command`.
pub fn classify(command: &str, dialect: Dialect) -> Category {
    classify_parsed(command, dialect).0
}

pub(crate) fn classify_parsed(command: &str, dialect: Dialect) -> (Category, Line) {
    let line = shell::parse(command, dialect);
    if command.trim().is_empty() {
        return (Category::Unknown, line);
    }
    if safety::catastrophic(command).is_some() {
        return (Category::Dangerous, line);
    }
    if !line.opaque.is_empty() || line.pipelines.is_empty() {
        return (Category::Unknown, line);
    }
    let worst = line
        .commands()
        .map(|simple| classify_simple(simple, dialect))
        .max()
        .unwrap_or(Category::Unknown);
    (worst, line)
}

/// Auto-allowed in every mode except plan restrictions: read-only and no network.
pub fn read_only(command: &str, dialect: Dialect) -> bool {
    let (category, line) = classify_parsed(command, dialect);
    category == Category::Safe && !line.commands().any(fetches)
}

/// Auto-allowed in `auto` mode: read-only or build/test, and no network fetch.
pub fn auto_approvable(command: &str, dialect: Dialect) -> bool {
    let (category, line) = classify_parsed(command, dialect);
    matches!(category, Category::Safe | Category::Build) && !line.commands().any(fetches)
}

/// True when an otherwise auto-approvable command can still rewrite project sources
/// (formatters and autofixers): evidence of earlier checks must not survive it.
pub fn may_rewrite_sources(command: &str, dialect: Dialect) -> bool {
    let line = shell::parse(command, dialect);
    line.commands().any(|simple| {
        let argv: Vec<String> = simple.argv().iter().map(|w| (*w).to_owned()).collect();
        let words = shell::strip_runners(&argv);
        let Some(exe) = words.first() else {
            return false;
        };
        let name = shell::program_name(exe);
        let args: Vec<&str> = words[1..].iter().map(String::as_str).collect();
        let sub = args.first().copied().unwrap_or_default();
        match name.as_str() {
            "cargo" => {
                matches!(sub, "fmt" | "fix")
                    || (sub == "clippy" && args.iter().any(|a| a.starts_with("--fix")))
            }
            "go" => matches!(sub, "fmt" | "fix"),
            "gofmt" | "shfmt" => args.iter().any(|a| *a == "-w" || a.starts_with("-w=")),
            // rustfmt rewrites in place unless told to only check or print.
            "rustfmt" => !args
                .iter()
                .any(|a| *a == "--check" || a.starts_with("--emit")),
            _ => false,
        }
    })
}

/// The first command of `command` that installs packages, generates code, migrates a
/// database or formats/autofixes files, as written: a step that changes files no single
/// plan item owns. `None` when there is none; commands it does
/// not know are not judged (their changes are caught after the fact).
pub fn generates_files(command: &str, dialect: Dialect) -> Option<String> {
    let line = shell::parse(&shell::normalize_dashes(command), dialect);
    line.commands().find_map(|simple| {
        let words = command_words(&simple.words, dialect)?;
        let argv: Vec<String> = words.iter().map(|word| word.text.clone()).collect();
        let rest = shell::strip_runners(&argv);
        generating(rest).then(|| argv.join(" "))
    })
}

/// Words of script, target and task names that usually generate, migrate, install or
/// format. Matched as whole words of the name (see [`name_words`]), or as compounds of
/// [`STEMS`]: `gen` is not in `agent` or `legend`, `fix` not in `prefix`, `proto` not in
/// `prototype`.
const GENERATING_WORDS: &[&str] = &[
    "gen",
    "regen",
    "codegen",
    "typegen",
    "protogen",
    "generate",
    "regenerate",
    "generated",
    "generator",
    "scaffold",
    "format",
    "fmt",
    "fix",
    "autofix",
    "prettier",
    "prettify",
    "migrate",
    "migration",
    "migrations",
    "makemigrations",
    "install",
    "prisma",
    "proto",
    "protos",
    "protoc",
    "protobuf",
    "bootstrap",
    "restore",
];

/// The words of a script/target name: split at every character that is not a letter or
/// digit (`:`, `-`, `_`, `.`, `/`) and at lower-to-upper case changes (`generateProto`),
/// lowercased.
fn name_words(name: &str) -> Vec<String> {
    let mut words = Vec::new();
    let mut word = String::new();
    let mut lower = false;
    for c in name.chars() {
        let boundary = !c.is_alphanumeric() || (c.is_uppercase() && lower);
        if boundary && !word.is_empty() {
            words.push(std::mem::take(&mut word));
        }
        if c.is_alphanumeric() {
            word.extend(c.to_lowercase());
            lower = c.is_lowercase() || c.is_numeric();
        } else {
            lower = false;
        }
    }
    if !word.is_empty() {
        words.push(word);
    }
    words
}

/// Stems that make a word generating when the word is longer than the stem and
/// starts (`prefix`) or ends (`suffix`) with it: `reformat`, `autoformat`, `lintfix`,
/// `dbmigrate`, `gentypes`, `mockgen`, `genproto`, `htmlgen`. `except` lists ordinary
/// words of that shape.
struct Stem {
    stem: &'static str,
    prefix: bool,
    suffix: bool,
    except: &'static [&'static str],
}

const STEMS: &[Stem] = &[
    Stem {
        stem: "format",
        prefix: true,
        suffix: true,
        except: &[],
    },
    Stem {
        stem: "fmt",
        prefix: true,
        suffix: true,
        except: &[],
    },
    Stem {
        stem: "fix",
        prefix: false,
        suffix: true,
        except: &["prefix", "suffix", "affix", "infix", "postfix", "crucifix"],
    },
    Stem {
        stem: "gen",
        prefix: true,
        suffix: true,
        except: &[
            "general", "generic", "generics", "genre", "genres", "genius", "genome", "gentle",
            "genuine", "genesis", "gender", "genetic", "genetics", "oxygen", "halogen", "collagen",
            "antigen", "nitrogen", "hydrogen",
        ],
    },
    Stem {
        stem: "generate",
        prefix: true,
        suffix: true,
        except: &[],
    },
    Stem {
        stem: "migrate",
        prefix: true,
        suffix: true,
        except: &[],
    },
    Stem {
        stem: "migration",
        prefix: true,
        suffix: true,
        except: &[],
    },
    Stem {
        stem: "install",
        prefix: true,
        suffix: true,
        except: &[],
    },
    Stem {
        stem: "proto",
        prefix: true,
        suffix: true,
        except: &[
            "prototype",
            "prototypes",
            "prototyping",
            "protocol",
            "protocols",
            "proton",
            "protons",
        ],
    },
];

/// A word of a name that generates, migrates, installs or formats: one of
/// [`GENERATING_WORDS`], or a compound of one of the [`STEMS`].
fn generating_word(word: &str) -> bool {
    GENERATING_WORDS.contains(&word)
        || STEMS.iter().any(|stem| {
            word.len() > stem.stem.len()
                && ((stem.prefix && word.starts_with(stem.stem))
                    || (stem.suffix && word.ends_with(stem.stem)))
                && !stem.except.contains(&word)
        })
}

/// Formatting words, which also name a check-only mode next to `check`/`verify`.
fn formatting_word(word: &str) -> bool {
    ["format", "fmt", "prettier", "prettify"]
        .iter()
        .any(|stem| word.contains(stem))
}

/// A script or target name that generates, migrates, installs or formats. A formatting
/// word only checks when `check`/`verify` stands right next to it or inside it
/// (`format:check`, `check-fmt`, `fmtcheck`), or when `checking` (the script runs with
/// `--check`); other generating words always count (`generate-and-check`), and so does a
/// formatting word with something in between (`format-and-check`).
fn generating_name(name: &str, checking: bool) -> bool {
    let words = name_words(name);
    let checks = |word: &str| matches!(word, "check" | "verify");
    words.iter().enumerate().any(|(at, word)| {
        if !generating_word(word) {
            return false;
        }
        if !formatting_word(word) {
            return true;
        }
        let only_checks = checking
            || word.contains("check")
            || word.contains("verify")
            || (at > 0 && checks(&words[at - 1]))
            || words.get(at + 1).is_some_and(|next| checks(next));
        !only_checks
    })
}

/// Arguments after a script name that make it only check (`npm run format -- --check`).
fn checking_only(args: &[&str]) -> bool {
    args.iter()
        .any(|arg| matches!(*arg, "--check" | "--dry-run" | "--list-different"))
}

/// The targets, tasks or goals of a build tool's arguments: not options, not the value of
/// an option that takes one (`-C dir`, `--tests '*X*'`, `-pl module`), not `NAME=value`.
fn build_targets<'a>(tool: &str, args: &[&'a str]) -> Vec<&'a str> {
    let valued: &[&str] = match tool {
        "make" => &[
            "-C",
            "--directory",
            "-f",
            "--file",
            "--makefile",
            "-I",
            "--include-dir",
            "-o",
            "--old-file",
            "--assume-old",
            "-W",
            "--what-if",
            "--new-file",
            "--assume-new",
        ],
        "gradle" | "gradlew" => &[
            "--tests",
            "-p",
            "--project-dir",
            "-b",
            "--build-file",
            "-c",
            "--settings-file",
            "-x",
            "--exclude-task",
            "-I",
            "--init-script",
            "-g",
            "--gradle-user-home",
            "--console",
            "--warning-mode",
            "--include-build",
            "--project-cache-dir",
            "--priority",
            "-D",
            "-P",
            "-M",
        ],
        "mvn" | "mvnw" => &[
            "-pl",
            "--projects",
            "-f",
            "--file",
            "-s",
            "--settings",
            "-gs",
            "--global-settings",
            "-t",
            "--toolchains",
            "-gt",
            "--global-toolchains",
            "-P",
            "--activate-profiles",
            "-D",
            "--define",
            "-rf",
            "--resume-from",
            "-T",
            "--threads",
            "-l",
            "--log-file",
            "-b",
            "--builder",
        ],
        "just" => &[
            "-f",
            "--justfile",
            "-d",
            "--working-directory",
            "--set",
            "--shell",
            "--shell-arg",
            "--dotenv-filename",
            "--dotenv-path",
            "--color",
        ],
        "task" => &[
            "-d",
            "--dir",
            "-t",
            "--taskfile",
            "-o",
            "--output",
            "-c",
            "--concurrency",
            "--sort",
            "--interval",
        ],
        _ => &[],
    };
    // Options whose value is optional: skipped only when a number follows.
    let counted = tool == "make";
    let mut targets = Vec::new();
    let mut words = args.iter().copied();
    while let Some(word) = words.next() {
        if valued.contains(&word) {
            words.next();
        } else if counted && matches!(word, "-j" | "--jobs" | "-l" | "--load-average") {
            if words
                .clone()
                .next()
                .is_some_and(|next| next.parse::<f64>().is_ok())
            {
                words.next();
            }
        } else if word == "--" {
            continue;
        } else if !word.starts_with('-') && !word.contains('=') {
            targets.push(word);
        }
    }
    targets
}

fn generating(words: &[String]) -> bool {
    let Some(program) = words.first() else {
        return false;
    };
    let name = shell::program_name(program);
    let args: Vec<&str> = words[1..].iter().map(String::as_str).collect();
    let sub = args.first().copied().unwrap_or_default();
    let has = |flags: &[&str]| {
        args.iter().any(|arg| {
            flags
                .iter()
                .any(|flag| arg == flag || arg.starts_with(&format!("{flag}=")))
        })
    };
    // Runners of a package's tool: judge the tool.
    let runner = match name.as_str() {
        "npx" | "pnpx" | "bunx" => Some(&words[1..]),
        "npm" | "pnpm" | "yarn" | "bun" if matches!(sub, "exec" | "dlx" | "x") => Some(&words[2..]),
        "bundle" | "bundler" | "poetry" | "pipenv" | "uv" if sub == "run" || sub == "exec" => {
            Some(&words[2..])
        }
        "dotnet" if sub == "tool" && args.get(1) == Some(&"run") => words.get(3..),
        _ => None,
    };
    if let Some(rest) = runner {
        let start = rest
            .iter()
            .position(|word| !word.starts_with('-'))
            .unwrap_or(rest.len());
        return generating(&rest[start..]);
    }
    match name.as_str() {
        "npm" | "pnpm" | "yarn" | "bun" => {
            matches!(
                sub,
                "install"
                    | "i"
                    | "ci"
                    | "add"
                    | "update"
                    | "up"
                    | "upgrade"
                    | "remove"
                    | "rm"
                    | "uninstall"
                    | "un"
                    | "dedupe"
                    | "prune"
                    | "link"
                    | "rebuild"
                    | "import"
            ) || (name == "yarn" && args.is_empty())
                || (matches!(sub, "run" | "run-script")
                    && args[1..]
                        .iter()
                        .position(|word| !word.starts_with('-'))
                        .is_some_and(|at| {
                            generating_name(args[1 + at], checking_only(&args[2 + at..]))
                        }))
                || (name != "npm"
                    && !sub.starts_with('-')
                    && generating_name(sub, checking_only(&args[1..])))
        }
        "pip" | "pip3" | "pipx" => matches!(sub, "install" | "uninstall" | "download"),
        "python" | "python3" | "py" => {
            let module = args
                .iter()
                .position(|arg| *arg == "-m")
                .and_then(|at| args.get(at + 1..));
            match module {
                Some([tool, rest @ ..]) => match *tool {
                    "pip" => rest
                        .first()
                        .is_some_and(|sub| matches!(*sub, "install" | "uninstall" | "download")),
                    "black" | "isort" => !rest
                        .iter()
                        .any(|arg| matches!(*arg, "--check" | "--diff" | "--check-only")),
                    "autopep8" | "autoflake" => {
                        rest.iter().any(|arg| *arg == "-i" || *arg == "--in-place")
                    }
                    "ruff" => rest.first() == Some(&"format") || rest.contains(&"--fix"),
                    "grpc_tools.protoc" | "alembic" | "django" => true,
                    _ => false,
                },
                _ => args
                    .iter()
                    .position(|arg| arg.ends_with("manage.py"))
                    .is_some_and(|at| {
                        args.get(at + 1).is_some_and(|command| {
                            matches!(*command, "migrate" | "makemigrations" | "collectstatic")
                        })
                    }),
            }
        }
        "uv" => {
            matches!(sub, "add" | "remove" | "sync" | "lock")
                || (sub == "pip" && args.get(1).is_some_and(|sub| *sub == "install"))
        }
        "poetry" | "pipenv" | "pdm" => {
            matches!(
                sub,
                "install" | "add" | "remove" | "update" | "lock" | "sync"
            )
        }
        "conda" | "mamba" | "gem" | "brew" | "choco" | "winget" | "scoop" | "apt" | "apt-get"
        | "yum" | "dnf" | "zypper" | "snap" | "flatpak" => matches!(
            sub,
            "install" | "uninstall" | "remove" | "update" | "upgrade" | "add" | "create"
        ),
        "pacman" => sub.starts_with("-S") || sub.starts_with("-R") || sub.starts_with("-U"),
        "bundle" | "bundler" => matches!(sub, "install" | "update" | "add" | "lock" | "remove"),
        "composer" => matches!(
            sub,
            "install" | "require" | "update" | "remove" | "dump-autoload" | "dumpautoload"
        ),
        "dotnet" => {
            matches!(sub, "restore" | "format" | "new" | "add" | "remove" | "ef")
                || (sub == "tool"
                    && args
                        .get(1)
                        .is_some_and(|sub| matches!(*sub, "restore" | "install" | "update")))
        }
        "dotnet-ef" | "dotnet-format" => true,
        "nuget" => matches!(sub, "restore" | "install" | "update"),
        "cargo" => {
            matches!(
                sub,
                "fmt" | "fix" | "add" | "remove" | "rm" | "update" | "install" | "generate"
            ) || (sub == "clippy" && args.iter().any(|arg| arg.starts_with("--fix")))
        }
        "rustfmt" => !args
            .iter()
            .any(|arg| *arg == "--check" || arg.starts_with("--emit")),
        "go" => {
            matches!(sub, "generate" | "get" | "fmt" | "fix" | "install")
                || (sub == "mod"
                    && args.get(1).is_some_and(|sub| {
                        matches!(*sub, "tidy" | "download" | "vendor" | "init" | "edit")
                    }))
        }
        "gofmt" | "goimports" | "gofumpt" | "shfmt" => has(&["-w"]),
        "protoc"
        | "flatc"
        | "thrift"
        | "mockgen"
        | "wire"
        | "openapi-generator"
        | "openapi-generator-cli"
        | "swagger-codegen"
        | "graphql-codegen"
        | "quicktype" => true,
        "buf" => matches!(sub, "generate" | "format"),
        "sqlc" => sub == "generate",
        "prisma" => matches!(sub, "generate" | "migrate" | "db" | "format"),
        "drizzle-kit" => matches!(sub, "generate" | "migrate" | "push"),
        "sequelize" | "sequelize-cli" | "knex" | "typeorm" | "mikro-orm" => args
            .iter()
            .any(|arg| arg.contains("migrat") || arg.contains("seed")),
        "alembic" => matches!(sub, "upgrade" | "downgrade" | "revision" | "stamp"),
        "flyway" | "liquibase" => matches!(sub, "migrate" | "update" | "repair" | "baseline"),
        "rails" | "rake" => {
            matches!(sub, "generate" | "g" | "destroy" | "d") || sub.starts_with("db:")
        }
        "prettier" => has(&["--write", "-w"]),
        "eslint" | "stylelint" | "tslint" | "standard" | "xo" => {
            args.iter().any(|arg| arg.starts_with("--fix"))
        }
        "biome" => {
            sub == "format" && has(&["--write"])
                || args
                    .iter()
                    .any(|arg| matches!(*arg, "--write" | "--apply" | "--apply-unsafe" | "--fix"))
        }
        "rubocop" => args
            .iter()
            .any(|arg| matches!(*arg, "-a" | "-A") || arg.starts_with("--auto")),
        "ruff" => sub == "format" && !has(&["--check", "--diff"]) || has(&["--fix"]),
        "black" | "isort" => !args
            .iter()
            .any(|arg| matches!(*arg, "--check" | "--diff" | "--check-only")),
        "autopep8" | "autoflake" => has(&["-i", "--in-place"]),
        "clang-format" => has(&["-i"]),
        "swiftformat" | "dartfmt" => true,
        "ktlint" => has(&["-F", "--format"]),
        "terraform" | "tofu" => sub == "fmt" && !has(&["-check"]),
        "deno" => sub == "fmt" && !has(&["--check"]) || sub == "install",
        "mix" => matches!(
            sub,
            "format" | "deps.get" | "ecto.migrate" | "ecto.gen.migration"
        ),
        "dart" => sub == "format" || (sub == "fix" && has(&["--apply"])) || sub == "pub",
        "flutter" => matches!(sub, "pub" | "create" | "format"),
        "make" | "just" | "task" | "gradle" | "gradlew" | "mvn" | "mvnw" => {
            build_targets(&name, &args)
                .iter()
                .any(|target| generating_name(target, false))
        }
        "install-module" | "install-package" | "update-module" | "install-script" => true,
        _ => false,
    }
}

/// Git steps that stay in the local repository and can be undone (`add`, `commit`,
/// `tag`), alone or with read-only commands: no approval in default or auto mode.
/// Push and anything forced, deleting or rewriting still asks.
pub fn local_git(command: &str, dialect: Dialect) -> bool {
    git_only(command, dialect, &["add", "commit", "tag"])
}

/// `git push`/`git fetch`, alone or with read-only commands: they never touch the
/// working tree. Only for check evidence; approval rules are unchanged.
pub fn remote_git(command: &str, dialect: Dialect) -> bool {
    git_only(command, dialect, &["push", "fetch"])
}

fn git_only(command: &str, dialect: Dialect, subcommands: &[&str]) -> bool {
    let (category, line) = classify_parsed(command, dialect);
    if category > Category::Git || !line.opaque.is_empty() {
        return false;
    }
    let git = group("git");
    let mut any = false;
    for simple in line.commands() {
        if classify_simple(simple, dialect) == Category::Safe && !fetches(simple) {
            continue;
        }
        let words = simple.argv();
        let local = words
            .first()
            .is_some_and(|program| shell::program_name(program) == "git")
            && git_subcommand(&words[1..]).is_some_and(|(sub, _)| subcommands.contains(&sub))
            && git.is_some_and(|group| group.covers(&words))
            && simple
                .redirects
                .iter()
                .all(|r| r.op.starts_with('<') || discards(&r.target));
        if !local {
            return false;
        }
        any = true;
    }
    any
}

fn fetches(simple: &Simple) -> bool {
    simple.words.first().is_some_and(|word| {
        matches!(
            shell::program_name(&word.text).as_str(),
            "curl" | "wget" | "iwr" | "irm" | "invoke-webrequest" | "invoke-restmethod"
        )
    })
}

pub(crate) fn discards(target: &str) -> bool {
    let target = target.to_ascii_lowercase();
    target.starts_with('&')
        || matches!(
            target.as_str(),
            "/dev/null" | "$null" | "nul" | "/dev/stdout" | "/dev/stderr"
        )
}

/// Installed toolchains whose version/help/listing flags only print.
const TOOLCHAINS: &[&str] = &[
    "dotnet", "node", "npm", "npx", "pnpm", "yarn", "bun", "deno", "python", "python3", "py",
    "pip", "pip3", "go", "rustc", "cargo", "rustup", "java", "javac", "mvn", "gradle", "git",
    "pwsh", "powershell", "cmake", "make", "ninja", "gcc", "g++", "clang", "tsc", "ruby", "gem",
    "php", "composer", "perl", "dart", "flutter", "kotlin", "swift", "nuget", "msbuild",
    "docker", "kubectl", "helm", "terraform", "gh", "uv", "poetry", "conda",
];

const INFO_ARGS: &[&str] = &[
    "--version", "-version", "-v", "-V", "version", "--help", "-h", "--info", "--list-sdks",
    "--list-runtimes",
];

/// Subcommands that only query: package searches, dependency listings, status views.
/// Matched against the leading non-option arguments.
const QUERIES: &[(&str, &[&str])] = &[
    ("dotnet", &["package", "search"]),
    ("dotnet", &["package", "list"]),
    ("dotnet", &["list", "package"]),
    ("dotnet", &["list", "reference"]),
    ("dotnet", &["sdk", "check"]),
    ("dotnet", &["workload", "list"]),
    ("dotnet", &["tool", "list"]),
    ("dotnet", &["tool", "search"]),
    ("dotnet", &["new", "list"]),
    ("dotnet", &["nuget", "why"]),
    ("cargo", &["search"]),
    ("cargo", &["version"]),
    ("cargo", &["locate-project"]),
    ("cargo", &["pkgid"]),
    ("cargo", &["verify-project"]),
    ("cargo", &["read-manifest"]),
    ("cargo", &["info"]),
    ("rustup", &["show"]),
    ("rustup", &["which"]),
    ("rustup", &["toolchain", "list"]),
    ("rustup", &["target", "list"]),
    ("rustup", &["component", "list"]),
    ("go", &["doc"]),
    ("pip", &["index", "versions"]),
    ("pip3", &["index", "versions"]),
    ("gh", &["pr", "view"]),
    ("gh", &["pr", "list"]),
    ("gh", &["pr", "status"]),
    ("gh", &["pr", "diff"]),
    ("gh", &["pr", "checks"]),
    ("gh", &["issue", "view"]),
    ("gh", &["issue", "list"]),
    ("gh", &["repo", "view"]),
    ("gh", &["run", "list"]),
    ("gh", &["run", "view"]),
    ("gh", &["release", "list"]),
    ("gh", &["release", "view"]),
    ("kubectl", &["get"]),
    ("kubectl", &["describe"]),
    ("kubectl", &["logs"]),
    ("kubectl", &["explain"]),
    ("kubectl", &["api-resources"]),
    ("helm", &["list"]),
    ("helm", &["status"]),
    ("conda", &["list"]),
    ("conda", &["info"]),
    ("conda", &["env", "list"]),
    ("uv", &["tree"]),
    ("uv", &["pip", "list"]),
    ("uv", &["pip", "show"]),
    ("poetry", &["show"]),
];

/// `args` start with one of `program`'s query subcommands and write no output file.
fn query(program: &str, args: &[&str]) -> bool {
    let positional: Vec<&str> = args.iter().copied().filter(|a| !a.starts_with('-')).collect();
    let writes = args.iter().any(|a| {
        *a == "-o" || a.starts_with("--output") || a.starts_with("--out") || a.starts_with("--web")
    });
    !writes
        && QUERIES.iter().any(|(name, sub)| {
            *name == program
                && positional.len() >= sub.len()
                && positional.iter().zip(sub.iter()).all(|(a, b)| a == b)
        })
}

/// Programs that only read and have no option that writes, deletes or runs something:
/// an argument whose value is unknown (`"$f"`, `$_.FullName`, `%1`) cannot change that.
const PURE: &[&str] = &[
    "cat", "head", "tail", "wc", "ls", "dir", "pwd", "echo", "printf", "true", "false", ":",
    "test", "[", "[[", "read", "cd", "chdir", "pushd", "popd", "basename", "dirname", "realpath",
    "readlink", "stat", "du", "df", "grep", "egrep", "fgrep", "cut", "tr", "nl", "diff", "cmp",
    "comm", "rev", "tac", "od", "hexdump", "md5sum", "sha1sum", "sha256sum", "seq", "column",
    "expand", "fold", "paste", "jq", "which", "where", "type", "whoami", "uname", "cygpath",
    "strings", "findstr", "more", "fc", "sleep", "exit", "return", "break", "continue", "shift",
    // Only reaches classification as `command -v NAME` (otherwise it is a runner).
    "command",
    "get-childitem", "gci", "get-content", "gc", "select-string", "sls", "test-path", "get-item",
    "gi", "resolve-path", "split-path", "join-path", "get-location", "gl", "set-location", "sl",
    "write-output", "write-host", "measure-object", "sort-object", "select-object",
    "format-list", "format-table", "out-string", "get-filehash", "get-itemproperty",
    "get-command", "gcm",
];

/// The words of `simple` that form a command, past control-flow keywords: `None` for
/// constructs never judged (`case`, `function`, cmd `if`/`for`), an empty slice for a
/// keyword alone (`done`, `fi`, a `for` header, `rem`).
pub(crate) fn command_words(words: &[Word], dialect: Dialect) -> Option<&[Word]> {
    let mut rest = words;
    while let Some(first) = rest.first().filter(|word| !word.quoted && !word.dynamic) {
        let keyword = first.text.to_ascii_lowercase();
        match (dialect, keyword.as_str()) {
            (
                Dialect::Posix,
                "!" | "if" | "then" | "else" | "elif" | "while" | "until" | "do" | "time",
            ) => rest = &rest[1..],
            (Dialect::Posix, "fi" | "done" | "for") => return Some(&[]),
            (Dialect::Posix, "case" | "esac" | "select" | "function" | "coproc") => return None,
            (Dialect::Cmd, "rem") => return Some(&[]),
            (Dialect::Cmd, "if" | "for" | "call" | "goto") => return None,
            _ => break,
        }
    }
    Some(rest)
}

/// Variables that change what the shell itself runs or how it splits words: a plain
/// assignment of any other name only affects later expansions in the same line.
const SHELL_CONTROL: &[&str] = &[
    "PATH", "IFS", "CDPATH", "PS4", "PROMPT_COMMAND", "BASH_ENV", "ENV", "SHELLOPTS",
    "BASHOPTS", "GLOBIGNORE", "HOME", "BASH_FUNC",
];

/// Exported, a variable also reaches child processes: these make them load or run code.
fn runs_code_in_children(name: &str) -> bool {
    const PREFIXES: &[&str] = &[
        "LD_", "DYLD_", "GIT_", "PYTHON", "NODE_", "NPM_CONFIG_", "RUSTC", "CARGO_", "PERL5",
        "RUBY", "JAVA_", "_JAVA_", "DOTNET_", "MSBUILD", "COMSPEC", "PSMODULEPATH", "EDITOR",
        "VISUAL", "PAGER", "SSH_", "LESSOPEN", "BROWSER",
    ];
    let upper = name.to_ascii_uppercase();
    SHELL_CONTROL.contains(&upper.as_str())
        || PREFIXES.iter().any(|prefix| upper.starts_with(prefix))
        || upper == "RUSTFLAGS"
}

/// `NAME=value` (or a bare `NAME` for `unset`/`local`) that cannot change what later
/// commands run; `exported` for `export`/`declare -x`, which reach child processes.
pub(crate) fn harmless_variable(word: &str, exported: bool) -> bool {
    let (name, value) = word.split_once('=').unwrap_or((word, ""));
    shell::is_assignment(&format!("{name}="))
        && if exported {
            !runs_code_in_children(name) || inert_value(name, value)
        } else {
            !SHELL_CONTROL.contains(&name.to_ascii_uppercase().as_str())
        }
}

/// A value that names no program, library or path to load: `GIT_PAGER=cat`,
/// `GIT_TERMINAL_PROMPT=0`, `PYTHONIOENCODING=utf-8`. `GIT_CONFIG_KEY_n` only names a
/// setting; its value is judged through `GIT_CONFIG_VALUE_n`.
fn inert_value(name: &str, value: &str) -> bool {
    let upper = name.to_ascii_uppercase();
    if upper == "GIT_CONFIG_COUNT" {
        return value.chars().all(|c| c.is_ascii_digit());
    }
    if upper.starts_with("GIT_CONFIG_KEY_") {
        return !value.is_empty() && value.chars().all(|c| c.is_ascii_alphanumeric() || matches!(c, '.' | '-' | '_'));
    }
    !SHELL_CONTROL.contains(&upper.as_str())
        && matches!(
            value.to_ascii_lowercase().as_str(),
            "" | "0" | "1" | "true" | "false" | "yes" | "no" | "cat" | ":" | "dumb" | "never"
                | "always" | "auto" | "utf-8" | "utf8" | "off" | "on"
        )
}

/// A PowerShell statement that is a value, not a command: `$_.Length -gt 1` in a
/// `Where-Object` block. Assignments are not values.
pub(crate) fn powershell_value(words: &[Word]) -> bool {
    words.iter().all(|word| {
        let text = word.text.as_str();
        !word.block && !(text.ends_with('=') && !text.starts_with('-'))
    })
}

fn classify_simple(simple: &Simple, dialect: Dialect) -> Category {
    if simple
        .redirects
        .iter()
        .any(|r| !r.op.starts_with('<') && !discards(&r.target))
        || simple
            .redirects
            .iter()
            .any(|r| r.op.starts_with('<') && r.op != "<")
    {
        return Category::Unknown;
    }
    let Some(words) = command_words(&simple.words, dialect) else {
        return Category::Unknown;
    };
    let Some(first) = words.first() else {
        return Category::Safe;
    };
    if first.block {
        // A script block literal does not run; its commands are judged on their own.
        return Category::Safe;
    }
    if first.dynamic {
        return if dialect == Dialect::PowerShell
            && first.text.starts_with('$')
            && powershell_value(words)
        {
            Category::Safe
        } else {
            Category::Unknown
        };
    }
    let argv: Vec<String> = words.iter().map(|word| word.text.clone()).collect();
    let rest = shell::strip_runners(&argv);
    let offset = argv.len() - rest.len();
    let Some(program) = rest.first() else {
        // Only assignments: harmless unless they set what later commands run with.
        // (`env` or `timeout 5` alone also strip to nothing, but are not assignments.)
        return if argv
            .iter()
            .all(|word| shell::is_assignment(word) && harmless_variable(word, false))
        {
            Category::Safe
        } else {
            Category::Unknown
        };
    };
    let name = shell::program_name(program);
    if dialect == Dialect::PowerShell {
        let args = &words[offset + 1..];
        match name.as_str() {
            "invoke-expression" | "iex" => return Category::Unknown,
            // Filters run their block (judged separately) or compare properties.
            "where-object" | "where" | "?" => {
                return if args.iter().any(|w| w.dynamic) {
                    Category::Unknown
                } else {
                    Category::Safe
                };
            }
            // Without a block, `ForEach-Object Delete` calls a method on every item.
            "foreach-object" | "foreach" | "%" => {
                return if args.iter().all(|w| w.block || w.text.starts_with('-')) {
                    Category::Safe
                } else {
                    Category::Unknown
                };
            }
            _ => {}
        }
    }
    if words[..offset].iter().any(|w| w.dynamic)
        || (words[offset + 1..].iter().any(|w| w.dynamic || w.block)
            && !PURE.contains(&name.as_str()))
    {
        return Category::Unknown;
    }
    let words: Vec<&str> = argv.iter().map(String::as_str).collect();
    classify_words(&words)
}

fn has(args: &[&str], wanted: &[&str]) -> bool {
    args.iter().any(|arg| wanted.contains(arg))
}

fn unc(word: &str) -> bool {
    let word = word.trim_matches(['"', '\'']);
    (word.starts_with("\\\\") || word.starts_with("//"))
        && word
            .chars()
            .nth(2)
            .is_some_and(|c| !matches!(c, '/' | '\\' | ' '))
}

pub fn classify_words(words: &[&str]) -> Category {
    let Some(exe) = words.first() else {
        return Category::Unknown;
    };
    // The project's own Maven/Gradle wrapper runs the build like the tool itself.
    let wrapper = match exe.trim_start_matches("./").trim_start_matches(".\\") {
        "gradlew" | "gradlew.bat" | "gradlew.cmd" => Some("gradle"),
        "mvnw" | "mvnw.cmd" => Some("mvn"),
        _ => None,
    };
    if let Some(name) = wrapper
        && exe.starts_with('.')
    {
        return classify_build(name, &words[1..]);
    }
    // `[ -f x ]` and `[[ ... ]]` only test.
    if matches!(*exe, "[" | "[[") {
        return Category::Safe;
    }
    if exe.contains(['/', '\\', '$', '=', '*', '?', '[']) || exe.starts_with('-') {
        return Category::Unknown;
    }
    let name = shell::program_name(exe);
    let args = &words[1..];
    if args.iter().any(|arg| unc(arg)) {
        return Category::Unknown;
    }
    // A known toolchain asked only for its version, help or installed SDKs.
    if TOOLCHAINS.contains(&name.as_str())
        && !args.is_empty()
        && args.iter().all(|arg| INFO_ARGS.contains(arg))
    {
        return Category::Safe;
    }
    // Any installed program asked only for its version or usage prints and exits.
    if matches!(args, ["--version" | "--help"]) {
        return Category::Safe;
    }
    if query(&name, args) {
        return Category::Safe;
    }
    match name.as_str() {
        "git" => classify_git(args),
        "ls" | "dir" | "pwd" | "echo" | "cat" | "head" | "tail" | "wc" | "du" | "df"
        | "printenv" | "which" | "where" | "whoami" | "uname" | "uptime" | "id" | "groups"
        | "grep" | "egrep" | "fgrep" | "stat" | "type" | "realpath" | "basename" | "dirname"
        | "true" | "sort" | "uniq" | "cut" | "diff" | "cmp" | "comm" | "nl" | "cd" | "pushd"
        | "popd" | "get-childitem" | "gci" | "get-content" | "gc" | "get-location" | "gl"
        | "select-string" | "sls" | "test-path" | "get-item" | "gi" | "resolve-path"
        | "get-command" | "gcm" | "set-location" | "sl" | "write-output" | "write-host"
        | "select-object" | "measure-object" | "sort-object" | "get-date" | "format-list"
        | "format-table" | "out-string" | "get-filehash" | "split-path" | "join-path"
        | "get-itemproperty" | "strings" | "cygpath" | "file" | "readlink" | "tac" | "rev"
        | "tr" | "od" | "hexdump" | "md5sum" | "sha1sum" | "sha256sum" | "seq" | "column"
        | "expand" | "fold" | "paste" | "jq" | "read" | "test" | "printf" | "false" | ":"
        | "findstr" | "ver" | "vol" | "chdir" | "more" | "fc" | "sleep" | "exit" | "return"
        | "break" | "continue" | "shift" | "wait" => {
            let positional = args.iter().filter(|a| !a.starts_with('-')).count();
            if name == "sort"
                && args.iter().any(|a| {
                    a.starts_with("-o")
                        || a.starts_with("--output")
                        // cmd's `sort /o file` writes too.
                        || a.eq_ignore_ascii_case("/o")
                        // GNU sort runs this program on its temp files: arbitrary execution.
                        || a.starts_with("--compress-program")
                })
                // `uniq IN OUT` writes OUT.
                || name == "uniq" && positional > 1
            {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        // Shell state only; uppercase names (PATH, IFS, LD_PRELOAD) change what later commands do.
        "set" | "export" | "unset" | "local" | "declare" | "typeset" | "readonly" => {
            let flags_only = args.iter().all(|a| {
                a.starts_with('-')
                    || a.starts_with('+')
                    || matches!(*a, "pipefail" | "errexit" | "nounset" | "xtrace")
            });
            let exported = matches!(name.as_str(), "export" | "declare" | "typeset" | "readonly")
                && (name == "export" || args.iter().any(|a| a.starts_with('-') && a.contains('x')));
            let names_only = name != "set"
                && args
                    .iter()
                    .all(|a| a.starts_with('-') || harmless_variable(a, exported));
            if flags_only || names_only {
                Category::Safe
            } else {
                Category::Unknown
            }
        }
        // Runs one command per input line; the input is unknown, so only pure readers.
        "xargs" => {
            let mut rest = args;
            while let Some(first) = rest.first().filter(|a| a.starts_with('-')) {
                rest = match *first {
                    "-I" | "-n" | "-P" | "-L" | "-d" | "-E" | "-s" | "-a" => {
                        rest.get(2..).unwrap_or(&[])
                    }
                    _ => &rest[1..],
                };
            }
            match rest.first() {
                None => Category::Safe,
                Some(program)
                    if PURE.contains(&shell::program_name(program).as_str())
                        && classify_words(rest) == Category::Safe =>
                {
                    Category::Safe
                }
                Some(_) => Category::Unknown,
            }
        }
        "command" if args.len() >= 2 && matches!(args[0], "-v" | "-V") => Category::Safe,
        // `timeout [options] DURATION COMMAND...` is as safe as the command it runs.
        "timeout" => {
            let mut rest = args;
            while let Some(first) = rest.first().filter(|a| a.starts_with('-')) {
                rest = match *first {
                    "-s" | "-k" | "--signal" | "--kill-after" => rest.get(2..).unwrap_or(&[]),
                    _ => &rest[1..],
                };
            }
            match rest.split_first() {
                Some((_, command)) if !command.is_empty() => classify_words(command),
                _ => Category::Unknown,
            }
        }
        "sed" => {
            if sed_prints_only(args) {
                Category::Safe
            } else {
                Category::Unknown
            }
        }
        "hostname" | "date" if args.is_empty() || args.iter().all(|a| a.starts_with('+')) => {
            Category::Safe
        }
        "rg" => {
            if args.iter().take_while(|a| **a != "--").any(|a| {
                let name = a.split('=').next().unwrap_or(a);
                matches!(name, "--pre" | "--pre-glob" | "--hostname-bin")
            }) {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "tree" => {
            if args.iter().any(|a| {
                a.starts_with("--output")
                    || (a.starts_with('-') && !a.starts_with("--") && a.contains(['o', 'H', 'R']))
            }) {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "find" => {
            if args.iter().any(|a| {
                matches!(
                    *a,
                    "-delete" | "-exec" | "-execdir" | "-ok" | "-okdir" | "-fls"
                ) || a.starts_with("-fprint")
            }) {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "go" | "cargo" | "rustc" | "javac" | "tsc" | "make" | "cmake" | "ninja" | "bazel"
        | "meson" | "gradle" | "mvn" => classify_build(&name, args),
        "dotnet" => match args.first() {
            Some(&("test" | "build" | "run")) if !args.iter().any(|a| a.starts_with("--force")) => {
                Category::Build
            }
            _ => Category::Unknown,
        },
        "pytest" | "gofmt" | "rustfmt" | "jest" | "vitest" | "mocha" | "eslint" | "prettier"
        | "ruff" | "mypy" => {
            if args.iter().any(|a| {
                a.starts_with("--basetemp")
                    || a.contains("cpuprofile")
                    || *a == "--write"
                    || *a == "-w"
                    || *a == "--fix"
            }) {
                Category::Unknown
            } else {
                Category::Build
            }
        }
        "python" | "python3" | "py" => match args {
            [
                "-m",
                "pytest" | "unittest" | "compileall" | "py_compile" | "mypy" | "ruff",
                ..,
            ] => Category::Build,
            ["--version" | "-V"] => Category::Safe,
            _ => Category::Unknown,
        },
        "node" | "rustup" | "npx" if matches!(args, ["--version" | "-v"]) => Category::Safe,
        "npm" | "yarn" | "pnpm" | "bun" | "pip" | "pip3" | "gem" | "composer" | "nuget" | "apt"
        | "apt-get" | "yum" | "dnf" | "brew" | "choco" | "winget" | "pacman" | "zypper"
        | "snap" | "flatpak" => classify_package(&name, args),
        "docker" | "podman" | "nerdctl" => classify_docker(args),
        "certutil" if matches!(args, [flag, _, ..] if flag.eq_ignore_ascii_case("-hashfile")) => {
            Category::Safe
        }
        "curl" | "wget" => classify_fetch(&name, args),
        _ => Category::Unknown,
    }
}

/// `sed` that only prints: no in-place editing, no script file, and scripts made of
/// address/print commands or substitutions without the `w` and `e` flags (the
/// commands that write files or run programs).
pub(crate) fn sed_prints_only(args: &[&str]) -> bool {
    let mut scripts = Vec::new();
    let mut iter = args.iter();
    let mut positional = Vec::new();
    while let Some(arg) = iter.next() {
        match *arg {
            "-n" | "--quiet" | "--silent" | "-E" | "-r" | "--regexp-extended" | "-s"
            | "--separate" | "-u" | "--unbuffered" | "-z" | "--null-data" | "--posix" => {}
            "-e" | "--expression" => match iter.next() {
                Some(script) => scripts.push(*script),
                None => return false,
            },
            arg if arg.starts_with("--expression=") => {
                scripts.push(&arg["--expression=".len()..]);
            }
            arg if arg.starts_with('-') && arg != "-" => return false,
            arg => positional.push(arg),
        }
    }
    if scripts.is_empty() {
        match positional.first() {
            Some(script) => scripts.push(script),
            None => return false,
        }
    }
    !scripts.is_empty() && scripts.iter().all(|script| sed_script_prints_only(script))
}

fn sed_script_prints_only(script: &str) -> bool {
    let script = script.trim_matches(['"', '\'']);
    if script.contains([';', '\n', '{', '}']) {
        return false;
    }
    // Addresses (`10`, `$`, `10,20`, `/re/`), then the command.
    let command = script
        .trim_start_matches(|c: char| c.is_ascii_digit() || matches!(c, ',' | '$' | ' ' | '!'));
    if command.is_empty() {
        return false;
    }
    if command.starts_with('/') {
        // `/regex/p` and `/a/,/b/p`.
        return command.ends_with('p') && !command.contains(" w") && !command.contains(" e");
    }
    match command.chars().next() {
        Some('p' | 'd' | '=' | 'q' | 'Q') => command.len() == 1,
        Some('s') => {
            let mut chars = command.chars().skip(1);
            let Some(delimiter) = chars.next() else {
                return false;
            };
            let rest: String = chars.collect();
            let mut parts = Vec::new();
            let mut current = String::new();
            let mut escaped = false;
            for c in rest.chars() {
                if escaped {
                    current.push(c);
                    escaped = false;
                } else if c == '\\' {
                    current.push(c);
                    escaped = true;
                } else if c == delimiter {
                    parts.push(std::mem::take(&mut current));
                } else {
                    current.push(c);
                }
            }
            parts.len() == 2
                && current
                    .chars()
                    .all(|flag| matches!(flag, 'g' | 'p' | 'i' | 'I' | '0'..='9'))
        }
        _ => false,
    }
}

pub(crate) fn git_subcommand<'a>(args: &'a [&'a str]) -> Option<(&'a str, &'a [&'a str])> {
    let mut rest = args;
    while let Some(first) = rest.first().filter(|a| a.starts_with('-')) {
        match *first {
            "-C" => rest = rest.get(2..)?,
            "--no-pager" | "-P" | "--no-optional-locks" | "--no-replace-objects" => {
                rest = &rest[1..]
            }
            _ => return None,
        }
    }
    let (sub, rest) = rest.split_first()?;
    Some((sub, rest))
}

const GIT_READ_ONLY: &[&str] = &[
    "status",
    "log",
    "diff",
    "show",
    "ls-files",
    "ls-tree",
    "rev-parse",
    "rev-list",
    "describe",
    "blame",
    "shortlog",
    "whatchanged",
    "cherry",
    "merge-base",
    "for-each-ref",
    "cat-file",
    "count-objects",
    "range-diff",
    "show-ref",
    "name-rev",
    "check-ignore",
    "check-attr",
    "version",
    "show-branch",
];

pub(crate) fn classify_git(args: &[&str]) -> Category {
    if matches!(args, ["--version" | "--help"]) {
        return Category::Safe;
    }
    let Some((sub, rest)) = git_subcommand(args) else {
        return Category::Unknown;
    };
    // An external diff tool, pager or output file runs or writes something.
    if rest.iter().any(|a| {
        a.starts_with("--output")
            || a.starts_with("--ext-diff")
            || a.starts_with("--textconv")
            || a.starts_with("--exec")
    }) {
        return Category::Unknown;
    }
    let first = rest.first().copied();
    match sub {
        "grep" => {
            if rest
                .iter()
                .take_while(|a| **a != "--")
                .any(|a| a.starts_with("-O") || a.starts_with("--open-files-in-pager"))
            {
                Category::Unknown
            } else {
                Category::Safe
            }
        }
        "branch" => {
            let listing = rest.is_empty()
                || rest.iter().all(|a| {
                    matches!(
                        *a,
                        "-a" | "--all"
                            | "-r"
                            | "--remotes"
                            | "-v"
                            | "-vv"
                            | "--verbose"
                            | "-l"
                            | "--list"
                            | "--show-current"
                            | "--merged"
                            | "--no-merged"
                            | "--contains"
                    ) || a.starts_with("--format")
                        || a.starts_with("--sort")
                });
            if listing {
                Category::Safe
            } else {
                Category::Git
            }
        }
        "tag"
            if rest.is_empty()
                || (has(rest, &["-l", "--list"])
                    && !has(
                        rest,
                        &[
                            "-d", "--delete", "-a", "-s", "-f", "--force", "-m", "-F", "-u",
                        ],
                    )) =>
        {
            Category::Safe
        }
        "remote"
            if rest.is_empty()
                || rest.iter().all(|a| matches!(*a, "-v" | "--verbose"))
                || first == Some("get-url") =>
        {
            Category::Safe
        }
        "stash" if matches!(first, Some("list" | "show")) => Category::Safe,
        "config"
            if has(
                rest,
                &["--get", "--get-all", "--get-regexp", "--list", "-l"],
            ) && !has(
                rest,
                &[
                    "--unset",
                    "--unset-all",
                    "--add",
                    "--replace-all",
                    "--rename-section",
                    "--remove-section",
                    "-e",
                    "--edit",
                ],
            ) =>
        {
            Category::Safe
        }
        "worktree" if first == Some("list") => Category::Safe,
        "submodule" if first == Some("status") => Category::Safe,
        "reflog" if first.is_none_or(|a| a == "show" || a.starts_with('-')) => Category::Safe,
        "notes" if matches!(first, Some("show" | "list")) => Category::Safe,
        sub if GIT_READ_ONLY.contains(&sub) => Category::Safe,
        _ => Category::Git,
    }
}

fn classify_build(name: &str, args: &[&str]) -> Category {
    let Some(sub) = args.first() else {
        return if name == "make" || name == "ninja" {
            Category::Build
        } else {
            Category::Unknown
        };
    };
    if name == "go" {
        if args.iter().any(|a| {
            a.starts_with("-exec") || a.starts_with("-toolexec") || a.starts_with("--exec")
        }) {
            return Category::Unknown;
        }
        match *sub {
            "version" => return Category::Safe,
            "env" => {
                return if has(args, &["-w", "-u"]) {
                    Category::Unknown
                } else {
                    Category::Safe
                };
            }
            "list" => return Category::Build,
            _ => {}
        }
    }
    if matches!(name, "cargo" | "rustc") && matches!(*sub, "--version" | "-V") {
        return Category::Safe;
    }
    // Gradle takes several tasks, each maybe module-qualified (`:app:test`).
    if name == "gradle" {
        let tasks = args.iter().filter(|a| !a.starts_with('-'));
        let known = tasks.clone().all(|task| {
            matches!(
                task.rsplit(':').next().unwrap_or(task),
                "build" | "test" | "check" | "assemble" | "compileJava" | "compileKotlin"
            )
        });
        return if known && tasks.count() > 0 && !has(args, &["--init-script", "-I"]) {
            Category::Build
        } else {
            Category::Unknown
        };
    }
    match *sub {
        "build" | "test" | "bench" | "compile" | "lint" | "vet" | "fmt" | "check" | "clippy"
        | "doc" | "metadata" | "tree" => Category::Build,
        _ if name == "make" && !sub.starts_with('-') => Category::Build,
        _ => Category::Unknown,
    }
}

const PACKAGE_SCRIPTS: &[&str] = &[
    "test",
    "build",
    "lint",
    "check",
    "typecheck",
    "compile",
    "bench",
];

fn package_script(script: &str) -> bool {
    let base = script.split(':').next().unwrap_or(script);
    PACKAGE_SCRIPTS.contains(&base)
        && script
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "_.:-".contains(c))
}

fn classify_package(name: &str, args: &[&str]) -> Category {
    let Some((sub, rest)) = args.split_first() else {
        return Category::Install;
    };
    if matches!(name, "npm" | "pnpm" | "yarn" | "bun") {
        let script = match *sub {
            "run" | "run-script" => rest
                .first()
                .filter(|s| package_script(s))
                .map(|_| &rest[1..]),
            "test" | "t" | "tst" => Some(rest),
            s if name != "npm" && package_script(s) => Some(rest),
            _ => None,
        };
        if let Some(rest) = script {
            let flags_ok = rest
                .iter()
                .take_while(|a| **a != "--")
                .all(|a| !a.starts_with('-') || *a == "--if-present" || *a == "--silent");
            if flags_ok {
                return Category::Build;
            }
        }
    }
    if matches!(*sub, "--version" | "-v" | "--help") && rest.is_empty() {
        return Category::Safe;
    }
    match *sub {
        "list" | "ls" | "info" | "show" | "view" | "search" | "outdated" | "why" | "explain"
        | "freeze" | "doctor"
            if name != "yarn" =>
        {
            Category::Safe
        }
        "audit" if !rest.iter().any(|a| a.contains("fix")) => Category::Safe,
        "config" if matches!(rest.first(), Some(&("list" | "get"))) => Category::Safe,
        _ => Category::Install,
    }
}

fn classify_docker(args: &[&str]) -> Category {
    match args {
        [sub, ..]
            if matches!(
                *sub,
                "ps" | "images" | "inspect" | "logs" | "stats" | "info" | "version"
            ) =>
        {
            Category::Safe
        }
        [a, b, ..]
            if matches!(
                (*a, *b),
                ("network", "ls")
                    | ("network", "inspect")
                    | ("volume", "ls")
                    | ("volume", "inspect")
                    | ("compose", "ps")
                    | ("compose", "logs")
                    | ("context", "ls")
                    | ("system", "info")
                    | ("system", "df")
                    | ("image", "ls")
                    | ("image", "inspect")
                    | ("container", "ls")
                    | ("container", "inspect")
                    | ("container", "logs")
            ) =>
        {
            Category::Safe
        }
        _ => Category::Unknown,
    }
}

/// curl/wget that only GET to stdout.
fn classify_fetch(name: &str, args: &[&str]) -> Category {
    let mut to_stdout = name == "curl";
    let mut index = 0;
    while index < args.len() {
        let arg = args[index];
        let (flag, inline) = match arg.split_once('=') {
            Some((flag, value)) if flag.starts_with("--") => (flag, Some(value)),
            _ => (arg, None),
        };
        let mut value = || {
            inline.map(str::to_owned).or_else(|| {
                index += 1;
                args.get(index).map(|v| (*v).to_owned())
            })
        };
        match flag {
            "-s"
            | "-S"
            | "-sS"
            | "-f"
            | "-L"
            | "-sL"
            | "-fsSL"
            | "-sSL"
            | "-I"
            | "-i"
            | "-v"
            | "--silent"
            | "--show-error"
            | "--fail"
            | "--location"
            | "--head"
            | "--include"
            | "--compressed"
            | "-q"
            | "--quiet"
            | "--no-progress-meter" => {}
            "-X" | "--request" => {
                if !value().is_some_and(|v| {
                    v.eq_ignore_ascii_case("get") || v.eq_ignore_ascii_case("head")
                }) {
                    return Category::Unknown;
                }
            }
            "-H" | "--header" | "-A" | "--user-agent" | "-m" | "--max-time" => {
                if value().is_none_or(|v| v.starts_with('@')) {
                    return Category::Unknown;
                }
            }
            "-o" | "-O" | "--output" | "--output-document" | "-qO" => {
                let target = if flag == "-O" && name == "curl" {
                    None
                } else {
                    value()
                };
                if target.as_deref() != Some("-") {
                    return Category::Unknown;
                }
                to_stdout = true;
            }
            "-qO-" | "-O-" => to_stdout = true,
            flag if flag.starts_with('-') => return Category::Unknown,
            _ => {}
        }
        index += 1;
    }
    if to_stdout {
        Category::Safe
    } else {
        Category::Unknown
    }
}

/// Routine command groups a remembered approval may cover ("git" covers add/commit/push...).
pub struct Group {
    pub id: &'static str,
    pub label: &'static str,
    programs: &'static [&'static str],
    subcommands: &'static [&'static str],
    refused: &'static [&'static str],
    refused_short: &'static str,
}

pub const GROUPS: &[Group] = &[
    Group {
        id: "git",
        label: "git routine operations (add/commit/push/pull/switch...; not --force, reset, clean or checkout)",
        programs: &["git"],
        subcommands: &[
            "init",
            "add",
            "commit",
            "push",
            "pull",
            "fetch",
            "switch",
            "merge",
            "rebase",
            "stash",
            "branch",
            "tag",
            "cherry-pick",
            "mv",
            "status",
            "log",
            "diff",
            "show",
        ],
        refused: &[
            "--interactive",
            "--patch",
            "--edit",
            "--force",
            "--force-with-lease",
            "--force-if-includes",
            "--delete",
            "--mirror",
            "--prune",
            "--receive-pack",
            "--upload-pack",
            "--exec",
            "--discard-changes",
            "--edit-todo",
        ],
        refused_short: "ipefdDMCxc",
    },
    Group {
        id: "cargo",
        label: "cargo routine operations (build/test/check/run/fmt/clippy/add...; not install, publish or clean)",
        programs: &["cargo"],
        subcommands: &[
            "build", "test", "check", "run", "fmt", "clippy", "add", "update", "doc", "bench",
            "tree", "metadata",
        ],
        refused: &["--force"],
        refused_short: "",
    },
    Group {
        id: "npm",
        label: "npm/pnpm/yarn routine operations (install/run/test/build...; not publish, global installs or exec)",
        programs: &["npm", "pnpm", "yarn"],
        subcommands: &[
            "install",
            "i",
            "ci",
            "run",
            "test",
            "init",
            "uninstall",
            "remove",
            "build",
            "ls",
            "list",
            "outdated",
            "audit",
        ],
        refused: &["--force", "--global", "--prefix", "--location"],
        refused_short: "g",
    },
    Group {
        id: "go",
        label: "go routine operations (build/test/vet/run/mod/get...; not install, clean or env -w)",
        programs: &["go"],
        subcommands: &[
            "build", "test", "vet", "run", "fmt", "mod", "get", "generate", "list", "doc",
            "version",
        ],
        refused: &["-exec", "-toolexec"],
        refused_short: "",
    },
    Group {
        id: "dotnet",
        label: "dotnet routine operations (new/sln/add/restore/build/test/run...; not publish, tool or --force)",
        programs: &["dotnet"],
        subcommands: &[
            "new", "sln", "add", "remove", "restore", "build", "test", "run", "format", "clean",
            "pack", "list",
        ],
        refused: &["--force"],
        refused_short: "",
    },
];

pub fn group(id: &str) -> Option<&'static Group> {
    GROUPS.iter().find(|group| group.id == id)
}

impl Group {
    /// Whether one simple command's words are a routine operation of this group.
    pub fn covers(&self, words: &[&str]) -> bool {
        let Some(exe) = words.first() else {
            return false;
        };
        if exe.contains(['/', '\\']) || !self.programs.contains(&shell::program_name(exe).as_str())
        {
            return false;
        }
        let args = &words[1..];
        let (sub, rest) = if self.id == "git" {
            match git_subcommand(args) {
                Some(found) => found,
                None => return false,
            }
        } else {
            match args.split_first() {
                Some((sub, rest)) if !sub.starts_with('-') => (*sub, rest),
                _ => return false,
            }
        };
        if !self.subcommands.contains(&sub) {
            return false;
        }
        for arg in rest.iter().take_while(|a| **a != "--") {
            if let Some(long) = arg.strip_prefix("--") {
                let name = long.split('=').next().unwrap_or(long);
                if name.len() >= 2
                    && self.refused.iter().any(|r| {
                        r.trim_start_matches('-').starts_with(name)
                            || name.starts_with(r.trim_start_matches('-'))
                    })
                {
                    return false;
                }
            } else if let Some(short) = arg.strip_prefix('-') {
                if self.refused.contains(arg) {
                    return false;
                }
                // git commit -m/-F values are free text, not flags.
                if !short.is_empty()
                    && short.chars().all(|c| c.is_ascii_alphabetic())
                    && short.chars().any(|c| self.refused_short.contains(c))
                {
                    if self.id == "git" && sub == "commit" && !short.contains(['c', 'e', 'p', 'i'])
                    {
                        continue;
                    }
                    return false;
                }
            }
        }
        // `git push origin +main` is a force push.
        !(self.id == "git" && sub == "push" && rest.iter().any(|a| a.starts_with('+')))
    }
}

/// The group that covers every simple command of `command`, if one does.
pub fn line_group(command: &str, dialect: Dialect) -> Option<&'static Group> {
    let line = shell::parse(command, dialect);
    if !line.opaque.is_empty() || safety::catastrophic(command).is_some() {
        return None;
    }
    let commands: Vec<&Simple> = line.commands().collect();
    let first = commands.first()?;
    let program = shell::program_name(first.words.first()?.text.as_str());
    let group = GROUPS
        .iter()
        .find(|g| g.programs.contains(&program.as_str()))?;
    commands
        .iter()
        .all(|simple| {
            simple.redirects.is_empty()
                && simple.words.iter().all(|w| !w.dynamic)
                && group.covers(&simple.argv())
        })
        .then_some(group)
}

/// The paths of a line that only deletes files named literally:
/// POSIX `rm`/`unlink`, PowerShell `Remove-Item`/`del`/`rm`, joined by `&&` or `;`, and
/// possibly read-only commands around them (`rm a && ls`). `None` for anything else:
/// recursion, other options, globs, variables, `~`, command substitution, redirects, a
/// pipe into the deletion, a change of directory, or any command that is not read-only.
pub fn deletes(command: &str, dialect: Dialect) -> Option<Vec<String>> {
    if safety::catastrophic(command).is_some() {
        return None;
    }
    let line = shell::parse(command, dialect);
    if !line.opaque.is_empty() || !line.substitutions.is_empty() || line.pipelines.is_empty() {
        return None;
    }
    let mut paths = Vec::new();
    for pipeline in &line.pipelines {
        if pipeline.iter().all(|simple| inert(simple, dialect)) {
            continue;
        }
        let [simple] = pipeline.as_slice() else {
            return None;
        };
        if !simple.redirects.is_empty() || !simple.heredocs.is_empty() {
            return None;
        }
        let (program, args) = simple.words.split_first()?;
        if program.dynamic || program.glob || program.block {
            return None;
        }
        let name = shell::program_name(&program.text);
        let found = match dialect {
            Dialect::Posix => posix_deletes(&name, args)?,
            Dialect::PowerShell => powershell_deletes(&name, args)?,
            Dialect::Cmd => return None,
        };
        paths.extend(found);
    }
    (!paths.is_empty()).then_some(paths)
}

/// A read-only command that leaves the directory alone, so the deletions next to it
/// still resolve their paths against the work tree.
fn inert(simple: &Simple, dialect: Dialect) -> bool {
    let moves = simple.words.first().is_none_or(|program| {
        matches!(
            shell::program_name(&program.text).as_str(),
            "cd" | "pushd"
                | "popd"
                | "chdir"
                | "set-location"
                | "sl"
                | "push-location"
                | "pop-location"
        )
    });
    !moves && classify_simple(simple, dialect) == Category::Safe && !fetches(simple)
}

/// Whether some command of the line deletes files (`rm`, `unlink`, `Remove-Item` and its
/// aliases, `git rm`): for the hint that a plain `rm` may run without approval.
pub fn deletes_files(command: &str, dialect: Dialect) -> bool {
    shell::parse(command, dialect).commands().any(|simple| {
        let words = simple.argv();
        let Some(program) = words.first() else {
            return false;
        };
        match shell::program_name(program).as_str() {
            "rm" | "unlink" | "remove-item" | "del" | "erase" | "ri" => true,
            "git" => git_subcommand(&words[1..]).is_some_and(|(sub, _)| sub == "rm"),
            _ => false,
        }
    })
}

/// A word naming one file as written: no expansion, wildcard, `~` or escape left.
fn literal_path(word: &Word) -> Option<String> {
    let text = word.text.as_str();
    let plain = !word.dynamic
        && !word.glob
        && !word.block
        && !text.is_empty()
        && !text.contains([
            '*', '?', '[', ']', '$', '~', '`', ',', '{', '}', '(', ')', '@',
        ])
        && !text.chars().any(char::is_control);
    plain.then(|| text.to_owned())
}

fn posix_deletes(name: &str, args: &[Word]) -> Option<Vec<String>> {
    let unlink = match name {
        "rm" => false,
        "unlink" => true,
        _ => return None,
    };
    let mut paths = Vec::new();
    let mut options = true;
    for word in args {
        if options && word.text == "--" {
            options = false;
            continue;
        }
        if options && word.text.starts_with('-') && word.text != "-" {
            if word.text == "-f" && !unlink {
                continue;
            }
            return None;
        }
        paths.push(literal_path(word)?);
    }
    if paths.is_empty() || (unlink && paths.len() != 1) {
        return None;
    }
    Some(paths)
}

fn powershell_deletes(name: &str, args: &[Word]) -> Option<Vec<String>> {
    if !matches!(name, "remove-item" | "del" | "rm") {
        return None;
    }
    let mut path = None;
    let mut iter = args.iter();
    while let Some(word) = iter.next() {
        // A quoted `'-x'` is an argument, not a parameter.
        let parameter = word.text.starts_with('-') && !word.quoted;
        let value = if parameter {
            let flag = word.text.to_ascii_lowercase();
            match flag.as_str() {
                "-force" => continue,
                "-literalpath" | "-path" => iter.next()?,
                _ => return None,
            }
        } else {
            word
        };
        // Remove-Item takes one positional path; a second one would bind elsewhere.
        if path.replace(literal_path(value)?).is_some() {
            return None;
        }
    }
    path.map(|path| vec![path])
}

/// The files a line runs as code, as written: the script of an
/// interpreter (`python x.py`, `node a.js`, `bash s.sh`, `pwsh -File a.ps1`, `cmd /c
/// x.bat`, `source s.sh`), a program named by a path (`./s.sh`, `.\x.ps1`), also inside
/// `bash -c`/`cmd /c`/`pwsh -Command` text. A relative file follows a literal `cd`
/// before it (`cd sub && python x.py` gives `sub/x.py`). Modules (`python -m`), inline
/// code (`-c`, `-e`) and words with expansions or wildcards are not files.
pub fn runs_files(command: &str, dialect: Dialect) -> Vec<String> {
    let mut files = Vec::new();
    collect_runs(command, dialect, 0, &mut files);
    files
}

fn collect_runs(command: &str, dialect: Dialect, depth: usize, files: &mut Vec<String>) {
    if depth > 4 {
        return;
    }
    let line = shell::parse(command, dialect);
    // The directory a literal `cd` moved to; `None` is the shell's starting directory.
    let mut dir: Option<String> = None;
    for simple in line.commands() {
        let Some(words) = command_words(&simple.words, dialect) else {
            continue;
        };
        let argv: Vec<String> = words.iter().map(|word| word.text.clone()).collect();
        let offset = argv.len() - shell::strip_runners(&argv).len();
        let Some((program, args)) = words[offset..].split_first() else {
            continue;
        };
        let name = shell::program_name(&program.text);
        if matches!(
            name.as_str(),
            "cd" | "chdir"
                | "pushd"
                | "popd"
                | "set-location"
                | "sl"
                | "push-location"
                | "pop-location"
        ) {
            let target = args.iter().find(|word| !word.text.starts_with('-'));
            dir = match target.and_then(literal_path) {
                Some(target) if !matches!(name.as_str(), "popd" | "pop-location") => {
                    Some(under(dir.as_deref(), &target))
                }
                _ => None,
            };
            continue;
        }
        let mut found = Vec::new();
        interpreted(&name, program, args, dialect, depth, &mut found);
        files.extend(found.iter().map(|file| under(dir.as_deref(), file)));
    }
}

/// `file` seen from the directory `dir` (relative to the starting one).
fn under(dir: Option<&str>, file: &str) -> String {
    let absolute = file.starts_with(['/', '\\']) || file.as_bytes().get(1) == Some(&b':');
    match dir {
        Some(dir) if !absolute => format!("{}/{file}", dir.trim_end_matches(['/', '\\'])),
        _ => file.to_owned(),
    }
}

/// The files `program args` runs: an interpreter's script or the program itself.
fn interpreted(
    name: &str,
    program: &Word,
    args: &[Word],
    dialect: Dialect,
    depth: usize,
    found: &mut Vec<String>,
) {
    let first = |inline: &[&str], values: &[&str], skip: &[&str]| {
        script_argument(args, inline, values, skip)
    };
    let file = match name {
        "python" | "python3" | "py" | "pythonw" => first(&["-c", "-m", "-"], &["-W", "-X"], &[]),
        _ if name
            .strip_prefix("python")
            .is_some_and(|version| version.chars().all(|c| c.is_ascii_digit() || c == '.')) =>
        {
            first(&["-c", "-m", "-"], &["-W", "-X"], &[])
        }
        "node" => first(
            &["-e", "--eval", "-p", "--print", "-"],
            &[
                "-r",
                "--require",
                "--import",
                "--loader",
                "-C",
                "--conditions",
            ],
            &[],
        ),
        "bun" => first(
            &["-e", "--eval", "-p", "--print"],
            &["-r", "--preload"],
            &["run"],
        ),
        "deno" => first(&["eval", "repl"], &[], &["run"]),
        "ruby" => first(&["-e"], &["-I", "-r"], &[]),
        "perl" => first(&["-e", "-E"], &["-I", "-M"], &[]),
        "php" => match args.iter().position(|word| word.text == "-f") {
            Some(index) => args.get(index + 1).and_then(literal_path),
            None => first(&["-r", "-a"], &["-c", "-d", "-t", "-S"], &[]),
        },
        "source" | "." => args.first().and_then(literal_path),
        "sh" | "bash" | "zsh" | "dash" | "ksh" | "ash" | "fish" | "cmd" => {
            let texts: Vec<String> = std::iter::once(&program.text)
                .chain(args.iter().map(|word| &word.text))
                .cloned()
                .collect();
            match safety::unwrap_shell(&texts) {
                Some(safety::Nested::Text(text)) => {
                    let inner = if name == "cmd" {
                        Dialect::Cmd
                    } else {
                        Dialect::Posix
                    };
                    collect_runs(&text, inner, depth + 1, found);
                    None
                }
                Some(safety::Nested::Encoded) => None,
                None if name == "cmd" => None,
                None => first(&["-s"], &["-o", "-O", "+o", "+O"], &[]),
            }
        }
        "pwsh" | "powershell" => {
            powershell_script(name, program, args, depth, found);
            None
        }
        _ => {
            let path = program.text.contains(['/', '\\'])
                || (dialect == Dialect::Cmd && {
                    let lower = program.text.to_ascii_lowercase();
                    lower.ends_with(".bat") || lower.ends_with(".cmd")
                });
            if path { literal_path(program) } else { None }
        }
    };
    found.extend(file);
}

/// The first argument that is not an option: `None` when an `inline` option (code or a
/// module instead of a file) comes first. `values` take the next word; `skip` are
/// subcommands before the file (`deno run`).
fn script_argument(
    args: &[Word],
    inline: &[&str],
    values: &[&str],
    skip: &[&str],
) -> Option<String> {
    let mut words = args.iter();
    let mut options = true;
    let mut leading = true;
    while let Some(word) = words.next() {
        let text = word.text.as_str();
        if options && inline.contains(&text) {
            return None;
        }
        if options && text == "--" {
            options = false;
            continue;
        }
        if options && values.contains(&text) {
            words.next();
            continue;
        }
        if options && text.len() > 1 && (text.starts_with('-') || text.starts_with('+')) {
            continue;
        }
        if leading && skip.contains(&text) {
            leading = false;
            continue;
        }
        return literal_path(word);
    }
    None
}

/// `pwsh -File a.ps1`, `pwsh a.ps1`, `powershell a.ps1`, and the files of `-Command` text.
fn powershell_script(
    name: &str,
    program: &Word,
    args: &[Word],
    depth: usize,
    found: &mut Vec<String>,
) {
    let mut index = 0;
    while let Some(word) = args.get(index) {
        let lower = word.text.to_ascii_lowercase();
        let Some(flag) = lower.strip_prefix('-').or_else(|| lower.strip_prefix('/')) else {
            // pwsh takes a script file here; Windows PowerShell a command, which may
            // still name a script.
            if name == "pwsh" || lower.ends_with(".ps1") {
                found.extend(literal_path(word));
                return;
            }
            break;
        };
        if flag == "f" || (flag.len() >= 2 && "file".starts_with(flag)) {
            found.extend(args.get(index + 1).and_then(literal_path));
            return;
        }
        if flag == "c" || (flag.len() >= 2 && "command".starts_with(flag)) {
            break;
        }
        let value = safety::VALUE_FLAGS
            .iter()
            .any(|value| flag.len() >= 2 && value.starts_with(flag));
        index += if value { 2 } else { 1 };
    }
    let texts: Vec<String> = std::iter::once(&program.text)
        .chain(args.iter().map(|word| &word.text))
        .cloned()
        .collect();
    if let Some(safety::Nested::Text(text)) = safety::unwrap_shell(&texts) {
        collect_runs(&text, Dialect::PowerShell, depth + 1, found);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deletions_of_literal_files_are_recognized() {
        let posix = |command| deletes(command, Dialect::Posix);
        let ps = |command| deletes(command, Dialect::PowerShell);
        let list = |paths: &[&str]| Some(paths.iter().map(|p| (*p).to_owned()).collect());
        assert_eq!(posix("rm a.txt"), list(&["a.txt"]));
        assert_eq!(posix("rm -f a b"), list(&["a", "b"]));
        assert_eq!(posix("rm -- -x"), list(&["-x"]));
        assert_eq!(posix("unlink config/old.ini"), list(&["config/old.ini"]));
        assert_eq!(
            posix("rm 'my file.txt' && rm b.txt; rm -f c"),
            list(&["my file.txt", "b.txt", "c"])
        );
        assert_eq!(ps("Remove-Item -LiteralPath a.txt"), list(&["a.txt"]));
        assert_eq!(ps("Remove-Item -Path 'a b.txt' -Force"), list(&["a b.txt"]));
        assert_eq!(ps("del a.txt"), list(&["a.txt"]));
        assert_eq!(ps("rm a.txt; del b.txt"), list(&["a.txt", "b.txt"]));
        // Read-only commands around the deletion change nothing.
        assert_eq!(
            posix("rm config/old.ini && ls config"),
            list(&["config/old.ini"])
        );
        assert_eq!(posix("ls; rm a && git status --short"), list(&["a"]));
        assert_eq!(ps("Remove-Item a.txt; Get-ChildItem"), list(&["a.txt"]));
        assert!(deletes_files("git rm a && git status", Dialect::Posix));
        assert!(deletes_files("ls && rm -rf x", Dialect::Posix));
        assert!(!deletes_files("git status && ls", Dialect::Posix));
        for command in [
            "rm -rf build",
            "rm -r build",
            "rm -R build",
            "rm --recursive build",
            "rm -fr build",
            "rm -i a",
            "rm *.log",
            "rm 'a*.log'",
            "rm a?.log",
            "rm $F",
            "rm \"$F\"",
            "rm ~/a",
            "rm a && curl x",
            "ls",
            "cd sub && rm a",
            "rm a; cd ..",
            "rm a && echo x > log",
            "rm a && python x.py",
            "cat list | rm a",
            "rm a | cat",
            "rm a > log",
            "rm $(cat list)",
            "rm `cat list`",
            "rm",
            "rm -f",
            "unlink a b",
            "find . -delete",
            "xargs rm < list",
            "git rm a",
            "sudo rm a",
            "X=1 rm a",
            "rmdir d",
            "Remove-Item a",
        ] {
            assert_eq!(posix(command), None, "{command}");
        }
        for command in [
            "Remove-Item -Recurse x",
            "Remove-Item x -Recurse -Force",
            "Remove-Item *.log",
            "Remove-Item $f",
            "Remove-Item a.txt, b.txt",
            "Remove-Item a.txt b.txt",
            "Remove-Item -Path",
            "Remove-Item ~/a",
            "Get-ChildItem x | Remove-Item",
            "Set-Location sub; rm a",
            "ri a",
        ] {
            assert_eq!(ps(command), None, "{command}");
        }
        assert_eq!(deletes("del a.txt", Dialect::Cmd), None);
    }

    fn posix(command: &str) -> Category {
        classify(command, Dialect::Posix)
    }

    #[test]
    fn reading_commands_from_real_sessions_need_no_approval() {
        for command in [
            "cd /d/repo/mt4proxy && cat CLAUDE.md; git ls-files | sed -n 80,237p; ls docs",
            "cd ~/.nuget/x && strings -n 5 a.dll | grep -iE \"journal|Log\" | sort -u | head -40",
            "cd /d/repo && timeout 100 grep -rIl \"LOG_TYPE_FULL\" Libraries mtconnector 2>/dev/null | head",
            "grep -rn \"Time\" a.cs | head -20; sed -n 1,80p c.cs",
            "sed 's/=.*/=***/' notes.txt",
            "sed -n '/fn main/,/^fn /p' src/main.rs",
            "sed -E 's#a#b#g' a.txt",
            "cygpath -w /c/Users",
            "ls -la \"$USERPROFILE/.cove\" 2>/dev/null | head -40; echo ---; ls \"$USERPROFILE/.cove/logs\" 2>/dev/null | tail -20",
            "cd \"$USERPROFILE/.cove\" && tail -c 4000 errors.log",
            "grep -n -i \"panic\" \"$USERPROFILE/.cove/errors.log\" | tail -20",
        ] {
            assert_eq!(posix(command), Category::Safe, "{command}");
        }
        for command in [
            "sed -i 's/a/b/' a.txt",
            "sed -i.bak 's/a/b/' a.txt",
            "sed 's/a/b/w out.txt' a.txt",
            "sed 's/a/b/e' a.txt",
            "sed -n '1p;w out' a.txt",
            "sed -f script.sed a.txt",
            "sed '1e rm -rf x' a.txt",
            "timeout 5 rm -rf build",
            "timeout 5",
            "env",
        ] {
            assert_ne!(posix(command), Category::Safe, "{command}");
        }
    }

    #[test]
    fn generating_commands_are_found_in_every_dialect() {
        let posix = |command| generates_files(command, Dialect::Posix);
        for command in [
            "npm install",
            "npm ci --silent",
            "pnpm add zod",
            "yarn",
            "npm run codegen",
            "npx prisma generate",
            "pnpm exec prettier --write .",
            "dotnet restore",
            "dotnet ef migrations add Init",
            "dotnet tool restore",
            "dotnet format",
            "cargo fmt",
            "cargo clippy --fix",
            "go generate ./...",
            "go mod tidy",
            "gofmt -w main.go",
            "protoc --go_out=. api.proto",
            "buf generate",
            "python -m pip install -r requirements.txt",
            "python manage.py migrate",
            "alembic upgrade head",
            "eslint --fix src",
            "black src",
            "ruff check --fix .",
            "rustfmt src/main.rs",
            "make generate",
            "cd web && npm install > /dev/null",
            "timeout 60 npm install",
        ] {
            assert!(posix(command).is_some(), "{command}");
        }
        for command in [
            "cargo test",
            "cargo build --release",
            "npm test",
            "npm run build",
            "npm run dev",
            "dotnet build --no-restore",
            "dotnet test",
            "go test ./...",
            "gofmt -l .",
            "black --check src",
            "rustfmt --check src/main.rs",
            "prettier --check .",
            "git status",
            "python -m pytest",
            "make test",
            "ls -la",
        ] {
            assert!(posix(command).is_none(), "{command}");
        }
        assert_eq!(
            posix("ls && npm install left-pad").as_deref(),
            Some("npm install left-pad")
        );
        // Script and target names match by whole words, and only the real target counts.
        for command in [
            "npm run generate",
            "npm run gen:types",
            "npm run --silent db:migrate",
            "pnpm format",
            "yarn lint:fix",
            "make gen",
            "make -C web fmt",
            "make -j 4 proto",
            "gradle :app:generateProto",
            "./gradlew spotless flywayMigrate",
            "mvn -pl core spotless:apply install",
            "just codegen",
            "npm install",
            "go generate ./...",
            "dotnet ef migrations add X",
            "cargo fmt",
            "prettier --write .",
            // Compound names.
            "npm run reformat",
            "npm run autoformat",
            "yarn lintfix",
            "npm run dbmigrate",
            "npm run gentypes",
            "make mockgen",
            "make genproto",
            "./gradlew HTMLGen",
            "npm run preinstall",
            // A generating word is not cancelled by a check elsewhere in the name.
            "npm run format-and-check",
            "npm run generate-and-check",
            "npm run gen:check",
        ] {
            assert!(posix(command).is_some(), "{command}");
        }
        for command in [
            "mvn -pl agent-core test",
            "mvn -Dtest=AgentTest -pl agents test",
            "gradle test --tests '*AgentTest*'",
            "./gradlew :legend:test",
            "make -C agents test",
            "make -f gen.mk test",
            "make test PREFIX=/tmp",
            "npm run test:agents",
            "npm run prototype",
            "npm run prefix-check",
            "npm run format:check",
            "npm run fmt -- --check",
            "pnpm check-format",
            "yarn legend",
            "just test agents",
            "npm run fmtcheck",
            "npm run check:prettier",
            "npm run prefix",
            "npm run general",
            "npm run protocol-test",
            "npm run test:AgentTest",
        ] {
            assert!(posix(command).is_none(), "{command}");
        }
        assert_eq!(
            name_words("generateProto:gen-types_v2.fix"),
            ["generate", "proto", "gen", "types", "v2", "fix"]
        );
        let ps = |command| generates_files(command, Dialect::PowerShell);
        assert!(ps("Install-Module PSReadLine").is_some());
        assert!(ps("dotnet restore; dotnet build").is_some());
        assert!(generates_files("npm.cmd install", Dialect::Cmd).is_some());
        // Every source rewrite the evidence check knows is a generating step too.
        for command in ["cargo fmt", "go fix ./...", "shfmt -w a.sh", "rustfmt a.rs"] {
            assert!(may_rewrite_sources(command, Dialect::Posix));
            assert!(posix(command).is_some(), "{command}");
        }
    }

    #[test]
    fn local_git_steps_need_no_approval_but_push_and_forcing_do() {
        let local = |command| local_git(command, Dialect::Posix);
        for command in [
            "git add -A",
            "git add src/a.rs && git commit -m 'fix: x'",
            "git commit -am wip",
            "git tag v1.2.0",
            "git tag -a v1.2.0 -m release",
            "git status && git add . && git commit -m x && git log --oneline -3",
        ] {
            assert!(local(command), "{command}");
        }
        for command in [
            "git push",
            "git commit -m x && git push origin main",
            "git push --tags",
            "git tag -d v1",
            "git tag -f v1",
            "git add -f build/out.txt",
            "git add -p",
            "git reset --hard",
            "git commit -m \"$(cat msg.txt)\"",
            "git commit -m x > log.txt",
            "git status",
            "rm -rf x && git add .",
        ] {
            assert!(!local(command), "{command}");
        }
    }

    #[test]
    fn categories_follow_the_classifier_rules() {
        assert_eq!(posix("ls -la && git status | head -5"), Category::Safe);
        assert_eq!(posix("cargo test --all"), Category::Build);
        assert_eq!(posix("npm run build"), Category::Build);
        assert_eq!(posix("npm install left-pad"), Category::Install);
        assert_eq!(posix("git commit -m x"), Category::Git);
        assert_eq!(posix("git status; cargo build"), Category::Build);
        assert_eq!(posix("echo hi > file.txt"), Category::Unknown);
        assert_eq!(posix("echo hi 2>/dev/null"), Category::Safe);
        // The substitution is judged on its own; `cat` cannot be turned into a writer.
        assert_eq!(posix("cat $(which x)"), Category::Safe);
        assert_eq!(posix("sort $(cat opts.txt) a.txt"), Category::Unknown);
        assert_eq!(posix("find . -name x -delete"), Category::Unknown);
        assert_eq!(posix("git diff --ext-diff"), Category::Unknown);
        assert_eq!(posix("rm -rf /"), Category::Dangerous);
        assert_eq!(posix("curl -s https://example.com"), Category::Safe);
        assert_eq!(posix("curl -o x https://example.com"), Category::Unknown);
        assert_eq!(
            posix(r#"certutil -hashfile "model.gguf" SHA256"#),
            Category::Safe
        );
        assert_eq!(posix("python -m pytest -q"), Category::Build);
        assert_eq!(posix("/bin/ls"), Category::Unknown);
    }

    #[test]
    fn listing_source_files_with_find_sed_sort_head_is_read_only() {
        let command = r"find /g/hytech/quote-hub/src -maxdepth 3 -type f \( -name '*.cs' -o -name '*.csproj' \) | sed 's#^/g/hytech/quote-hub/##' | sort | head -200";
        assert!(read_only(command, Dialect::Posix));
    }

    #[test]
    fn control_flow_and_substitutions_are_judged_command_by_command() {
        let posix = |c| classify(c, Dialect::Posix);
        for safe in [
            r#"cd /g/hytech/quote-hub && git ls-files 'src/QuoteHub.Sources.Crypto/**' | while read f; do wc -l "$f"; done | sort -n"#,
            r#"for f in *.rs; do cat "$f"; done"#,
            "if [ -f a ]; then cat a; else echo none; fi",
            "cat $(git ls-files '*.md')",
            "( cd src && ls )",
            "{ ls; pwd; }",
            "set -euo pipefail; ls",
            "f=README.md; wc -l \"$f\"",
            "git ls-files | xargs wc -l",
            "uniq a.txt",
        ] {
            assert_eq!(posix(safe), Category::Safe, "{safe}");
        }
        for unknown in [
            r#"for f in *; do rm "$f"; done"#,
            "echo $(touch x)",
            "sort $opts a.txt",
            "git ls-files | xargs rm",
            "uniq a.txt b.txt",
            "f() { ls; }; f",
            "case $x in a) ls;; esac",
            "PATH=/tmp:$PATH; ls",
            "$cmd -x",
        ] {
            assert_eq!(posix(unknown), Category::Unknown, "{unknown}");
        }
    }

    #[test]
    fn powershell_blocks_and_cmd_builtins() {
        let ps = |c| classify(c, Dialect::PowerShell);
        for safe in [
            "Get-ChildItem -Recurse | Where-Object { $_.Length -gt 1 } | Select-Object FullName",
            "Get-ChildItem *.cs | ForEach-Object { Get-Content $_.FullName | Measure-Object -Line }",
            "Get-ChildItem | Where-Object Name -like '*.rs'",
        ] {
            assert_eq!(ps(safe), Category::Safe, "{safe}");
        }
        for unknown in [
            "Get-ChildItem | ForEach-Object { Remove-Item $_ }",
            "Get-ChildItem | ForEach-Object Delete",
            "Get-ChildItem | Where-Object { $x = 1 }",
            "Get-ChildItem | Where-Object $filter",
            "$items.Delete()",
        ] {
            assert_eq!(ps(unknown), Category::Unknown, "{unknown}");
        }
        let cmd = |c| classify(c, Dialect::Cmd);
        for safe in ["dir /s /b src", "type a.txt | findstr foo", "echo %PATH%", "rem note"] {
            assert_eq!(cmd(safe), Category::Safe, "{safe}");
        }
        for unknown in ["del /q x", "if exist a (del a)", "sort /o out.txt in.txt", "for %f in (*) do del %f"] {
            assert_eq!(cmd(unknown), Category::Unknown, "{unknown}");
        }
    }

    #[test]
    fn the_host_dialect_is_the_shell_the_tool_runs() {
        assert_eq!(
            Dialect::host(),
            Dialect::of(crate::process::Shell::current().kind)
        );
        // A neutral line stays on the default shell; a PowerShell one goes to PowerShell when installed.
        assert_eq!(Dialect::for_command("git status"), Dialect::host());
        if crate::process::Shell::available(crate::process::ShellKind::PowerShell).is_some() {
            assert_eq!(
                Dialect::for_command("Get-ChildItem | Select-String x"),
                Dialect::PowerShell
            );
        }
    }

    #[test]
    fn toolchain_version_and_listing_flags_are_read_only() {
        for command in [
            "dotnet --list-sdks && dotnet --list-runtimes | head -30",
            "node -v && npm --version",
            "java -version",
            "python --version; go version",
            "docker version",
            "dotnet package search Pomelo.EntityFrameworkCore.MySql --take 10 2>&1 | head -60",
            "cargo search serde --limit 3",
            "gh pr view 12 --json title",
            r#"P=/c/Users/v/AppData/Local/hermes/venv/Scripts; echo "--- dir ---"; ls -la "$P" | head -30; echo; hermes --version 2>&1 | head -20; echo "exit=$?""#,
            r#"echo "$PATH" | tr ':' '\n'; for n in herness harness hermes; do printf '%-10s -> ' "$n"; command -v "$n" 2>/dev/null || echo "(not found)"; done"#,
            "export FOO=1; echo $FOO",
        ] {
            assert!(read_only(command, Dialect::Posix), "{command}");
        }
        for command in [
            "dotnet --list-sdks --install x",
            "dotnet package add x",
            "npm -v install",
            "./node -v",
            "gh pr view 12 --web",
            "hermes --version --upgrade",
            "IFS=x; ls",
            "export LD_PRELOAD=/tmp/x.so; ls",
            "export GIT_SSH_COMMAND=x",
        ] {
            assert!(!read_only(command, Dialect::Posix), "{command}");
        }
    }

    #[test]
    fn powershell_lines() {
        let ps = |c| classify(c, Dialect::PowerShell);
        assert_eq!(
            ps("Get-ChildItem -Recurse | Select-String foo"),
            Category::Safe
        );
        assert_eq!(
            ps("Get-ChildItem | Where-Object { $_.Length -gt 1 }"),
            Category::Safe
        );
        assert_eq!(ps("Set-Content x.txt hi"), Category::Unknown);
        assert_eq!(ps("dotnet test"), Category::Build);
        assert_eq!(ps("Get-Content a.txt > b.txt"), Category::Unknown);
    }

    #[test]
    fn per_language_verification_commands_are_builds() {
        let posix = |c| classify(c, Dialect::Posix);
        for command in [
            "./gradlew :app:test :libs:core:test",
            "gradle test",
            "./mvnw test -q -pl core -am",
            "mvn test -q",
            "python -m py_compile a.py",
            "bun run test",
            "pnpm test -- --findRelatedTests src/a.ts",
            "dotnet test tests/App.Tests/App.Tests.csproj",
        ] {
            assert_eq!(posix(command), Category::Build, "{command}");
        }
        assert_eq!(classify(r".\gradlew.bat test", Dialect::PowerShell), Category::Build);
        assert_eq!(posix("./gradlew publish"), Category::Unknown);
        assert_eq!(posix("./gradlew test --init-script x.gradle"), Category::Unknown);
        assert_eq!(posix("tools/gradlew test"), Category::Unknown);
    }

    #[test]
    fn read_only_and_auto_exclude_network() {
        assert!(read_only("git log -3", Dialect::Posix));
        assert!(!read_only("curl -s https://x", Dialect::Posix));
        assert!(!read_only("cargo test", Dialect::Posix));
        assert!(auto_approvable(
            "cargo test && go vet ./...",
            Dialect::Posix
        ));
        assert!(!auto_approvable("cargo install x", Dialect::Posix));
    }

    #[test]
    fn routine_groups() {
        assert_eq!(
            line_group("git add -A && git commit -m 'msg -f'", Dialect::Posix).map(|g| g.id),
            Some("git")
        );
        assert!(line_group("git push --force", Dialect::Posix).is_none());
        assert!(line_group("git push -f origin", Dialect::Posix).is_none());
        assert!(line_group("git push origin +main", Dialect::Posix).is_none());
        assert!(line_group("git reset --hard", Dialect::Posix).is_none());
        assert!(line_group("git add . && npm test", Dialect::Posix).is_none());
        assert_eq!(
            line_group("npm install", Dialect::Posix).map(|g| g.id),
            Some("npm")
        );
        assert!(line_group("npm install -g x", Dialect::Posix).is_none());
    }

    #[test]
    fn files_run_as_code_are_recognized() {
        let posix = |command| runs_files(command, Dialect::Posix);
        let ps = |command| runs_files(command, Dialect::PowerShell);
        let list = |files: &[&str]| files.iter().map(|f| (*f).to_owned()).collect::<Vec<_>>();
        for (command, file) in [
            ("python x.py", "x.py"),
            ("python -u x.py arg", "x.py"),
            ("python3 -W ignore tools/gen.py --check", "tools/gen.py"),
            ("py -3 _install.py", "_install.py"),
            ("/usr/bin/python3.11 x.py", "x.py"),
            ("node a.js", "a.js"),
            ("node -r ts-node/register a.ts", "a.ts"),
            ("deno run -A main.ts", "main.ts"),
            ("bun run x.ts", "x.ts"),
            ("ruby x.rb", "x.rb"),
            ("perl -w x.pl", "x.pl"),
            ("php -f x.php", "x.php"),
            ("bash s.sh", "s.sh"),
            ("sh -x s.sh a", "s.sh"),
            ("./s.sh", "./s.sh"),
            ("scripts/run.sh --fast", "scripts/run.sh"),
            ("source env.sh", "env.sh"),
            (
                "PYTHONIOENCODING=utf-8 timeout 60 python 'my x.py'",
                "my x.py",
            ),
            ("bash -c 'python _install.py'", "_install.py"),
            ("cd sub && python x.py", "sub/x.py"),
            ("cd /tmp/w && python /d/x.py", "/d/x.py"),
            (
                "pwsh -NoProfile -ExecutionPolicy Bypass -File a.ps1",
                "a.ps1",
            ),
            ("pwsh a.ps1", "a.ps1"),
            ("powershell -File b.ps1", "b.ps1"),
            ("cmd /c x.bat", "x.bat"),
        ] {
            assert_eq!(posix(command), list(&[file]), "{command}");
        }
        assert_eq!(ps(r".\x.ps1 -Verbose"), list(&[r".\x.ps1"]));
        assert_eq!(ps("python x.py; node y.js"), list(&["x.py", "y.js"]));
        assert_eq!(ps("Set-Location sub; python x.py"), list(&["sub/x.py"]));
        assert_eq!(ps("powershell -Command python c.py"), list(&["c.py"]));
        // Modules, inline code, stdin and unknown words are no files.
        for command in [
            "python -m pytest",
            "python -m pip install pytest",
            r#"python -c "import os""#,
            "python - <<'EOF'\nprint(1)\nEOF",
            "python",
            "node -e 'x'",
            "ruby -e 'x'",
            "perl -e 'x'",
            "php -r 'x'",
            "deno eval 'x'",
            "bash -s < s.sh",
            "python $SCRIPT",
            "python *.py",
            "cargo test",
            "ls ./src",
            "pwsh -Command Get-ChildItem",
        ] {
            assert!(posix(command).is_empty(), "{command}: {:?}", posix(command));
        }
    }
}
