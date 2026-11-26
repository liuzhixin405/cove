# 0043 `-p` 下 `python -c` 等内联代码的审批

- 状态：已完成
- 日期：2026-11-24（创建）；2026-11-25（完成）
- 关联：0024（没有终端时拒绝并交回模型）、0034（auto 下可撤销的删除）、0038（被拒后运行本任务写的脚本也要批准）

## 背景

`auto` 模式下，运行项目脚本（`python x.py`）、运行模型刚用 `write` 写的脚本都不需要批准；但把同样的代码直接写在命令行里（`python -c "..."`、`python - <<'PY' ... PY`、`node -e '...'`）却要批准。`-p` 没有终端，这类调用按 0024 直接不执行，模型拿不到运行结果，只能换一种写法再试，或者改测试文件来达到同样的目的。

## 现状与问题

1. **评测现场里反复出现。** `%TEMP%\cove-eval-*\*\stderr.txt`（2026-11-23 16:25 至 2026-11-24 19:13，29 次运行）里，`未执行（没有终端可以询问）` 后面是 `python -c` 的有 32 行（`grep -h "未执行.*python -c"` 计数），另有 1 行 `python - <<'PY'`。涉及的用例有 `review-loop-fix`、`review-loop-wrong-finding`、`many-small-failures`、`unverifiable-tests`、`impact-review-commits`、`explore-then-fix`、`wide-trace-config`、`handoff-back`、`pub-swe-sympy-24562` 等 13 个。
2. **被拒之后的绕法会改动用户文件。** `cove-eval-j_rf9fuc\many-small-failures-1\stderr.txt`：
   - 第 65 行，`python - <<'PY'`（调用 `report` 里的几个函数打印结果）没有执行；
   - 第 88 行，同样内容改写成 `python -c "..."`，仍然没有执行；
   - 第 91–92 行，模型改为 `write` 覆盖 `tests/test_report.py`，把要验证的东西塞进测试文件里运行。

   `cove-eval-j_rf9fuc\review-loop-wrong-finding-1\stderr.txt` 第 75–86 行同理：一段 4 行的 `python -c` 检查 `pricing`、`report` 的返回值，没有执行。
3. **当前代码里这条路径还在。** 数据来自 0033 之后的构建，但为了确认，读当前代码：
   - `src/safety/permission/risk.rs` 的 `python_reasons`：遇到 `-c` 给出"执行内联代码（python -c）"，遇到 `-` 或没有参数给出"从标准输入执行代码（python）"；`interpreter_reasons` 对 `node -e`、`ruby -e`、`perl -e`、`php -r`、`deno eval` 给出"执行内联代码（…）"。
   - `src/safety/permission.rs` 的 `mode_default`：`auto` 下只有 `risk::reasons` 为空才放行，所以上面这些都落到 `Decision::Ask`，没有终端时变成 `Error::Unattended`。
   - here-document 的正文不参与判断：解析器只把正文存进 `Simple::heredocs`（`shell.rs` 的 `newline`），`risk` 里没有用到；`python - <<'PY'` 要批准的原因是"从标准输入执行代码"，和正文写了什么无关。
   - 单元测试 `permission::tests::inline_code_runs_in_auto_until_something_is_refused` 在改动前运行，`python -c`、`python - <<'PY'`、`node -e` 等在 `auto` 下都得到 `Ask`，复现了问题。
4. **规则本身前后不一致。** `risk.rs` 开头的说明把内联代码归为"运行前读不到内容的代码"。但 `-c` 的代码和带引号定界符的 here-document 正文就写在命令行里，审批界面和 trace 里都看得到，比 `python x.py`（看不到 `x.py` 里写了什么）更透明。真正读不到内容的是 `curl … | python -`、`cat a.py | python -`、`python -c "$(…)"` 这类，代码来自命令行以外。
5. **一个顺带的漏洞：不带引号的 here-document 里的命令替换没有被分析。** `cat <<EOF` 和 `$(rm -rf build)`、`EOF` 三行，shell 会先执行 `$(rm -rf build)` 再把结果交给 `cat`。解析器把正文当成纯文本，`risk::reasons` 只看到 `cat`，于是 `default`、`auto` 下都直接放行。本文放开 here-document 之后，这个问题会扩大到 `python - <<PY`，所以一并处理。

## 目标

