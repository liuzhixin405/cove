# 0025 自动验证认识 .slnx、Directory.Build.props 里的测试项目和 MTP 模式的 dotnet test

- 状态：已完成
- 日期：2026-11-23（创建）；2026-11-23（完成）
- 关联：0023（调研时发现）

## 背景

0023 的调研里发现：在本机主力项目 quote-hub 上，自动验证门禁一次都没有运行过。

## 现状与问题

quote-hub 的实际配置（只读查看）：根目录只有 `QuoteHub.slnx`；`global.json` 是 `"test": {"runner": "Microsoft.Testing.Platform"}`；测试项目的 csproj 里没有任何测试包，`IsTestProject`、`OutputType=Exe` 和 `xunit.v3.mtp-v2` 都写在 `tests/Directory.Build.props` 里，按项目名后缀（`UnitTest`/`IntegrationTest`/`SmokeTest`）生效。

三个问题层层叠着，修了前一个，后一个就会暴露：

1. **不认 `.slnx`。** `src/safety/verify.rs:133` 的 `dotnet()` 只认根目录的 `.sln`/`.csproj`。quote-hub 被当成"推断不出检查命令"，构建和测试都不跑。调研统计：`~/.cove/acceptance/` 里 quote-hub 的 29 份验收报告、206 次编辑，自动检查一次都没运行。`.slnx` 是 .NET 9.0.200 起支持的新解决方案格式，.NET 10 新建项目默认就用它。
2. **认不出写在 Directory.Build.props 里的测试项目。** `dotnet_tests`（`verify.rs:428`）只在 csproj 文本里找 `Microsoft.NET.Test.Sdk` 或 `<IsTestProject>true`。quote-hub 的 14 个测试项目一个都认不出来，任何改动都只会选到 `dotnet build <改动的项目>`，不跑测试。用 MTP 的 xunit v3、MSTest.Sdk、TUnit 也都不引用 `Microsoft.NET.Test.Sdk`。
3. **MTP 模式不接受位置参数。** `global.json` 选了 Microsoft.Testing.Platform 之后，`dotnet test --help` 显示只支持 `--project <路径>`/`--solution <路径>`，没有位置参数。cove 生成的是 `dotnet test tests/X/X.csproj`（`verify.rs:483`），会直接报参数错误，被记成"验证失败"并打回模型。

另外，文档漏写了：`docs/reference.md` 里 `done_verify_tests` 的说明没有提到早已支持的 Maven/Gradle 测试（`verify.rs:496` 的 `java_tests`）。

## 目标

- quote-hub 这类项目改了代码后，自动验证会运行，并且只跑覆盖改动的测试项目，命令格式能被 MTP 接受。
- 不影响现有的 `.sln`、VSTest 项目。

## 范围

- 做：`dotnet()` 认 `.slnx`；测试项目识别补上 MTP 测试框架和 `Directory.Build.props`；MTP 模式下用 `--project`；对应单元测试；reference 补上 `.slnx`、MTP 和 Maven/Gradle 的说明。
- 不做：
  - 不解析 MSBuild 条件表达式。`Directory.Build.props` 里的条件按"项目名以 Test 或 Tests 结尾"近似判断，见方案 2。
  - 不处理根目录有多个解决方案或项目文件的情况（`dotnet test` 会报 MSB1011）。这是原有行为，本机数据里没有遇到过。
  - 不处理 Makefile/CMake 项目。没有真实需求，另记。

## 调研与参考

- `dotnet test --help`（SDK 10.0.404，MTP 模式）：只有 `--project`、`--solution`、`--test-modules` 等选项，没有位置参数；不带参数时默认用当前目录。
- MTP 下各测试框架的标志：xunit v3 是 `xunit.v3`（MTP 包 `xunit.v3.mtp-v1/v2`），MSTest 是 `MSTest.Sdk` 或 `MSTest`，NUnit 是 `NUnit`，TUnit 是 `TUnit`，平台本身是 `Microsoft.Testing.Platform`；VSTest 时代是 `Microsoft.NET.Test.Sdk`。
- `Directory.Build.props`：MSBuild 会自动导入项目所在目录及各级上层目录里最近的一个，文件里通常用项目名之类的条件控制哪些项目受影响。

测试项目识别的取舍：

| 方案 | 结果 |
|---|---|
| 运行 `dotnet msbuild -getProperty:IsTestProject` | 准确，但每个项目要起一次进程，而且在审批前就得执行项目的 MSBuild 逻辑 |
| **文本判断：csproj 里有测试包标志；或者上层 `Directory.Build.props` 有测试包标志、项目名以 Test 或 Tests 结尾** | 不执行任何东西；quote-hub 的写法能覆盖；被误判成测试项目时，最多多跑一个项目 |

## 方案

