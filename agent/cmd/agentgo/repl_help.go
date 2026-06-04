package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/agentgo/internal/api"
	"github.com/agentgo/internal/command"
	"github.com/agentgo/internal/config"
	"github.com/agentgo/internal/tool"
)

func showConfig() {
	cfg, _ := config.Load()
	pc := cfg.EffectiveProvider()
	data, _ := json.MarshalIndent(map[string]any{
		"version":         Version,
		"model":           cfg.Model,
		"provider":        pc.Name,
		"base_url":        pc.BaseURL,
		"permission_mode": cfg.PermissionMode,
		"max_budget_usd":  cfg.MaxBudgetUsd,
		"thinking_tokens": cfg.ThinkingTokens,
		"debug":           cfg.Debug,
		"api_key_set":     pc.APIKey != "",
		"mcp_servers":     len(cfg.MCPServers),
	}, "", "  ")
	fmt.Println(string(data))
}

func providerHelpLine() string {
	return "  /provider <名称>    设置供应商 (anthropic, deepseek, openai, openai-compatible, glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral)"
}

func providerEnvHelpLine() string {
	return "环境变量: LLM_API_KEY | ANTHROPIC_API_KEY | DEEPSEEK_API_KEY | OPENAI_API_KEY | GLM_API_KEY | KIMI_API_KEY | QWEN_API_KEY | OPENROUTER_API_KEY | SILICONFLOW_API_KEY | LLM_BASE_URL"
}

func printHelp(cmdReg *command.Registry, toolReg *tool.Registry) {
	fmt.Println("\n=== agentgo v" + Version + " ===")
	fmt.Println("\n供应商 / 模型:")
	fmt.Println("  /model <名称>       设置模型")
	fmt.Println(providerHelpLine())
	fmt.Println("  /api-key <密钥>     保存 API 密钥")
	fmt.Println("  /base-url <地址>    设置自定义接口地址")
	fmt.Println("  /mode <模式>        设置权限模式 (default|plan|auto|bypass)")
	fmt.Println("  /budget <金额|auto> 设置每会话预算上限 ($)，auto 为一键提升")
	fmt.Println("  /cost               查看用量和费用")
	fmt.Println("  /ratelimit          查看 API 速率限制状态")
	fmt.Println("  /attach <文件...>   挂载图片或文件；list/remove/clear 管理列表")
	fmt.Println("  /config             查看完整配置")
	fmt.Println("\n会话:")
	fmt.Println("  /compact            压缩对话历史")
	fmt.Println("  /undo               回退到上一个检查点")
	fmt.Println("  /checkpoints        列出所有检查点")
	fmt.Println("  /history            查看和继续历史会话")
	fmt.Println("  /resume [id]        恢复已保存的会话")
	fmt.Println("  /memory             管理持久化记忆")
	fmt.Println("\n系统:")
	fmt.Println("  /mcp                管理 MCP 服务器")
	fmt.Println("  /plugin             管理插件")
	fmt.Println("  /skills             列出技能")
	fmt.Println("\n命令:")
	for _, c := range cmdReg.All() {
		fmt.Printf("  /%-16s %s\n", c.Name(), c.Description())
	}
	fmt.Println("\n工具:")
	for _, t := range toolReg.All() {
		d := t.Def()
		ro := " "
		if d.IsReadOnly {
			ro = "R"
		}
		fmt.Printf("  [%s] %-12s %s\n", ro, d.Name, truncateDesc(d.Description, 48))
	}
	fmt.Println("\n" + providerEnvHelpLine())
	fmt.Println("启动参数: -p <提示> [--image <路径>] [--file <路径>] | -d --debug | -v --version | --doctor | --config")
	fmt.Println("附件输入: 在 REPL 或 -p 文本中可写 @路径，例如：解释这张图 @assets/screen.png")
	fmt.Println()
}

func missingAPIKeyMessage(provider string) string {
	provider = api.NormalizeProviderName(strings.TrimSpace(provider))
	if provider == "" {
		provider = "anthropic"
	}
	providerEnvCandidates := api.ProviderEnvCandidates(provider)
	primaryEnv := "LLM_API_KEY"
	if len(providerEnvCandidates) > 0 {
		primaryEnv = providerEnvCandidates[0]
	}
	openAICompatList := "glm, kimi, qwen, doubao, openrouter, siliconflow, groq, together, fireworks, xai, mistral"
	return fmt.Sprintf(
		"No API key configured / 未配置 API key.\n"+
			"先看当前厂商：%s\n\n"+
			"最快的办法：直接在当前 REPL 输入\n"+
			"  /api-key <你的key>\n\n"+
			"如果你用 Claude / Anthropic：设置 %s\n"+
			"如果你用 DeepSeek：设置 DEEPSEEK_API_KEY\n"+
			"如果你用 OpenAI：设置 OPENAI_API_KEY\n\n"+
			"如果你用 GLM / Kimi / Qwen / 豆包 / OpenRouter / 硅基流动 / Groq / Together / Fireworks / xAI / Mistral 这类兼容 OpenAI 的接口：\n"+
			"  1) /provider openai-compatible  （或直接 /provider 对应厂商名）\n"+
			"  2) /base-url <兼容 OpenAI 的接口地址>\n"+
			"  3) /api-key <你的key>\n\n"+
			"例如 GLM：        /provider glm          + GLM_API_KEY / ZHIPU_API_KEY\n"+
			"例如 Kimi：       /provider kimi         + KIMI_API_KEY / MOONSHOT_API_KEY\n"+
			"例如 Qwen：       /provider qwen         + QWEN_API_KEY / DASHSCOPE_API_KEY\n"+
			"例如 豆包：       /provider doubao       + DOUBAO_API_KEY / ARK_API_KEY\n"+
			"例如 OpenRouter： /provider openrouter   + OPENROUTER_API_KEY\n"+
			"例如 硅基流动：   /provider siliconflow + SILICONFLOW_API_KEY\n\n"+
			"当前内置适配 provider：anthropic, deepseek, openai, openai-compatible, %s\n"+
			"也可用通用变量：LLM_API_KEY\n"+
			"设置后执行 /config，确认 api_key_set: true。",
		provider,
		primaryEnv,
		openAICompatList,
	)
}