- `auto` 下，本任务里还没有操作被拒时，代码完整写在命令行里的内联代码直接运行：`python -c`、`python - <<'PY'`、`python <<< '…'`、`node -e`/`-p`、`ruby -e`、`perl -e`、`php -r`、`deno eval` 等。
- 本任务里一旦有操作被拒（与 0038 同一个标记），这些内联代码重新要批准，没有终端时不执行，免得把被拒的命令改写成 `python -c "import subprocess; …"` 执行。
- 一行里除了内联代码还有别的需要批准的原因（重定向写文件、`git push`、代码来自 `$(…)` 或管道），照旧要批准。
- `default` 模式不变。
- 不带引号的 here-document 里有 `$(…)` 或反引号时要批准（修问题 5）。

## 范围

- 做：
  - `risk.rs`：给"代码在命令行里"的内联代码统一一个可识别的原因前缀；here-document 和 `<<<` 喂给解释器时算内联代码；代码词里有变量展开时不算；`perl -i` 改为"修改文件"；不带引号的 here-document 里有命令替换时给出原因；喂给 shell 的 here-document 正文按 `bash -c` 一样递归判断。
  - `shell.rs`：记录每个 here-document 的定界符有没有引号。
  - `permission.rs`：`auto` 下只有内联代码这一类原因时放行；被拒后要批准；拒绝说明和询问标题写明。
  - 文档：`docs/reference.md`、README、`evals/README.md` 第 38 行。
- 不做：
  - **扫描内联代码的内容**（比如 Python 里的 `subprocess`、`os.remove`）：和 0038 问题 3 一样，换个写法就能绕过；`auto` 对 `python x.py` 也不看脚本内容，内联代码与它同等对待。
  - **`default` 模式放开**：`default` 的定位是"会改东西的才问"，`default` 下 `python x.py` 放行，是因为项目脚本是用户自己的代码；内联代码完全由模型当场写出，等同于"运行模型写的脚本"，在 `default` 下需要用户看一眼。保持不变，理由见"决策记录"。
  - **`osascript -e`**：AppleScript 主要用来操控别的应用和系统（`tell application …`、`do shell script`），不是"运行项目代码"，继续要批准。
  - **管道喂进解释器的代码**（`cat <<'PY' | python -`、`echo '…' | python`）：要判断前一段命令输出的是不是字面文本，规则复杂、用得少；继续要批准。模型可以直接写成 `python - <<'PY'`。
  - **PowerShell here-string**（`@'…'@ | python -`）：解析器目前把 here-string 当成"无法静态分析的结构"（`shell.rs` 里 `'@'` 后跟引号的分支），正文里有引号时会解析错位；要支持得先正确解析 here-string，并处理管道喂给解释器的情况（上一条）。cove 在 Windows 上优先用 Git Bash，评测现场里的写法都是 POSIX 的。PowerShell 里的 `python -c '…'`、`node -e '…'` 走同一套判断，已经支持。
  - `bash -c '…'`、`pwsh -Command '…'`：本来就按里面的命令递归判断，不属于这里说的内联代码，不变。

## 调研与参考

- **cove 自己的先例**：0038 已经把"运行模型本任务写的脚本"定为：没被拒时放行，被拒后要批准。内联代码就是不落盘的脚本，套用同一条规则，两种写法的权限才一致，模型不会因为一种被拒而换另一种。
- **不扫描内容**：0038 问题 3 的结论同样适用。
- **here-document 的展开**：POSIX shell 中，定界符带任何引号（`<<'PY'`、`<<"PY"`、`<<\PY`）时正文按字面传递；不带引号时正文里的 `$变量`、`$(…)`、反引号会先被 shell 展开。所以：
  - 带引号：正文就是代码，算内联代码；
  - 不带引号、正文里没有 `$` 和反引号：展开不改变内容，同样算；
  - 不带引号、有 `$变量`：代码内容运行前不确定，不算内联代码，照旧要批准；
  - 不带引号、有 `$(…)` 或反引号：shell 会执行其中的命令，给出单独的原因，任何命令都要批准（问题 5）。
- **代码词里的变量展开**：`python -c "exec('$CODE')"` 的代码来自环境变量，运行前看不到，不算内联代码。`$(…)` 在双引号里时解析器已经把它当成嵌套命令分析（`curl` 等会给出自己的原因）。

## 方案

