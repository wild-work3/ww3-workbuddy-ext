package wb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work3/workbuddy-ext/internal/protocol/chat"
	"wild-work3/workbuddy-ext/internal/protocol/errkind"
)

// 上游对话流式（旧版 upstream/client.go ChatStream + sse.go 语义）。
//
// 插件不做 SSE → HTTP 透传：把上游帧解析成归一化 IPC 事件逐条发出（W3-13）。
// 硬约束：
//   - 空流不得合成假成功（validEvents==0 → errEmptyStream）
//   - 截断（半帧 EOF / 传输错误）不得补 completed，报 Truncated
//   - 已发出正文后 core 不会重放（W3-10），因此这里绝不吞错

// errEmptyStream 上游一帧有效数据都没给（典型：被 WAF 静默丢包）。
var errEmptyStream = errors.New("upstream empty stream")

// Emitter 是归一化事件出口（由 channel 包接到 rpc.EventWriter）。
type Emitter func(event string, payload map[string]any) error

// ChatStream 发一次上游对话请求并把增量事件转给 emit。
// 返回最终 usage（为空时给零值 Usage，遵守 null≠0 的约定由上层判定）。
func (c *Client) ChatStream(ctx context.Context, a *Auth, req *chat.GenerateRequest, emit Emitter) (*chat.Usage, error) {
	model := UpstreamModelOf(req.Model, a.RegionOf())

	// 1) 组装上游请求体：先按 OpenAI wire 形态拼，再经 PrepareBody 改写（首条 system / tool 重排）。
	// messages 必须走 JSON 往返转成 []any：PrepareBody 内部按 []any 断言，
	// 直接塞强类型 []chat.ChatMessage 会让全部改写（首条 system /
	// tool 结果重排）静默跳过。
	wireMsgs, err := toWire(req.Messages)
	if err != nil {
		return nil, &Error{Kind: errkind.BadParams, Msg: "请求体序列化失败: " + err.Error()}
	}
	body := map[string]any{
		"model":    model,
		"messages": wireMsgs,
		"stream":   true,
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.ToolChoice != nil {
		body["tool_choice"] = req.ToolChoice
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if req.Thinking {
		body["thinking"] = map[string]any{"type": "enabled"}
	}
	PrepareBody(body)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Kind: errkind.BadParams, Msg: "请求体序列化失败: " + err.Error()}
	}

	// 2) 会话锚点（会话头族依赖原始 messages 的用户文本指纹）
	conv, turn := conversationAnchors(marshalMessages(req.Messages))

	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoints.Base+epChat, strings.NewReader(string(raw)))
	if err != nil {
		return nil, &Error{Kind: errkind.BadParams, Msg: "构造上游请求失败: " + err.Error()}
	}
	httpReq.Header = c.chatHeaders(a, conv, turn)
	resp, err := c.Stream.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Kind: errkind.Cancel, Msg: "请求已取消"}
		}
		return nil, &Error{Kind: kindOfNetErr(err), Msg: "上游连接失败: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &Error{Kind: Classify(resp.StatusCode, string(msg)), Status: resp.StatusCode,
			Msg: fmt.Sprintf("上游 %d: %s", resp.StatusCode, truncate(string(msg), 300))}
	}

	// 3) 逐帧解析（idle 看门狗用 ctx 超时实现：每次读到数据就重置计时）
	u := &chat.Usage{}
	var firstTokenAt time.Time
	validEvents := 0
	sawDone := false
	finish := ""

	dec := newSSEReader(resp.Body, c.Idle)
	for {
		payload, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return u, &Error{Kind: errkind.Cancel, Msg: "请求已取消"}
			}
			if errors.Is(err, errIdleTimeout) {
				return u, &Error{Kind: errkind.IdleTimeout, Msg: fmt.Sprintf("上游空闲超时（%s 无数据）", c.Idle)}
			}
			return u, &Error{Kind: errkind.Truncated, Msg: "上游流中断: " + err.Error()}
		}
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // 坏帧静默跳过（旧版同语义）
		}
		// 上游错误信封：提前终止（不得当作正常结束）
		if e, ok := chunk["error"].(map[string]any); ok {
			msg, _ := e["message"].(string)
			code, _ := e["code"].(string)
			return u, &Error{Kind: Classify(200, msg+" "+code), Msg: "上游错误: " + orDefault(msg, code)}
		}
		validEvents++
		if us, ok := chunk["usage"].(map[string]any); ok {
			applyUsage(u, us)
		}
		choices, _ := chunk["choices"].([]any)
		for _, ci := range choices {
			ch, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
			delta, ok := ch["delta"].(map[string]any)
			if !ok {
				continue
			}
			if rc := reasoningDeltaOf(delta); rc != "" {
				if err := emit("reasoning.delta", map[string]any{"content": rc}); err != nil {
					return u, err
				}
			}
			if txt, ok := delta["content"].(string); ok && txt != "" {
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				if err := emit("text.delta", map[string]any{"content": txt}); err != nil {
					return u, err
				}
			}
			if tcs, ok := delta["tool_calls"].([]any); ok {
				if err := emitToolCalls(tcs, emit); err != nil {
					return u, err
				}
			}
		}
	}

	// 4) 收尾：空流/截断绝不伪装正常结束
	if validEvents == 0 {
		return u, &Error{Kind: errkind.Server, Msg: "上游返回空流（未收到任何有效事件）"}
	}
	if !sawDone && finish == "" {
		return u, &Error{Kind: errkind.Truncated, Msg: "上游流已中断（未收到结束标记）"}
	}
	u.TimeMs = time.Since(start).Milliseconds()
	if !firstTokenAt.IsZero() {
		u.FirstTokenMs = firstTokenAt.Sub(start).Milliseconds()
	}
	if err := emit("finish_reason", map[string]any{"reason": finish}); err != nil {
		return u, err
	}
	if err := emit("usage", usageFields(u)); err != nil {
		return u, err
	}
	if err := emit("completed", map[string]any{"finish_reason": finish}); err != nil {
		return u, err
	}
	return u, nil
}

