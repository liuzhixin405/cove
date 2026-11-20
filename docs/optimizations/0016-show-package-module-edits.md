# 0016 Python 包里的文件不再被当成临时脚本折叠

- 状态：已完成
- 日期：2026-11-20（创建）；2026-11-20（完成）
- 关联：[0015](0015-readme-rewrite.md)（录制 README 演示时发现）

## 背景

为 README 录制演示时，cove 修 bug 的那一次编辑没有显示出来。

## 现状与问题

- 控制台会把 agent 给自己写的临时脚本折叠成一行"N 个辅助脚本调用未展示"（`app/render.rs` 的 `scratch_call`），避免它们刷屏。
- 判断依据之一：项目里没有 Python 打包文件（`pyproject.toml`、`setup.py`、`setup.cfg`、`requirements.txt`、`Pipfile`）时，所有 `.py` 文件都算临时脚本。
- 不少 Python 项目没有这些文件（脚本仓库、教学项目、只用标准库的服务）。它们的 `read`、`edit`、`write` 都会被折叠，**用户看不到 agent 改了哪些代码**。
- 证据：演示项目 `order-service`（`order_service/` 和 `tests/` 都是带 `__init__.py` 的包，没有打包文件）。13 次工具调用里，2 次 read 和 7 次 edit 全部被折叠，其中包括修 bug 的那一次（`>` 改成 `>=`）。

## 目标

项目里正经的 Python 代码，读取和修改都显示出来；agent 随手写的临时脚本仍然折叠。

## 方案

`.py` 文件所在的目录里有 `__init__.py`（说明它是一个包的模块），就算项目代码。临时目录、`target/`、`tmp/` 这些位置照旧算临时脚本。

- 这条判断只看文件系统，不额外运行 git。
- 不在包里、又没有打包文件的根目录脚本（比如 `repro.py`）仍然折叠。这正是 agent 最常写的临时脚本。

## 影响范围

`src/app/render.rs`。行为变化：没有打包文件的 Python 项目里，包内文件的读取和编辑会正常显示。

## 验证

在 `helper_scripts_are_told_from_project_code` 里增加：包目录里的 `shop/pricing.py` 的 write 和 edit 不算临时脚本。全量 `cargo test`。

## 实施计划

- [x] 修改判断并补测试
- [x] 全量测试

## 待决策

无。

## 决策记录

- 2026-11-20：用 `__init__.py` 判断，不用"git 是否跟踪"。后者每次显示工具调用都要启动一次 git，而这段代码在界面线程上运行。（实施方）

## 验证结果

- 新增断言通过；全量 `cargo test` 通过（见 0015 的验证结果）。

## 遗留

- 没有 `__init__.py` 的命名空间包（Python 3.3+ 允许）里的文件，在没有打包文件的项目中仍会被折叠。这种项目很少见，遇到再处理。
