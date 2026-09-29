<div align="center">

# 🤖 cove

**像专家一样在终端里写代码，不仅是 AI 助手，更是你随叫随到的自动化专家。**

[![CI](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml/badge.svg)](https://github.com/liuzhixin405/cove/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/liuzhixin405/cove?include_prereleases)](https://github.com/liuzhixin405/cove/releases)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

[中文](#chinese) | [English](#english)

</div>

---

### 🌟 为什么选择 Cove？

大多数 AI CLI 只是把 API 包了一层，而 **Cove** 把整个开发环境变成了 AI 的“游乐场”：

*   **⚡️ 极致轻量**：单文件 Go 二进制，零依赖，下载即用。
*   **🧠 懂你的仓库**：内置智能代码地图（RepoMap），AI 对你的项目结构一目了然。
*   **⚙️ 深度自动化**：不仅仅是写代码，它能操作 Shell、浏览器、文件系统，甚至能自主规划多步任务（Plan Mode）。
*   **💸 极致经济**：对 DeepSeek / GLM / Kimi 等国产模型深度优化，以极低成本获得顶级开发能力。
*   **🔐 完全掌控**：你的 API Key 在你手里，本地数据，完全自主。

*(GIF: 在此展示 Cove 在终端里通过 `/cd` 切换目录，快速扫描项目，并自主修复一个测试报错的炫酷过程)*

> **想快速体验？** 运行 `cove -p "修复这个 repo 里的 TODOs"`，看它如何自动规划、执行并验证。

---

<a name="chinese"></a>
## 中文

cove 是一个**终端 AI 代码助手**——本质是写代码。因为它本地运行、自带 shell/浏览器/文件/MCP 等真实工具，所以顺带也能操作电脑、完成各类自动化。单文件 Go 二进制，本地运行，API key 完全由你掌控，并把 **DeepSeek / GLM / Kimi / Qwen / Doubao** 当作一等公民深度适配，而不是「兼容接口凑合用」。
