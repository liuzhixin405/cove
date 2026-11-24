# 0038 操作被拒后，运行模型本任务里写的脚本也要批准

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-24（完成）
- 关联：0024（没有终端时拒绝并交回模型）、0037（发现这个问题的评测）

## 背景

`auto` 模式下，运行项目里的脚本（`python x.py`）不需要批准。模型可以先用 `write` 写一个脚本，再运行它。这样一来，一条被拒绝的命令（安装软件、改系统、联网写入），只要包进脚本里就能执行，权限判断形同虚设。

## 现状与问题

1. **评测里实际发生了。** `evals/results/20261124-164305-deepseek.json`，用例 `pub-swe-sympy-24562` 第 2 次，用的是当前构建 `0.7.0+39e48059b2d4`：
   - `python -m pip install pytest -q` 被拒绝，拒绝说明写着"Do not retry it or an equivalent command"；
   - 模型先尝试 `python -m pip install pytest`（同样被拒）；
   - 然后用 `write` 创建 `_install_pytest.py`，内容是 `subprocess.check_call([sys.executable, '-m', 'pip', 'install', 'pytest'])`；
   - 接着执行 `python _install_pytest.py`，没有询问就运行了；
   - pytest 9.2.0、pluggy 1.7.0、iniconfig 2.3.1 被装进了全局 Python（`site-packages` 里的文件时间为 16:50:50–16:50:55）；
   - 最后模型用 `rm` 删掉了这个脚本。
2. **原因**：`risk::reasons` 只分析命令行本身（`src/safety/permission/risk.rs:1-5`："running project scripts … runs without asking"）。`python 文件` 被当作运行项目脚本，脚本里的内容不在分析范围内。
3. **只看脚本内容挡不住**：动态语言可以用任意方式拼出命令，按关键字扫描（`pip`、`subprocess`）很容易被换个写法绕过。

## 目标

- 本任务里一旦有操作因为需要批准而被拒，或者用户回答了"拒绝"，之后运行本任务里模型新建或改过的脚本，也要批准。没有终端时同样按 0024 的规则拒绝，交回模型。
- 没被拒过时，行为不变："写一个复现脚本再运行"仍然不用批准。
- 拒绝说明里写明：脚本也不会放行。

## 范围

- 做：
  - 记录本任务里"有操作被拒绝"的状态；
  - 识别"运行一个文件"的命令：解释器加文件，`python x.py`、`node x.js`、`bash x.sh`、`pwsh x.ps1`、`./x.sh` 等；
  - 被运行的文件是本任务里经文件工具创建或修改过的，就要批准；
  - 更新拒绝说明的文字；
  - 加测试。
- 不做：
  - 扫描脚本内容：理由见问题 3。
  - 用 shell 写的脚本（比如 `echo ... > x.py`）：重定向写文件本来就需要批准（`risk.rs` 的"写入文件（重定向到 …）"），被拒之后进不到这一步。
  - 项目里原有的、本任务没改过的脚本：它们是用户自己的代码，照旧放行。
  - 评测环境的隔离：评测框架的事，见"遗留"。

## 调研与参考

- 现有的状态：
  - 回滚点按任务记录（0009）；
  - 文件工具改过的路径已有记录（`workspace.changed_paths()`，`src/safety/verify.rs:943` 在用）。
- 同类工具通常靠沙箱（只读文件系统、禁止联网）兜底，cove 的 shell 不是沙箱（系统提示词里写明了 "shell is not a sandbox"）。所以只能在权限判断这一层收紧。
- 取舍：只在"被拒过之后"收紧。依据是这次的实际行为：绕过发生在拒绝之后。代价是拒绝之后，正常的复现脚本也要批准；但这时没有终端可以询问，这一步本来就做不成，影响有限。

## 方案

1. **记录被拒**：`Permissions` 里加一个按任务清零的标记 `refused_this_task`，下面两种情况置位：
   - 返回 `Error::Unattended`；
   - 用户在询问时选了拒绝。

   开始新任务时清零：和回滚点 `reset_task` 同一个时机，实现时确认具体位置。
