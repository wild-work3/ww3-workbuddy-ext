package wb

import "wild-work3/workbuddy-ext/internal/protocol/meta"

// brokenModels 实测不可用（400 code=11102 service info not found），
// 一律从动态目录结果中剔除，避免出现在 /models 与费率面板。
// 注：这些多为国内版命名，国际版靠 -preview/-f/-codex 后缀区分。
var brokenModels = map[string]bool{
	"deepseek-v4-pro":   true,
	"deepseek-v4-flash": true,
	"deepseek-chat":     true,
	"deepseek-reasoner": true,
	"deepseek-v4.1":     true,
	"hy3-preview":       true,
	"hy3-preview-agent": true,
	"gpt-6":             true,
	"kimi-k2.8":         true, // 须带 -preview
}

// extraModels 目录接口不返回、但实测可用的模型，硬编码补进模型清单。
// 无上游能力数据，故不声明任何能力（零臆造）。
var extraModels = []meta.ModelInfo{
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", Context: intp(1_000_000)},
	{ID: "hy4-preview-f", Name: "Hy4 preview F", Context: intp(1_000_000)},
	{ID: "gpt-6-astra", Name: "GPT-6-Astra", Context: intp(1_000_000)},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", Context: intp(1_000_000)},
	{ID: "kimi-k2.7", Name: "Kimi-K2.7", Context: intp(1_000_000)},
	{ID: "minimax-m3", Name: "MiniMax-M3", Context: intp(256_000)},
	{ID: "deepseek-v3", Name: "Deepseek-V3", Context: intp(192_000)},
	{ID: "glm-5.1", Name: "GLM-5.1", Context: intp(1_000_000)},
	{ID: "glm-5v-turbo", Name: "GLM-5V-Turbo", Context: intp(256_000)},
}

// freeModels 实测 x0.00 且可用的免费模型，按优先级排列。
// 每日保活（看门狗 poke）只用这些模型，确保不扣积分。
var freeModels = []string{
	"deepseek-v4.1-flash",
	"hy3",
	"hy4-preview",
	"hy4-preview-f",
}

// staticModels 静态兜底模型表：目录接口不可用时的 fallback（已剔除 brokenModels）。
var staticModels = []meta.ModelInfo{
	{ID: "hy3", Name: "Hy3", Context: intp(192_000)},
	{ID: "hy4-preview", Name: "Hy4 preview", Context: intp(1_000_000)},
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", Context: intp(1_000_000)},
	{ID: "hy4-preview-f", Name: "Hy4 preview F", Context: intp(1_000_000)},
	{ID: "default-model", Name: "Auto", Context: intp(176_000)},
	{ID: "fast-model", Name: "Fast", Context: intp(200_000)},
	{ID: "balanced-model", Name: "Balanced", Context: intp(256_000)},
	{ID: "primary-model", Name: "Primary", Context: intp(272_000)},
	{ID: "deep-model", Name: "Deep", Context: intp(176_000)},
	{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol", Context: intp(1_000_000)},
	{ID: "gpt-5.6-terra", Name: "GPT-5.6-Terra", Context: intp(1_000_000)},
	{ID: "gpt-5.6-luna", Name: "GPT-5.6-Luna", Context: intp(1_000_000)},
	{ID: "gpt-6-astra", Name: "GPT-6-Astra", Context: intp(1_000_000)},
	{ID: "gpt-5.5", Name: "GPT-5.5", Context: intp(1_000_000)},
	{ID: "gpt-5.4", Name: "GPT-5.4", Context: intp(272_000)},
	{ID: "gpt-5.3-codex", Name: "GPT-5.3-Codex", Context: intp(272_000)},
	{ID: "gemini-3.5-flash", Name: "Gemini-3.5-Flash", Context: intp(1_000_000)},
	{ID: "glm-5.3", Name: "GLM-5.3", Context: intp(1_000_000)},
	{ID: "glm-5.2", Name: "GLM-5.2", Context: intp(1_000_000)},
	{ID: "kimi-k3", Name: "Kimi-K3", Context: intp(1_000_000)},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6", Context: intp(256_000)},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", Context: intp(1_000_000)},
	{ID: "kimi-k2.7", Name: "Kimi-K2.7", Context: intp(1_000_000)},
	{ID: "minimax-m3", Name: "MiniMax-M3", Context: intp(256_000)},
	{ID: "deepseek-v3", Name: "Deepseek-V3", Context: intp(192_000)},
	{ID: "glm-5.1", Name: "GLM-5.1", Context: intp(1_000_000)},
	{ID: "glm-5v-turbo", Name: "GLM-5V-Turbo", Context: intp(256_000)},
}

// StaticModels 返回静态兜底模型表副本。
func StaticModels() []meta.ModelInfo {
	return append([]meta.ModelInfo{}, staticModels...)
}

func intp(v int) *int { return &v }