1. **原因前缀**（`risk.rs`）：内联代码统一写成 `执行内联代码（…）`，新增常量 `INLINE_CODE` 和函数 `inline_only(command, dialect) -> bool`：`reasons` 非空，且每一条都以这个前缀开头。
   - `python -c`、`node -e/-p/--eval/--print`、`bun -e`、`ruby -e`、`perl -e/-E`、`php -r`、`rscript -e`、`lua -e`、`deno eval`：沿用现有原因；
   - 解释器（`python`/`node`/`ruby`/`perl`/`php`/`bun`/`deno`/`lua`/`rscript`）从 here-document 或 `<<<` 读代码：原来的"从标准输入执行代码（X）"改为"执行内联代码（X，here-document）"；
   - 代码所在的词（`-c` 后面的词、here-document 正文、`<<<` 的词）有变量展开时，改为"执行含变量展开的代码（X）"，不带前缀；
   - `perl -i`/`-pi`/`-ni`：改为"原地修改文件（perl -i）"；
   - `osascript -e`：改为"执行 AppleScript（osascript）"。
2. **here-document 的引号**（`shell.rs`）：`Simple` 新增 `heredoc_literal: Vec<bool>`，与 `heredocs` 一一对应，定界符词带引号（`Word::quoted`）时为真。
3. **here-document 的命令替换**（`risk.rs` 的 `simple_reasons`）：不带引号的正文里有 `$(` 或反引号时，给出"包含无法静态分析的结构（here-document 里的命令替换）"。
4. **喂给 shell 的 here-document**：`bash <<'EOF'` 等原来给"从标准输入执行代码（bash）"；改为把正文按内层方言递归 `collect`，与 `bash -c` 一致。正文不是字面（不带引号且有 `$`）时保留原来的原因。
5. **放行与收紧**（`permission.rs`）：
   - `mode_default` 里，`Mode::Auto` 的 shell 命令 `risk::inline_only` 为真时 `Allow`；
   - `script_after_refusal` 扩展为"被拒之后要批准的代码"：原来的"运行本任务写的文件"（`default`、`auto`），加上 `auto` 下的 `inline_only`；
   - 询问标题：因为这条才询问时写"原因：执行内联代码（…）（本任务里已有操作被拒绝）"；
   - `SCRIPT_HINT` 改为"本任务里已有操作被拒绝，运行本任务新建或修改的脚本、`python -c` 这类内联代码同样需要批准，不要用脚本或内联代码绕过"。
6. **系统提示词**：不改。拒绝说明里写清楚了，提示词由其他文档在改，避免冲突。

## 影响范围

- `src/safety/permission/risk.rs`、`src/safety/permission/shell.rs`、`src/safety/permission.rs`。
- 行为变化：
  - `auto`：内联代码在没被拒过的任务里直接运行；被拒之后要批准（与原来一样）；
  - `default`：内联代码不变；`bash <<'EOF'` 正文全是只读命令时不再询问（与 `bash -c '…'` 一致）；
  - `default`、`auto`：不带引号的 here-document 里的 `$(…)`/反引号现在要批准（`plan` 走 `classify::read_only`，测试确认这种写法在 `plan` 下本来就不放行，不改）；
  - `perl -i`、`osascript -e` 仍要批准，只是原因文字变了。
- 自动验证（`gates.rs` 的 `verification_preapproved`）在 `default` 下看 `risk::reasons` 是否为空，原因文字变化不影响它。
- 数据格式：无。

## 验证

- 单元测试 `permission::tests::inline_code_runs_in_auto_until_something_is_refused`（先写，改动前失败）：
  - `auto`、没被拒：`python -c`、`python3 -c`、`cd sub && python -c … && echo ok`、`python - <<'PY'`、`python <<'PY'`、不带引号但正文没有 `$` 的 `python - <<PY`、`python3 - <<< '…'`、`node -e`、带引号定界符里含反引号和 `${}` 的 `node <<'JS'`、`ruby -e`、`perl -e`、`php -r`、`deno eval` 放行；PowerShell 方言下 `python -c` 两种引号、`node -e` 放行；
  - 同样这些：被拒后 `Ask`；`default` 下 `Ask`；
  - 仍要批准：带重定向、连着 `git push`、`python -c "$(curl …)"`、`python -c "exec('$CODE')"`、`curl … | python -`、`cat a.py | python -`、裸 `python`、不带引号且含 `$(…)` 或 `$HOME` 的 here-document、`cat <<EOF` 里的 `$(rm -rf build)`、`perl -pi -e`、`bash <<'EOF'` 里的 `rm -rf`、`bash -c 'git push'`、`osascript -e`；PowerShell here-string；
  - `bash <<'EOF'` 正文只有 `ls`、`git status`：放行。