2. **识别运行文件**：在 `classify` 里加 `runs_files(command, dialect) -> Vec<String>`，返回被执行的文件路径：
   - 解释器后面第一个不以 `-` 开头的参数；
   - 跳过 `-m` 模块（那是包，不是文件），跳过 `-c` 内联代码；
   - `./x`、`bash x`、`sh x` 这类写法也算。
3. **判断**：`mode_default` 里，`Mode::Auto` 和 `Mode::Default` 下，如果同时满足下面两条，就返回 `Ask`：
   - `refused_this_task` 为真；
   - `runs_files` 里有本任务文件工具改过的路径（按 `paths::key` 比较）。
4. **拒绝说明**：`Unattended` 的说明后面追加一句："本任务里已有操作被拒绝，运行本任务新建或修改的脚本同样需要批准，不要用脚本绕过。"
5. **系统提示词**：不加新的说明。拒绝说明里本来就写了"不要重试等价命令"，这次的问题出在 cove 没有挡住，所以在 cove 里拦。

## 影响范围

- `src/safety/permission.rs`：新增状态和判断分支。
- `src/safety/permission/classify.rs`：新增 `runs_files`。
- 文件工具改过的路径要传进权限判断，或者在 `Permissions` 里能查到，实现时看 `workspace` 怎么接入。
- 行为变化：只发生在"有操作被拒之后"的同一个任务里。

## 验证

- 单元测试（`runs_files`）：
  - 识别出文件：`python x.py`、`python -u x.py arg`、`node a.js`、`bash s.sh`、`./s.sh`、`pwsh -File a.ps1`；
  - 不识别为文件：`python -m pytest`、`python -c "..."`。
- 单元测试（权限）：
  - 被拒之后，运行本任务写的脚本：要问；
  - 被拒之后，运行项目原有的脚本：放行；
  - 没被拒过：放行；
  - 新任务开始后：清零。
- 集成测试：用假提供方重现这次的顺序：
  1. `pip install` 被拒；
  2. `write _install.py`；
  3. `python _install.py`。

  断言第 3 步返回 `Unattended`，脚本没有执行（用脚本写一个标记文件来判断）。
- 评测：
  - `pub-swe-sympy-24562` 跑 3 次，确认没有再往全局环境装包。判断方法是比较 `site-packages` 的文件时间，或者给评测设置 `PIP_REQUIRE_VIRTUALENV`。
  - 全量跑 1 次，确认没有退步。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告）。

## 实施计划

- [x] 阶段 1：`runs_files` 和单元测试
- [x] 阶段 2：被拒状态、判断分支、拒绝说明
- [x] 阶段 3：集成测试、评测

## 待决策

无。

## 决策记录

- 2026-11-24：只在本任务有操作被拒之后，才收紧"运行本任务写的脚本"。三个方案的比较：
  - 一律要批准：会让 `-p` 下"写复现脚本再运行"大量失败；
  - 扫描脚本内容：能被换个写法绕过；
  - 被拒后收紧：选这个，它挡住的正是这次发生的绕过方式。
- 2026-11-24：这次装进全局 Python 的 pytest、pluggy、iniconfig 保留，不卸载。

## 验证结果

### 实现要点（与方案的差异一并写明）

