# 📖 文档中心

欢迎查阅 cove 文档。cove 是一个带自我学习能力的 Go 语言 AI 编程助手。�?
## 用户文档

| 文档 | 说明 |
|------|------|
| [用户手册](USER_MANUAL.md) | 完整使用手册，涵盖安装、配置、命令、工具、插件、高级用�?|
| [用户手册 - 诊断系统](USER_MANUAL.md#26-诊断系统) | 结构化错误码、自动检测与热修�?|
| [用户手册 - 稳定性保障](USER_MANUAL.md#27-稳定性保�? | 防崩�?防卡死机制与测试覆盖 |
| [快速开始](../README.md#-快速开�? | 5 分钟上手 |

## 开发者文�?
| 文档 | 说明 |
|------|------|
| [贡献指南](../CONTRIBUTING.md) | 如何提交代码、报�?Bug |
| [更新日志](../CHANGELOG.md) | 版本历史和功能变�?|
| [安全政策](../SECURITY.md) | 漏洞报告流程 |

## 架构概览

```
cove/
├── agent/                  # Go 源码（单模块�?�?  ├── cli/cove/        # CLI 入口
�?  └── internal/           # 内部�?�?      ├── api/            # LLM Provider 适配（Anthropic, OpenAI, DeepSeek...�?�?      ├── engine/         # 核心引擎（消息循环、工具调用、权限检查）
�?      ├── repl/           # 终端交互（readline, spinner, color�?�?      ├── config/         # 配置加载与校�?�?      ├── tool/           # 工具注册与执行（bash, write, edit, grep...�?�?      ├── permission/     # 权限系统
�?      ├── diagnostic/     # 结构化异常管理与自检
�?      ├── session/        # 会话持久�?�?      ├── memory/         # 长期记忆
�?      ├── mcp/            # MCP 协议集成
�?      ├── plugin/         # 插件系统
�?      ├── dream/          # 后台记忆整理
�?      └── ...             # 其他 15+ 内部�?├── scripts/                # 构建脚本、Mock 服务�?├── docs/                   # 文档（你在这里）
├── CHANGELOG.md
├── CONTRIBUTING.md
├── LICENSE
└── README.md
```

## 常见问题

### Q: 如何切换 AI 提供商？

�?REPL 中输入：
```
/provider deepseek
/model deepseek-v4-pro
```

### Q: 程序提示 API Key 错误�?
运行诊断�?```
/diagnose
```
系统会自动检测并提示修复方式�?
### Q: 如何查看实时费用�?
```
/cost
```

### Q: 会话断了怎么恢复�?
```
cove --resume        # 恢复最后一次会�?/resume                 # �?REPL 中查看所有历史会�?```

### Q: 程序卡住了怎么办？

�?`Ctrl+C` 可以中断当前操作。如果是工具执行超时，引擎会自动检测并返回�?如果怀疑有系统问题，运�?`/diagnose full` 进行完整检查�?
### Q: 错误�?E2001 是什么意思？

运行 `/diagnose codes` 可以查看所有错误码及其含义�?详细说明�?[诊断系统文档](USER_MANUAL.md#26-诊断系统)�?
### Q: 怎么确认程序稳定性？

```bash
cd agent/
go test -v ./internal/engine/ -run "TestEngine"  # 16 个集成测�?go test ./...                                     # 全量测试
```
