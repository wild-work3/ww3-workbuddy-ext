package wb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"wild-work3/workbuddy-ext/internal/protocol/meta"
)

// FetchModels 拉模型目录（旧版 upstream/client.go FetchModels + workbuddyai/models.go 合并版）。
//
// 权威顺序来自 data.agents[name=="cli"].models，data.models 只提供元数据。
// 过滤 brokenModels（实测 400 code=11102），补齐 extraModels（目录不返回但可用）。
// 价格/上下文保持 nil 不臆造 —— 上游只在部分模型返回 maxInputTokens。
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]meta.ModelInfo, error) {
	h := http.Header{}
	commonHeaders(h, c.Endpoints)
	h.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		h.Set("X-Domain", a.Domain)
	}

	var raw json.RawMessage
	// 目录接口走 HTTP 层（不带信封解析，形状是 {code,data:{models,agents}}）
	if err := c.doJSON(ctx, http.MethodGet, c.Endpoints.Base+c.Endpoints.modelsPath(), h, nil, &raw); err != nil {
		return nil, err
	}
	var cat struct {
		Models []catalogModel `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(raw, &cat); err != nil {
		return nil, &Error{Kind: "server", Msg: "模型目录解析失败: " + truncate(err.Error(), 120)}
	}
	byID := make(map[string]catalogModel, len(cat.Models))
	for _, m := range cat.Models {
		byID[m.ID] = m
	}
	// 权威顺序：cli agent 的 models 列表
	var order []string
	for _, ag := range cat.Agents {
		if ag.Name == "cli" {
			order = ag.Models
			break
		}
	}
	if len(order) == 0 {
		return nil, &Error{Kind: "server", Msg: "模型目录未返回 cli agent 模型列表"}
	}
	out := make([]meta.ModelInfo, 0, len(order)+len(extraModels))
	seen := map[string]bool{}
	for _, id := range order {
		if seen[id] || isBrokenModel(id) {
			continue
		}
		m := byID[id]
		if m.Disabled {
			continue
		}
		seen[id] = true
		out = append(out, convertModel(m))
	}
	// 补齐目录外可用模型
	for _, m := range extraModels {
		if seen[m.ID] || isBrokenModel(m.ID) {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		out = StaticModels() // 双区目录都挂时的兜底
	}
	return out, nil
}

// catalogModel 是目录接口的模型元数据。
type catalogModel struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	MaxInputTokens     int     `json:"maxInputTokens"`
	MaxOutputTokens    int     `json:"maxOutputTokens"`
	Disabled           bool    `json:"disabled"`
	SupportsImages     bool    `json:"supportsImages"`
	SupportsReasoning  bool    `json:"supportsReasoning"`
	SupportsToolCall   bool    `json:"supportsToolCall"`
	DisabledMultimodal bool    `json:"disabledMultimodal"`
	Credits            string  `json:"credits"`
	BillRate           float64 `json:"billRate"`
}

// convertModel 把目录元数据转成归一化 ModelInfo。
func convertModel(m catalogModel) meta.ModelInfo {
	mi := meta.ModelInfo{ID: m.ID, Name: orDefault(m.Name, m.ID)}
	if m.MaxInputTokens > 0 {
		mi.Context = intp(m.MaxInputTokens)
	}
	mi.Capabilities = meta.Capabilities2{
		Tools:     m.SupportsToolCall,
		Vision:    m.SupportsImages && !m.DisabledMultimodal,
		Reasoning: m.SupportsReasoning,
	}
	return mi
}

// isBrokenModel 判断是否属实测不可用模型（400 code=11102）。
func isBrokenModel(id string) bool { return brokenModels[id] }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ModelPrefix 返回模型所属命名空间前缀（同时是区域标识）。
func ModelPrefix(region string) string { return NormalizeRegion(region) }

// PublicModelID 拼接对外的模型 ID（<前缀>/<上游模型名>）。
func PublicModelID(prefix, upstream string) string { return prefix + "/" + upstream }

// UpstreamModelOf 剥掉命名空间前缀（供 worker 使用）。
// core 下发的 req.Model 通常已是裸上游名，此处容错两种形态。
func UpstreamModelOf(model, region string) string {
	for _, p := range []string{RegionCN + "/", RegionGlobal + "/"} {
		if len(model) > len(p) && model[:len(p)] == p {
			return model[len(p):]
		}
	}
	return model
}

// String 便于日志里打印模型清单。
func String(ms []meta.ModelInfo) string { return fmt.Sprintf("%d models", len(ms)) }

// ModelPrefixOf 从对外模型名里取出命名空间前缀（`workbuddyai/gpt-5.5` → `workbuddyai`）。
// 无前缀返回空串：core 下发的裸上游名不带区信息，此时由渠道实例的 config.region 决定。
func ModelPrefixOf(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		return model[:i]
	}
	return ""
}