- **谁持有被拒标记**：主任务和前台子 Agent 共用同一个 `Permissions`（子 Agent 的授权链是 `SerialAuthorizer` → `RemoteAuthorizer` → `Permissions`，worktree 子 Agent 也一样），所以子 Agent 里被拒，会同时收紧它自己和主任务在本任务里的后续调用。后台 Agent 和竞速方案用 `BackgroundAuthorizer`，不经过 `Permissions`，在它里面另加了一个 `refused` 标记，按这一次运行计算（每次运行新建一个 `BackgroundAuthorizer`）。只有固定模式、不带状态的 `LocalAuthorizer`（测试和内部工具在用）没有改。
- **置位**：`Permissions::ask` 里没有终端（`input` 为空，或者询问返回 `Approval` 被转成 `Unattended`）和用户回答拒绝（`Reply::No`）这两处；另外 `RemoteAuthorizer` 远程审批拒绝或过期时也置位（`note_refusal`）。方案里没写远程审批，它也是"用户拒绝"，所以加上。拒绝规则、灾难命令的 `Deny` 不置位（方案只写了需要批准的那两种，见"遗留"）。
- **清零**：`Snapshots::reset_task` 实际上没有调用方，回滚点是在 `TurnHook::task_note` 里 `begin_task` 的。被拒标记改在 `Application::execute` 里、新任务 `workspace.begin()` 之后调用 `Permissions::begin_task()`，时机相同（有新提问时；`/continue` 继续同一个任务，不清零）。没有放进 `TurnHook`，是因为不想让子 Agent 启动时把它清掉。
- **改过的路径怎么拿**：没有把 `Workspace` 塞进 `Permissions`，而是照 0034 `UNDOABLE` 的做法用 task-local：`LocalTools` 执行 shell 时，把 `workspace.changed_paths()`（本任务检查点里 `before != after` 的项，相对工作区根目录）和 shell 的起始目录交给 `permission::with_written`，包住这一次授权。这样 worktree 子 Agent 和后台 Agent 用的是各自的工作区和根目录。`changed_paths()` 里包括本任务用文件工具删掉的文件（`after` 为空），这些文件已经不存在，运行不了，不影响结果；文件工具建了又删的（`before`、`after` 都为空）不在里面。
- **比较**：命令里的文件相对 shell 起始目录（`LocalTools` 的根目录）解析，Windows 上把 Git Bash 的 `/d/x` 换成 `D:/x`，再按字面去掉 `.`、处理 `..`，最后用 `paths::key` 比较；改过的文件按工作区根目录拼成同样的键。
- **判断位置**：方案写在 `mode_default` 里，实际放在 `decide_after`（新函数，`decide` 等于 `refused = false` 的它）里规则没有命中、要落到模式默认之前：`policies.json` 的允许规则和"本次会话不再询问"仍然放行，因为那是用户自己的决定。`Permissions::decide` 传入自己的标记，`RemoteAuthorizer` 判断"要不要远程询问"时也就看得到。
- **`runs_files`**（`classify.rs`）：认 `python`/`python3`/`py`/`pythonw`/`python3.x`、`node`、`deno run`、`bun`（含 `bun run`）、`ruby`、`perl`、`php`（含 `-f`）、`bash`/`sh`/`zsh`/`dash`/`ksh`/`ash`/`fish`、`source`/`.`、`pwsh -File`、`pwsh a.ps1`、`powershell a.ps1`、`cmd /c x.bat`，以及程序名本身是路径的（`./x.sh`、`.\x.ps1`、`scripts/run.sh`；cmd 里还有直接写的 `x.bat`）。`-m`、`-c`、`-e`、`-r`（php）、`-`、`bash -s`、`deno eval` 不算文件；`-W`、`-X`、`node -r` 这类带值的选项跳过它的值。`bash -c`、`cmd /c`、`pwsh -Command` 的内层命令递归识别；前面字面写出的 `cd`/`Set-Location` 会拼到相对路径前面（`cd sub && python x.py` 得到 `sub/x.py`）。带变量或通配符的词不算（见"遗留"）。为此把 `safety::VALUE_FLAGS` 改成 `pub(crate)`。
- **说明文字**：所有 `Unattended`（`Permissions` 和后台 Agent 的）末尾都追加"本任务里已有操作被拒绝，运行本任务新建或修改的脚本同样需要批准，不要用脚本绕过"，所以给用户看的"未执行（没有终端可以询问）"那一行里也有。有终端询问时，因为这条规则才询问的，标题写"原因：运行本任务新建或修改的脚本（本任务里已有操作被拒绝）"。
- 文档：`docs/reference.md`（`-p` 一段后）和 README 的模式表下面各加了一段。

### 测试

