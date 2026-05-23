# agentgo v1.0.1 最终交付清单

适用范围：`demo/` 下的 agentgo Windows 绿色版 / 多平台发布产物。

## 1. 绿色版使用说明

### 1.1 Windows 绿色版是什么
- 无安装器，解压即用。
- 当前 Windows 绿色版主交付物：
  - `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.zip`
  - `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.exe`
- `dist/latest/agentgo-v1.0.1-windows-amd64.exe` 现也已同步到同一新版本。

### 1.2 Windows 使用步骤
1. 解压 `agentgo-v1.0.1-windows-amd64.zip` 到任意目录。
2. 进入解压目录。
3. 直接运行 `agentgo.exe`，或直接运行发布目录内的 `agentgo-v1.0.1-windows-amd64.exe`。
4. 如需全局命令，再手动把解压目录加入 PATH。

### 1.3 Windows 中文/终端注意事项
- 优先使用 Windows Terminal。
- 终端建议使用 UTF-8。
- 如果在传统 cmd 中中文仍乱码，可先执行：
  - `chcp 65001`
- 新版本会尽量自动把 Windows 控制台输入/输出代码页切到 UTF-8。
- 若启动后仍出现控制台代码页提醒，请在同一个窗口先执行 `chcp 65001`，再重新启动 agentgo。

### 1.4 首次 API key 配置
最简单方式：
- 在 REPL 中直接执行：`/api-key <key>`

也可以在启动前设置环境变量，然后执行 `/config` 确认：
- `api_key_set: true`

当前内置 provider：
- 原生：`anthropic`、`deepseek`、`openai`
- 兼容 OpenAI：`openai-compatible`、`glm`、`kimi`、`qwen`、`doubao`、`openrouter`、`siliconflow`、`groq`、`together`、`fireworks`、`xai`、`mistral`

常见环境变量：
- `ANTHROPIC_API_KEY`
- `DEEPSEEK_API_KEY`
- `OPENAI_API_KEY`
- `GLM_API_KEY` / `ZHIPU_API_KEY`
- `KIMI_API_KEY` / `MOONSHOT_API_KEY`
- `QWEN_API_KEY` / `DASHSCOPE_API_KEY`
- `DOUBAO_API_KEY` / `ARK_API_KEY`
- `OPENROUTER_API_KEY`
- `SILICONFLOW_API_KEY`
- 通用回退：`LLM_API_KEY`
- 自定义兼容接口地址：`LLM_BASE_URL`

## 2. 发布步骤

### 2.1 本地发布
在仓库根目录执行：

```bash
python scripts/release_build.py v1.0.1
```

脚本位置：
- `scripts/release_build.py`

脚本行为（已验证）：
- 构建 Windows / Linux / macOS(amd64, arm64) 产物
- 输出到 `dist/v1.0.1/`
- 写出 `dist/v1.0.1/checksums.txt`
- 同步复制到 `dist/latest/`
- 若 `dist/latest/` 中某文件被占用，不再整次失败，而是输出 warning 并继续同步其余文件

### 2.2 GitHub Actions 发布
工作流文件：
- `.github/workflows/release.yml`

触发方式：
- 手动触发 `workflow_dispatch`
- 或推送 tag：`v*`

工作流关键步骤：
1. checkout
2. setup go（读取 `demo/go.mod`）
3. 在 `demo/` 下执行 `go test ./...`
4. 执行：`python scripts/release_build.py <version>`
5. 上传：
   - `dist/<version>/agentgo-*.zip`
   - `dist/<version>/agentgo-*.tar.gz`
   - `dist/<version>/checksums.txt`

## 3. 校验步骤

### 3.1 测试校验
已验证命令：

```bash
cd demo && go test ./...
```

针对本次帮助/发布补丁，还额外验证过：
- `go test ./cmd/agentgo`
- `python -m unittest scripts.test_release_build`

### 3.2 版本校验
已验证：

```bash
dist/v1.0.1/agentgo-v1.0.1-windows-amd64.exe --version
```

结果：
- `agentgo 1.0.1 (built 2026-05-23T04:10:53Z, commit unknown)`

并已确认 `dist/latest/agentgo-v1.0.1-windows-amd64.exe` 与 `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.exe` 当前为相同二进制。

### 3.3 首次启动提示校验
已验证新的 Windows 包包含以下提示：
- `先看当前厂商`
- `/provider openai-compatible`
- `OPENROUTER_API_KEY`
- `GLM_API_KEY`
- `SILICONFLOW_API_KEY`

### 3.4 压缩包 SHA256 校验
校验文件：
- `dist/v1.0.1/checksums.txt`

Windows zip 当前 SHA256：
- `f546d4234655c06ccf971deebb547fd2a3392ede33dfd4444ca992fdf533047e`

Windows 本地可执行校验命令示例：

```bash
certutil -hashfile dist\v1.0.1\agentgo-v1.0.1-windows-amd64.zip SHA256
```

然后与 `checksums.txt` 对比。

### 3.5 latest 同步校验
当前已验证：
- `dist/latest/agentgo-v1.0.1-windows-amd64.exe`
- `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.exe`

二者当前一致：
- 文件大小：`10476544`
- SHA256：`9a17829a73e4dc19ebfa659d65b6318521600070be5a20ed4a0f4a4a39ef335c`

## 4. 发布产物清单

### 4.1 dist/v1.0.1
- `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.exe`
- `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.zip`
- `dist/v1.0.1/agentgo-v1.0.1-linux-amd64`
- `dist/v1.0.1/agentgo-v1.0.1-linux-amd64.tar.gz`
- `dist/v1.0.1/agentgo-v1.0.1-darwin-amd64`
- `dist/v1.0.1/agentgo-v1.0.1-darwin-amd64.tar.gz`
- `dist/v1.0.1/agentgo-v1.0.1-darwin-arm64`
- `dist/v1.0.1/agentgo-v1.0.1-darwin-arm64.tar.gz`
- `dist/v1.0.1/checksums.txt`

### 4.2 dist/latest
- `dist/latest/agentgo-v1.0.1-windows-amd64.exe`
- `dist/latest/agentgo-v1.0.1-windows-amd64.zip`
- `dist/latest/checksums.txt`
- 以及同版本 linux / darwin 对应文件

## 5. 目录说明

### 5.1 核心源码
- `demo/`
  - agentgo 的 Go 实现主目录

### 5.2 主要文档
- `demo/README.md`
  - 绿色版使用、配置、命令、目录说明
- `README.md`
  - 根级说明与 portable/release 概览
- `demo/docs/delivery-checklist-v1.0.1.md`
  - 本最终交付清单
- `demo/docs/plans/2026-05-23-demo-rewrite.md`
  - 本轮实现/验证计划记录

### 5.3 发布相关
- `scripts/release_build.py`
  - 本地多平台发布构建脚本
- `scripts/test_release_build.py`
  - 发布脚本回归测试
- `.github/workflows/release.yml`
  - GitHub Actions 发布工作流

### 5.4 构建输出
- `dist/v1.0.1/`
  - 当前版本正式产物目录
- `dist/latest/`
  - latest 快捷目录，指向最近一次成功同步的发布产物

## 6. 当前交付结论

已确认当前可对外交付的 Windows 绿色版为：
- `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.zip`
- `dist/v1.0.1/agentgo-v1.0.1-windows-amd64.exe`

且 `dist/latest/agentgo-v1.0.1-windows-amd64.exe` 已同步到同一新版本，可作为 latest 快捷入口使用。