// toWire 把强类型 messages 转成 []any（map 形态），供 PrepareBody 的 []any 断言使用。
func toWire(msgs []chat.ChatMessage) ([]any, error) {
	b, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	var out []any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// marshalMessages 把归一化 messages 重新序列化成原始 JSON（供锚点指纹使用）。
func marshalMessages(msgs []chat.ChatMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(msgs))
	for i := range msgs {
		b, err := json.Marshal(msgs[i])
		if err != nil {
			continue
		}
		out = append(out, b)
	}
	return out
}

// applyUsage 解析 usage 帧（缓存命中按 ExtractUsageCache 口径：null ≠ 0）。
func applyUsage(u *chat.Usage, m map[string]any) {
	u.PromptTokens = intOf(m["prompt_tokens"])
	u.CompletionTokens = intOf(m["completion_tokens"])
	u.TotalTokens = intOf(m["total_tokens"])
	cache := &chat.CacheUsage{}
	if f, ok := m["prompt_cache_hit_tokens"]; ok {
		cache.Cached, cache.CachedReported = intOf(f), true
	} else if f, ok := m["cache_read_input_tokens"]; ok {
		cache.Cached, cache.CachedReported = intOf(f), true
	} else if d, ok := m["prompt_tokens_details"].(map[string]any); ok {
		if f, ok := d["cached_tokens"]; ok {
			cache.Cached, cache.CachedReported = intOf(f), true
		}
		if f, ok := d["cache_write_tokens"]; ok {
			cache.Write, cache.WriteReported = intOf(f), true
		}
	}
	if cache.CachedReported || cache.WriteReported {
		u.Cache = cache
	}
	markReported(u)
}

// intOf 取整（负值钳 0）。
func intOf(v any) int {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case json.Number:
		g, _ := x.Float64()
		f = g
	}
	if f < 0 {
		return 0
	}
	return int(f)
}

// reasoningDeltaOf：reasoning_content > reasoning > reasoning_details（text 全文优先于 summary 摘要）。
func reasoningDeltaOf(d map[string]any) string {
	if v, ok := d["reasoning_content"].(string); ok && v != "" {
		return v
	}
	if v, ok := d["reasoning"].(string); ok && v != "" {
		return v
	}
	items, ok := d["reasoning_details"].([]any)
	if !ok {
		return ""
	}
	var full, summary strings.Builder
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "reasoning.text":
			if s, ok := m["text"].(string); ok {
				full.WriteString(s)
			}
		case "reasoning.summary":
			if s, ok := m["summary"].(string); ok {
				summary.WriteString(s)
			}
		}
	}
	if full.Len() > 0 {
		return full.String()
	}
	return summary.String()
}

// emitToolCalls 把 tool_calls 分片转成归一化 tool_call.delta 事件。
func emitToolCalls(tcs []any, emit Emitter) error {
	for i, ti := range tcs {
		tc, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		idx := i
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		p := map[string]any{"index": idx}
		if id, _ := tc["id"].(string); id != "" {
			p["id"] = id
		}
		if typ, _ := tc["type"].(string); typ != "" {
			p["type"] = typ
		}
		if fn, ok := tc["function"].(map[string]any); ok {
			if name, _ := fn["name"].(string); name != "" {
				p["name"] = name
			}
			if args, _ := fn["arguments"].(string); args != "" {
				p["arguments_fragment"] = args
			}
		}
		if err := emit("tool_call.delta", p); err != nil {
			return err
		}
	}
	return nil
}

// usageFields 把 Usage 摊成事件负载（IPC 语义：null ≠ 0，未上报的缓存字段不出现）。
func usageFields(u *chat.Usage) map[string]any {
	// 三维 token 的来源标志（W3-34）：值来自上游同一 usage 对象，明确 0 也是有效实报。
	p := map[string]any{
		"prompt_tokens":       u.PromptTokens,
		"completion_tokens":   u.CompletionTokens,
		"total_tokens":        u.TotalTokens,
		"prompt_reported":     u.PromptReported,
		"completion_reported": u.CompletionReported,
		"total_reported":      u.TotalReported,
		"time_ms":             u.TimeMs,
		"first_token_ms":      u.FirstTokenMs,
	}
	if u.Cache != nil {
		p["cache"] = map[string]any{
			"cached":          u.Cache.Cached,
			"cached_reported": u.Cache.CachedReported,
			"write":           u.Cache.Write,
			"write_reported":  u.Cache.WriteReported,
		}
	}
	return p
}

// markReported 标记三维 token 来自上游 usage 对象（W3-34）。
// 本地加出来的 total 不是上游实报，必须清掉 TotalReported，否则把自算的数谎报成实报。
func markReported(u *chat.Usage) {
	if u == nil {
		return
	}
	derived := u.TotalTokens == 0 && u.PromptTokens > 0 && u.CompletionTokens > 0
	if derived {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	u.MarkReported()
	if derived {
		u.TotalReported = false
	}
}