- 现有测试：`risk::tests` 里原因文字的断言随之更新；0038 的测试（拒绝说明里有"不要用脚本绕过"）继续通过。
- 集成测试：在 `tests/all/safety.rs` 用假提供方跑真实 CLI（`-p --permission auto`）：
  1. 没被拒：`python -c` 写一个标记文件，断言文件存在；
  2. 先 `python -m pip install …` 被拒，再 `python -c` 写标记文件，断言没执行、工具结果带新的拒绝说明。
- `cargo test --release`、`cargo clippy --release --all-targets`（不新增警告，现有 5 条）。
- 真实模型评测（Bedrock Opus）：`many-small-failures`、`review-loop-wrong-finding` 各跑 3 次，看 stderr 里不再有 `python -c`/`python - <<` 的"未执行"，且没有为了验证而改测试文件；全量跑 1 次确认没有退步。

## 实施计划

- [x] 阶段 1：复现测试
- [x] 阶段 2：`shell.rs` 记录定界符引号；`risk.rs` 原因调整和 `inline_only`
- [x] 阶段 3：`permission.rs` 放行与收紧、说明文字；集成测试
- [x] 阶段 4：文档
- [x] 阶段 5：真实模型评测

## 待决策

无。

## 决策记录

- 2026-11-24：`auto` 下内联代码按"模型本任务写的脚本"对待：没被拒时放行，被拒后要批准。比较过的方案：
  - 维持现状：`-p` 下模型拿不到运行结果，转而改测试文件（问题 2），比运行一段看得见的代码更糟；
  - 一律放行：被拒后可以把被拒的命令写进 `python -c` 执行，绕过 0038；
  - 扫描代码内容：能被换个写法绕过（0038 问题 3）。
- 2026-11-24：`default` 不变。`default` 下 `python x.py` 放行，依据是"项目脚本是用户自己的代码"；内联代码是模型当场写的，相当于 0038 里"本任务写的脚本"，而 `default` 的用户有终端可以批准，询问的代价小；有终端时批准一次还能选"本次会话不再询问"。
- 2026-11-24：PowerShell here-string 和管道喂代码不在本次支持范围（理由见"范围"），保持要批准。
- 2026-11-24：不带引号的 here-document 里的命令替换一并修（问题 5），否则本次放开 here-document 会让 `python - <<PY` 里的 `$(…)` 在 `auto` 下直接运行。

## 验证结果

### 实现要点（与方案的差异一并写明）

- `shell.rs`：`Simple` 加 `heredoc_literal`（与 `heredocs` 一一对应）和 `here_strings`（`<<<` 的词，保留 `dynamic`）；`Parser::heredocs` 多记一个"定界符带引号"。
- `risk.rs`：
  - 新增 `INLINE_CODE`、`inline_only`、`code_flags`、`code_on_the_line`。`code_on_the_line` 在 `simple_reasons` 里紧跟 `argv_reasons` 调用，只改写这一条简单命令新增的原因：解释器的"从标准输入执行代码"在有 here-document/here-string 时改成"执行内联代码（X，here-document）"或"执行含变量展开的代码（X）"；shell 的则换成正文递归 `collect` 的结果；`-c`/`-e` 后面的代码词是 `dynamic` 时改成"执行含变量展开的代码（X）"。
  - `perl -i`/`-pi`/`-ni` 改为"原地修改文件（perl -i）"，`osascript -e` 改为"执行 AppleScript（osascript）"。
  - `simple_reasons` 开头：不带引号的 here-document 正文含 `$(` 或反引号时给出"包含无法静态分析的结构（here-document 里的命令替换）"。
- `permission.rs`：`mode_default` 在 `auto` 下 `inline_only` 时放行；`script_after_refusal` 在 `auto` 下加上 `inline_only`（`default` 只看本任务写的文件，和原来一样）；询问标题在内联代码的情况下显示原因本身；`SCRIPT_HINT` 改为"……运行本任务新建或修改的脚本、python -c 这类内联代码同样需要批准，不要用脚本绕过，也不要用内联代码绕过"（保留"不要用脚本绕过"这几个字，0038 的测试和说明不受影响）。后台 Agent 的 `BackgroundAuthorizer` 也走 `decide_after`，规则相同，没有另改。
- 方案里的 `python -c "$(curl …)"` 实际是 `Deny`：`safety::catastrophic` 把它判为"runs code fetched from the network"，比 `Ask` 更严，测试按"不放行"断言。
- `plan` 下 `cat <<EOF` 带 `$(rm -rf build)` 本来就不放行，测试里加了断言。

### 测试

