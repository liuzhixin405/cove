# 0061 默认权限模式改为 bypass

- 状态：已完成
- 日期：2026-11-25（创建）；2026-11-25（完成）
- 关联：0024（无终端时拒绝调用）；0034（auto 删除）

## 问题

实际使用中，审批提示几乎每次都被用户按 y 或 a 放行，没有真正拦下过操作；而仓库本身有 origin 可以恢复。默认的 `default` 模式只是每次多按一个键。真正需要保护的是电脑层面的危险命令，这部分由 `bypass` 里仍然生效的拒绝规则和灾难命令拦截负责。

## 改法

- `src/config/mod.rs`：没有命令行参数、配置里也没写 `permission_mode` 时，默认值由 `Mode::Default` 改为 `Mode::Bypass`。
- 想要审批的用户自己开：`--permission default`、`/mode default [save]`、配置 `permission_mode: "default"`。这些不需要改代码。
- 不改：项目配置里写 `permission_mode: bypass` 仍需信任才生效（`permission_mode` 只有 `default`/`plan` 免信任）；Plan 模式退出后回到 `Mode::Default` 的逻辑是"回到之前的模式"，不动。
- 文档：README 权限一节和 `-p` 一节、`docs/reference.md` 的模式表。

## 影响

- 工作区外写入、网络、扩展进程、`cargo install`、`git push` 以外的有副作用命令默认不再询问。`git push` 在 bypass 下是否被拒，以现有拒绝规则为准，本次没有改。
- 无终端（`-p`、后台 agent）下，以前"未执行"的调用现在会直接执行。

## 验证

- `cargo test --release`：lib 614 通过，集成测试 269 通过，0 失败（现有测试都显式指定模式或不依赖默认值）。
- clippy、rustfmt：见提交前检查。
- release 版实际运行确认默认不再弹审批：未做，待下次使用时确认。

## 验证结果

见"验证"。