1. `dotnet()`：根目录文件名以 `.sln`、`.slnx`、`.csproj` 结尾都算。
2. 测试项目判断 `is_test_project(root, project, text)`，满足其一即可：
   - csproj 文本里有 `Microsoft.NET.Test.Sdk`、`<IsTestProject>true`、`Microsoft.Testing.Platform`、`xunit.v3`、`MSTest.Sdk`、`"MSTest"`、`"NUnit"`、`"TUnit"`；
   - 从项目目录向上找到根目录为止，遇到的第一个 `Directory.Build.props` 里有上面任一标志，并且项目名以 `Test` 或 `Tests` 结尾（不区分大小写）。只看"含 Test"会把 `QuoteHub.TestSupport` 这类辅助库也当成测试项目，在 MTP 下对它 `dotnet test` 会报错。
3. MTP 模式：读根目录 `global.json` 的 `test.runner`，等于 `Microsoft.Testing.Platform` 时，选中的测试写成 `dotnet test --project <路径>`；整体测试仍是 `dotnet test`。
4. `docs/reference.md`：自动推断的说明里补上 `.slnx`、MTP 写法和 Maven/Gradle 测试。

## 影响范围

- 代码：只改 `src/safety/verify.rs`。
- 行为：用 `.slnx` 的项目开始自动运行验证（构建加测试，默认 300 秒超时）；MTP 项目的测试命令格式改变。其他项目不受影响。
- 数据格式：无变化。

## 验证

- 单元测试：
  - `detects_project_kinds` 增加只有 `.slnx` 的情况，期望 `dotnet build`；
  - 新增：仿照 quote-hub 的结构（`.slnx`、MTP 的 `global.json`、`tests/Directory.Build.props` 里条件引用 xunit.v3、csproj 里没有测试包），改动 `src/Lib` 时期望 `dotnet test --project tests/Lib.UnitTest/Lib.UnitTest.csproj`；改动没有测试覆盖的项目时期望 `dotnet build`；
  - 原有的 `dotnet_tests_only_the_test_projects_that_reach_the_change`（VSTest、`.sln`）保持不变。
- `cargo test --release` 全部通过；`cargo clippy --release --all-targets` 不新增警告。
- 实测：在 quote-hub 根目录调用 `test_commands`（用新增的测试函数针对真实目录打印结果，不改仓库），确认选出的命令；再手动跑其中一条，确认 MTP 能接受。

## 实施计划

- [x] 阶段 1：`dotnet()`、测试项目识别、MTP 写法
- [x] 阶段 2：单元测试
- [x] 阶段 3：reference
- [x] 阶段 4：全量测试、clippy、quote-hub 实测

## 待决策

无。

## 决策记录

- 2026-11-23：按"先修 .slnx，再处理 auto 下删除，再补评测"的顺序进行。
- 2026-11-23：测试项目用文本判断，不运行 msbuild。理由见"调研与参考"。

## 验证结果

- 新增单元测试 `slnx_and_microsoft_testing_platform_projects_are_verified` 通过。它仿照 quote-hub 的布局：只有 `.slnx`、MTP、测试框架写在 `tests/Directory.Build.props`。断言了四点：只有 `.slnx` 时选出 `dotnet build`；改 `src/Lib` 选出 `dotnet test --project tests/Lib.UnitTest/…`；`Lib.TestSupport` 不被当成测试项目；改动没有测试覆盖的项目时选出 `dotnet build`。原有的 VSTest/`.sln` 测试不变。
- 对真实的 quote-hub 只读调用（`print_checks_for_a_real_repository`，默认 ignored），改动 `src/QuoteHub.Sources.Crypto/…` 和 `src/QuoteHub.Admin/…`：
  - 修改前：`detect` 为空，不运行任何检查；
  - 修改后：`dotnet build` 加 4 个 `dotnet test --project …`（Admin 单元/集成、Console 集成、Crypto 单元），`TestSupport` 被正确排除。
- 在 quote-hub 实际运行其中一条 `dotnet test --project tests/QuoteHub.Sources.Crypto.UnitTest/…csproj`：265 个测试通过，退出码 0，MTP 接受这种写法；`git status` 前后一样（只多了被忽略的 bin/obj）。
- `cargo test --release`：405 个单元测试加 216 个集成测试通过；clippy 仍是原有 5 条；verify.rs 已按 rustfmt 格式化（只动了新增的代码）。

## 遗留

- 根目录有多个解决方案或项目文件时，`dotnet test` 会报 MSB1011。这是原有行为，没有遇到过，未处理。
- Makefile/CMake 项目仍推断不出检查命令。
- quote-hub 的集成测试需要 Docker，没有 Docker 时会被跳过（此前一次运行中是 193 个），跳过不算失败；集成测试较多时，可能超过 300 秒的默认时限，超时会记为"未验证"。
