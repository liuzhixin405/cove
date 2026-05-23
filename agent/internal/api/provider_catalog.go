package api

import "strings"

var providerAliases = map[string]string{
	"claude":            "anthropic",
	"anthropic":         "anthropic",
	"deepseek":          "deepseek",
	"openai":            "openai",
	"openai-compatible": "openai-compatible",
	"openai_compatible": "openai-compatible",
	"compatible":        "openai-compatible",
	"zhipu":             "glm",
	"bigmodel":          "glm",
	"glm":               "glm",
	"moonshot":          "kimi",
	"kimi":              "kimi",
	"dashscope":         "qwen",
	"tongyi":            "qwen",
	"qwen":              "qwen",
	"ark":               "doubao",
	"doubao":            "doubao",
	"openrouter":        "openrouter",
	"siliconflow":       "siliconflow",
	"groq":              "groq",
	"together":          "together",
	"fireworks":         "fireworks",
	"grok":              "xai",
	"xai":               "xai",
	"mistral":           "mistral",
}

var providerBaseURLs = map[string]string{
	"anthropic":         "https://api.anthropic.com/v1",
	"deepseek":          "https://api.deepseek.com/v1",
	"openai":            "https://api.openai.com/v1",
	"openai-compatible": "https://api.openai.com/v1",
	"glm":               "https://open.bigmodel.cn/api/paas/v4",
	"kimi":              "https://api.moonshot.cn/v1",
	"qwen":              "https://dashscope.aliyuncs.com/compatible-mode/v1",
	"doubao":            "https://ark.cn-beijing.volces.com/api/v3",
	"openrouter":        "https://openrouter.ai/api/v1",
	"siliconflow":       "https://api.siliconflow.cn/v1",
	"groq":              "https://api.groq.com/openai/v1",
	"together":          "https://api.together.xyz/v1",
	"fireworks":         "https://api.fireworks.ai/inference/v1",
	"xai":               "https://api.x.ai/v1",
	"mistral":           "https://api.mistral.ai/v1",
}

var providerEnvVars = map[string][]string{
	"anthropic":         {"ANTHROPIC_API_KEY"},
	"deepseek":          {"DEEPSEEK_API_KEY"},
	"openai":            {"OPENAI_API_KEY"},
	"openai-compatible": {"OPENAI_API_KEY"},
	"glm":               {"GLM_API_KEY", "ZHIPU_API_KEY", "BIGMODEL_API_KEY"},
	"kimi":              {"KIMI_API_KEY", "MOONSHOT_API_KEY"},
	"qwen":              {"QWEN_API_KEY", "DASHSCOPE_API_KEY"},
	"doubao":            {"DOUBAO_API_KEY", "ARK_API_KEY", "VOLCENGINE_API_KEY"},
	"openrouter":        {"OPENROUTER_API_KEY"},
	"siliconflow":       {"SILICONFLOW_API_KEY"},
	"groq":              {"GROQ_API_KEY"},
	"together":          {"TOGETHER_API_KEY"},
	"fireworks":         {"FIREWORKS_API_KEY"},
	"xai":               {"XAI_API_KEY", "GROK_API_KEY"},
	"mistral":           {"MISTRAL_API_KEY"},
}

func NormalizeProviderName(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return ""
	}
	if canonical, ok := providerAliases[key]; ok {
		return canonical
	}
	return key
}

func IsOpenAICompatibleProvider(name string) bool {
	normalized := NormalizeProviderName(name)
	return normalized != "" && normalized != "anthropic"
}

func DefaultBaseURL(name string) string {
	normalized := NormalizeProviderName(name)
	if normalized == "" {
		normalized = "openai-compatible"
	}
	if baseURL, ok := providerBaseURLs[normalized]; ok {
		return baseURL
	}
	return providerBaseURLs["openai-compatible"]
}

func ProviderEnvCandidates(name string) []string {
	normalized := NormalizeProviderName(name)
	seen := map[string]bool{}
	var envs []string
	appendEnv := func(key string) {
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		envs = append(envs, key)
	}
	for _, key := range providerEnvVars[normalized] {
		appendEnv(key)
	}
	if normalized == "" || normalized == "openai-compatible" {
		appendEnv("OPENAI_API_KEY")
	}
	if normalized != "" {
		guessed := strings.ToUpper(strings.NewReplacer("-", "_", " ", "_", "/", "_").Replace(normalized)) + "_API_KEY"
		appendEnv(guessed)
	}
	appendEnv("LLM_API_KEY")
	return envs
}