- 复现：`permission::tests::inline_code_runs_in_auto_until_something_is_refused` 在改动前运行失败（`python -c "import json; …"` 在 `auto` 下得到 `Ask`）；改动后通过。覆盖"验证"一节列出的全部写法。
- `risk::tests::changes_deletions_remote_and_hidden_code_ask_with_a_reason`：原有的 `python3 <<EOF` 一条改为新原因，新增 `cat a.py | python3`、带 `$HOME` 的 here-document、`cat <<EOF` 里的反引号、`perl -pi -e`、`bash <<'EOF'` 里的 `git push`。
- 集成测试 `tests/all/safety.rs::inline_code_runs_in_auto_until_something_is_refused`：真实 CLI（`-p --permission auto`）加假提供方。没被拒时 `python -c` 写出 `ran.txt`、stderr 没有"未执行"；先 `python -m pip install …` 被拒后，同一条 `python -c` 没有执行（没有 `ran.txt`），工具结果是 "nobody can be asked" 并带"内联代码绕过"。
- 工作区里其他文件同时在改（`src/engine/progress.rs` 等编译不过），所以在 `%TEMP%\cove-0043-iso` 里验证：共用快照 `0041-par` 加上本文改的 4 个源文件（`permission.rs`、`risk.rs`、`shell.rs`、`tests/all/safety.rs`），与工作区里的这 4 个文件逐字节相同。
  - `cargo test --release`：单元测试 514 通过、6 忽略、3 失败。`session::snapshot` 的 2 个单独重跑通过（并行负载下不稳定）；`app::render::tests::helper_scripts_are_told_from_project_code` 失败在 `!write("build.bat")`，原因是这份副本放在临时目录下，测试用的 `CARGO_MANIFEST_DIR/target` 整个算作临时目录，与本文无关。
  - `cargo test --release --test all`：236 通过、6 失败；6 个（`task_deadline_cancels…`、`all_hook_events…`、`project_review_hands…`、`cancelled_shell_kills…`、`cancelled_and_timed_out_mcp…`、`files_changed_by_shell…`）用 `--test-threads=2` 单独重跑全部通过，都是超时类，与本文无关；本文和 0038 的集成测试都通过。
  - `cargo clippy --release --all-targets`：5 条警告（`console.rs` 2 条、`process.rs`、`policy.rs`、`tests/all/cli.rs` 各 1 条），都是原有的，没有新增。
- 格式：只对改过的文件跑了 `rustfmt --edition 2024 --config max_width=100 --emit stdout`；`permission.rs`、`risk.rs` 的差异都在新代码里，已采用；`shell.rs` 的差异都在别处原有代码里，没有采用；`tests/all/safety.rs` 无差异。

### 评测

2026-11-25，构建 `0.8.0+250359b4ace5`，Bedrock Opus（`model_fast=off`）：

- `many-small-failures`、`review-loop-wrong-finding` 各 `--repeat 3 --keep`（`evals/results/20261125-165758-bedrock.json`）：6/6 通过。
  - `many-small-failures` 3 次 stderr 里都没有"未执行"；工作区只改了 `report.py`，没有改测试文件。
  - `review-loop-wrong-finding` 3 次各有 1 条"未执行"，都是同一条：`printf '…def count_items…' >> pricing.py && python -c "import pricing; …"`（任务里第一次 shell 调用，之前只有一次 `read`）。它用 shell 重定向往项目源码追加内容，按本文规则带重定向的命令本来就要批准，拦下的原因是 `>> pricing.py`，不是 `python -c`，属于预期行为。模型随后改用 `edit` 完成任务，工作区只改了 `pricing.py`，没有改测试文件。
  - 没有出现 `python -c` / `python - <<` 单独被拦的"未执行"。
- 全量 29 题（`evals/results/20261125-164807-bedrock.json`，对比 `20261124-190241`）：29/29 通过（`impact-review-commits` 首次判定失败是判定误判，修正判定后重新判定通过，见 0044），没有退步。

## 遗留

- PowerShell here-string（`@'…'@ | python -`）和管道喂给解释器的字面代码（`cat <<'PY' | python -`）仍要批准，理由见"范围"。
- 内联代码的内容不扫描，`python -c "import shutil; shutil.rmtree(…)"` 在 `auto`、没被拒的任务里会直接运行，这和 `auto` 下运行模型写的脚本一致；有终端时想审查可以用 `default`。
- 0038 的遗留（拒绝规则和灾难命令的 `Deny` 不置位"被拒"标记）同样适用于内联代码。
