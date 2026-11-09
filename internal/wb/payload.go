package wb

import (
	"encoding/json"
	"strings"
)

// PrepareBody 把归一化对话请求改写成上游可接受的形态（旧版 upstream/payload.go 语义）。
//
// 顺序有依赖，勿调换：
//  1. stream=true 强制
//  2. max_completion_tokens → max_tokens（别名只删不译，显式 max_tokens 优先）
//  3. 缺省补 stream_options.include_usage（否则末帧没有 usage）
//  4. tool_choice 归一（上游只认字符串）
//  5. 保证首条为 system（上游缺则回 400 11-128）—— 渠道专属怪癖，必须保留
//  6. tool 结果块重排 + 孤儿配对清理（顺序：先重排再删孤儿）
//
// developer→system 与出站脱敏已上提到网关统一规则引擎（AGENTS W3-35：role.developer
// 与 sanitize.* 规则在 gateway 入站统一执行），插件不再重复实现——早期各插件各抄一份
// 导致字面量漂移（本仓旧 sanitize.go 的改写对已被工具链破坏成自替换）。
func PrepareBody(obj map[string]any) map[string]any {
	obj["stream"] = true
	translateMaxCompletionTokens(obj)
	if _, ok := obj["stream_options"]; !ok {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	ensureLeadingSystemMessage(obj)
	if msgs, ok := obj["messages"].([]any); ok {
		if repacked, ch := repackToolResultBlocks(msgs); ch {
			obj["messages"] = repacked
			msgs = repacked
		}
		if cleaned, ch := cleanupOrphanToolCalls(msgs); ch {
			obj["messages"] = cleaned
		}
	}
	return obj
}

// translateMaxCompletionTokens 把别名翻成 max_tokens：显式 max_tokens 优先，别名一律删掉
// （上游见到别名会报 400）；0/null/负数不翻译（表示「不指定」）。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, ok := obj["max_completion_tokens"]
	if !ok {
		return
	}
	delete(obj, "max_completion_tokens")
	if _, has := obj["max_tokens"]; has {
		return
	}
	if f, ok := alias.(float64); ok && f > 0 {
		obj["max_tokens"] = int64(f)
	}
}

// normalizeToolChoice 上游只接受字符串 tool_choice：
// none → 删 tools/functions；auto|required → 字符串；function → 直接写名字；其他 → 删键。
func normalizeToolChoice(obj map[string]any) {
	v, ok := obj["tool_choice"]
	if !ok {
		return
	}
	switch tv := v.(type) {
	case string:
		if strings.EqualFold(tv, "none") {
			delete(obj, "tool_choice")
			delete(obj, "tools")
			delete(obj, "functions")
			return
		}
		obj["tool_choice"] = strings.ToLower(tv)
	case map[string]any:
		switch t, _ := tv["type"].(string); strings.ToLower(t) {
		case "none":
			delete(obj, "tool_choice")
			delete(obj, "tools")
			delete(obj, "functions")
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(t)
		case "function":
			name := ""
			if fn, ok := tv["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name = "auto"
			}
			obj["tool_choice"] = name
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// ensureLeadingSystemMessage 保证首条消息为 system。
// 上游（国内版/国际版一致）对首条非 system 的请求回 400 {"code":"11-128","msg":"first message is not system prompt"}，
// 旧版 internal/workbuddyai/wb_utils.go 同名函数即为本处来源，必须保留。
func ensureLeadingSystemMessage(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	if first, ok := msgs[0].(map[string]any); ok {
		if r, _ := first["role"].(string); r == "system" {
			return
		}
	}
	obj["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, msgs...)
}

// PrepareBodyJSON 是 PrepareBody 的字节入口（worker 侧使用）。
func PrepareBodyJSON(raw []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	PrepareBody(obj)
	return json.Marshal(obj)
}
