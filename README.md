<div align="center">

# 🤖 Cove-Agent: 终端里的 AI 自动化专家

**像专家一样在终端里写代码。不再是简单的 AI 聊天，而是你的自动化代码执行引擎。**

[![CI](https://github.com/liuzhixin405/cove-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/liuzhixin405/cove-agent/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/liuzhixin405/cove?include_prereleases)](https://github.com/liuzhixin405/cove-agent/releases)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](#中文) | [English](#english)

</div>

---

### 🚀 核心价值主张 (Key Value Propositions)

Cove-Agent 重新定义了 **AI 辅助编程**，将开发效率提升至自动化流水线级别：

* **⚡️ 真正的一等公民模型支持**：针对 **DeepSeek V3/R1, Qwen 2.5, GLM-4, Kimi, 豆包** 进行了底层 token 和推理路径的深度适配，成本极低，性能极致。
* **🛠️ 深度自动化引擎 (Plan Mode)**：基于 `Plan` 模式的复杂任务编排，让 AI 不仅能改代码，更能**自主规划、执行、测试、诊断**整个开发周期。
* **🧠 智能语境管理 (RepoMap)**：内置高性能代码上下文索引，AI 对你的仓库结构、依赖关系、符号定义了如指掌。
* **🖥️ 终端即是全能工作台**：内置 Shell、浏览器、文件系统操作，支持 MCP 协议，无需离开终端即可完成全链路开发。
* **🛡️ 隐私与安全**：单文件 Go 二进制，无依赖，所有 API Key 本地管理，数据绝不外泄。

---

### 📽️ 自动化演示

cove和其他agent没什么不一样的，此处省略N多token

---

<a name="中文"></a>

## 💡 为什么选择 Cove-Agent？

**Cove-Agent 是专门为需要终端自动化开发体验的工程师打造的工具。**

它不仅仅是 `Claude Code` 或 `Aider` 的替代品，更是国产模型在编程领域落地的最佳实践。我们深入调研了大量开发者工作流，专注于解决“如何更聪明地在终端内完成任务”的问题：

### 核心特性关键词

- **#终端编程** (Terminal-based IDE)
- **#AI自动化** (AI Agent Workflow)
- **#代码重构** (Refactoring with AI)
- **#国产模型优化** (DeepSeek/Qwen/Kimi/GLM/Doubao)
- **#MCP支持** (Model Context Protocol)
- **#低延迟开发** (Low-latency dev workflow)
- **#单元测试自动化** (TDD with AI)

---

### 快速安装与使用

```bash
# 下载二进制文件
curl -fsSL https://get.cove.dev | bash

# 启动任务
cove /new "添加一个新的 API 接口并更新 README"
```

*更多信息请查看 [贡献指南](CONTRIBUTING.md) 和 [开发文档](docs/README.md)。*

<a name="english"></a>

## English

Cove-Agent is an AI-native Terminal CLI designed for high-performance development. It treats your local repository as its playground, leveraging advanced models like DeepSeek to provide deep, contextual, and autonomous coding assistance.