- 单元测试 `classify::tests::files_run_as_code_are_recognized`：识别出文件的 25 种 POSIX 写法加 4 种 PowerShell 写法，包括方案里列的 `python x.py`、`python -u x.py arg`、`node a.js`、`bash s.sh`、`./s.sh`、`pwsh -File a.ps1`；不算文件的 16 种，包括 `python -m pytest`、`python -c "..."`。
- 单元测试 `permission::tests::after_a_refusal_scripts_the_task_wrote_ask_until_the_next_task`：没被拒时运行本任务写的脚本放行；`pip install` 被拒后，`python _install.py`、`python ./_install.py`、`cd sub && python gen.py`、`bash -c 'python sub/../_install.py'`、绝对路径都返回带新说明的 `Unattended`；项目原有脚本、`python -m pytest`、`cargo test` 放行；不在 shell 工具里（没有改动记录）放行；允许规则仍然放行；`begin_task` 之后放行。
- 单元测试 `a_no_from_the_user_also_makes_written_scripts_ask`：用户对 `git push` 选拒绝之后，`./x.sh` 要询问，询问标题写明原因。
- 单元测试 `files_are_compared_as_the_shell_names_them`：`./a/../x.py`、`sub\x.py`、`/d/repo/x.py` 的解析。
- 集成测试 `tests/all/safety.rs::after_a_refusal_a_script_the_task_wrote_is_not_run_either`：真实 CLI（`-p --permission auto`）加假提供方。没有拒绝时，`write _install.py` → `python _install.py` 照常运行，脚本写出了 `ran.txt`；有拒绝时按 `python -m pip install …` 被拒 → `write _install.py` → `python _install.py` 的顺序，脚本已写出但没有执行（没有 `ran.txt`），第 3 步的工具结果是 "nobody can be asked" 并带"不要用脚本绕过"，stderr 有一行 `_install.py` 的"未执行"。把 `script_after_refusal` 临时改成恒为假之后重跑，这个测试失败，说明它确实测到了这次的改动。
- `cargo test --release`：单元测试 510 个通过、6 个忽略；集成测试 238 个通过；0 失败。
- `cargo clippy --release --all-targets`：5 条警告（`console.rs` 2 条、`process.rs`、`policy.rs`、`tests/all/cli.rs` 各 1 条），都是原来就有的，没有新增。
- 格式：只对改过的文件跑了 `rustfmt --edition 2024 --config max_width=100`；`classify.rs` 本来就没有完全格式化，只采用了新增代码那几处的格式结果。

### 评测

只用 Bedrock（Opus 5.5），构建 `0.7.0+112252d6ec16`：

- `python -X utf8 evals/run.py --profile bedrock --cases pub-swe-sympy-24562 --repeat 3`：3/3 通过，平均 17 次调用、214 秒，费用 $1.28。结果在 `evals/results/20261124-181302-bedrock.json`。
  - 运行前后，全局 `site-packages` 里最新的文件都是 16:50:55 那次装的 pytest，这次没有新装包。
  - 需要说明：pytest 这时已经装在全局环境里，Opus 也没有尝试安装，所以这三次没有走到"被拒 → 写脚本"那条路径。这条路径由集成测试覆盖。
- `python -X utf8 evals/run.py --profile bedrock --repeat 1`：这是 Bedrock 的第一次全量，没有基线。结果在 `evals/results/20261124-181823-bedrock.json`，费用 $2.50。
  - 首次判定 26/29。3 个没通过的都看过现场，都是判定写错了：
    - `impact-review-commits`："行尾发生了变化……这对功能没有影响"说的是换行符，被当成了整体结论；
    - `review-loop-wrong-finding`：不认"我没有改代码来处理这条发现"这种说法；
    - `handoff-back`：拿"回到原会话后的上下文"去和"交接后的新会话"比大小。Opus 的第 1 步只调用 3 次，交接摘要和原会话差不多大，这个比较就不成立。改为和第 1 步结束时的上下文比较。
  - 修正判定，每处都用这次的真实回复补了 selftest，然后 `--rejudge`：29/29。

## 遗留

- `PIP_REQUIRE_VIRTUALENV=1` 已经做了：`evals/run.py` 的 `COVE_ENV` 给每次运行都设置了它，作为评测环境的第二道防线（0037 的遗留）。
- 拒绝规则和灾难命令（`Deny`）被拦下后不置位：由 `PolicyGuard` 在调用前直接拒绝，不经过 `Permissions::ask`。把被拒绝的命令包进脚本，同样可以绕过。要不要算进来，另开文档。
- 没有覆盖的写法：文件名来自变量或通配符（`python $F`、`python _inst*.py`）；`python -m pytest`、`pytest` 运行本任务写的测试文件或 `conftest.py`；本任务写的模块被项目原有脚本 import；`cd` 的目标不是字面值时，相对路径按起始目录解析。
