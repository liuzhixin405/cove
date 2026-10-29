# cove 技能市场

这个目录是 cove 内置的技能市场，编译时嵌入程序，`/config skills marketplace` 和
`/config skills install <名字>` 默认离线使用它。

## 结构

```
marketplace/
  registry.json              技能清单
  skills/<名字>/SKILL.md      技能正文（YAML frontmatter：name、description）
```

`registry.json` 是一个数组，每项：

```json
{"name": "dockerize", "description": "一句话说明", "url": "skills/dockerize/SKILL.md", "author": "作者"}
```

`url` 可以是相对于 `registry.json` 所在位置的路径，也可以是完整的 `https://` 地址。

## 新增技能

1. 新建 `skills/<名字>/SKILL.md`，名字只用小写字母、数字和 `-`；
2. 在 `registry.json` 里加一项；
3. 在 `src/extensions/skills.rs` 的 `MARKETPLACE_SKILLS` 里加上对应文件（嵌入程序）。

## 使用其他市场

在用户配置 `~/.cove/config.json` 顶层设置 `skills_registry`：

- `https://…/registry.json`：从网络读取，相对 `url` 按该地址解析；
- 本地路径：`registry.json` 文件，或包含它的目录。

把本目录原样发布到任意静态文件服务（例如 Git 仓库的 raw 地址）即可作为远程市场。
