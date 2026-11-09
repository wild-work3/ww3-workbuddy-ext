package wb

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"wild-work3/workbuddy-ext/internal/protocol/meta"
)

// 额度/签到（旧版 upstream/client.go getUserResource + checkin_activity.go）。
//
// 上游只给「套餐列表」，不提供余额总量：每个包按周期口径折算，剩余负值钳 0。
// Key 必须是 PackageName|到期日（不含时刻），否则周期升级瞬间每个包都会冒出一对假流水。

// resourceAccount 是一个套餐的额度事实。
type resourceAccount struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，UTC+8 墙钟
}

// bill 按周期口径优先返回 (总额, 已用, 剩余)；剩余负值钳 0。
func (r resourceAccount) bill() (tot, used, remain int64) {
	switch {
	case r.CycleCapacitySize > 0:
		tot, used, remain = r.CycleCapacitySize, r.CycleCapacityUsed, r.CycleCapacityRemain
	case r.CycleCapacityRemain > 0 || r.CycleCapacityUsed > 0:
		tot, used, remain = r.CycleCapacityRemain+r.CycleCapacityUsed, r.CycleCapacityUsed, r.CycleCapacityRemain
	default:
		tot, used, remain = r.CapacitySize, r.CapacityUsed, r.CapacityRemain
	}
	if remain < 0 {
		remain = 0
	}
	return tot, used, remain
}

// expireAt 把 CycleEndTime 墙钟串转为 RFC3339 精确时刻（固定 UTC+8，不能用 time.Local）。
func (r resourceAccount) expireAt() string {
	ts := strings.TrimSpace(r.CycleEndTime)
	if ts == "" {
		return ""
	}
	if t, err := time.ParseInLocation(wallClockLayout, ts, softRateResetLoc); err == nil {
		return t.Format(time.RFC3339)
	}
	return ""
}

// expireDay 取到期日的日期前缀（专供 ledger 伪键，键要稳定不含时刻）。
func (r resourceAccount) expireDay() string {
	at := r.expireAt()
	if len(at) >= 10 {
		return at[:10]
	}
	return ""
}

// fetchUserResource 拉套餐列表。
func (c *Client) fetchUserResource(ctx context.Context, a *Auth) ([]resourceAccount, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              productCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(wallClockLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(wallClockLayout),
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []resourceAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.Endpoints.Base+epUserResource, c.billingHeaders(a), body, &resp); err != nil {
		return nil, err
	}
	return resp.Response.Data.Accounts, nil
}

// Resources 把套餐折算成统一 ResourceItem 列表（按到期时间升序，早到期优先消耗）。
func (c *Client) Resources(ctx context.Context, a *Auth) ([]meta.ResourceItem, error) {
	accs, err := c.fetchUserResource(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]meta.ResourceItem, 0, len(accs))
	for _, r := range accs {
		tot, used, remain := r.bill()
		if tot == 0 && remain == 0 {
			continue
		}
		f := func(v int64) *float64 { x := float64(v); return &x }
		out = append(out, meta.ResourceItem{
			Name:     orDefault(r.PackageName, "套餐"),
			Total:    f(tot),
			Used:     f(used),
			Remain:   f(remain),
			Unit:     "credits",
			Usable:   remain > 0,
			ExpireAt: r.expireAt(),
			Key:      orDefault(r.PackageName, "pkg") + "|" + r.expireDay(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ExpireAt < out[j].ExpireAt })
	return out, nil
}

// Balance 汇总额度（按最早到期包的剩余量报，余额单位 credits）。
func (c *Client) Balance(ctx context.Context, a *Auth) (*float64, []meta.ResourceItem, error) {
	items, err := c.Resources(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	var sum float64
	for _, it := range items {
		if it.Remain != nil {
			sum += *it.Remain
		}
	}
	return &sum, items, nil
}

// Checkin 每日签到（先探活动状态，再发签到；已签到不发写请求）。
// 返回给用户看的消息（例如「已签到」「获得 X 积分」）。
func (c *Client) Checkin(ctx context.Context, a *Auth) (string, error) {
	h := c.billingHeaders(a)
	// 1) 活动状态：已签到则直接返回，避免重复写请求
	var st map[string]any
	if err := c.doJSON(ctx, http.MethodGet, c.Endpoints.Base+epCheckinStatus, h, nil, &st); err == nil {
		if done, ok := st["todayCheckedIn"].(bool); ok && done {
			return "今日已签到", nil
		}
		if done, ok := st["today_checked_in"].(bool); ok && done {
			return "今日已签到", nil
		}
	}
	// 2) 发签到
	var data map[string]any
	if err := c.doJSON(ctx, http.MethodPost, c.Endpoints.Base+epDailyCheckin, h, map[string]any{}, &data); err != nil {
		if isAlreadyChecked(err) {
			return "今日已签到", nil
		}
		return "", err
	}
	if n := numOf(data, "credit", "today_credit", "credits"); n > 0 {
		return "签到成功，获得 " + trimNum(n) + " 积分", nil
	}
	return "签到成功", nil
}

// isAlreadyChecked 判定「今天已签到」类业务错误（不动主流程）。
func isAlreadyChecked(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "already") || strings.Contains(s, "checked") || strings.Contains(s, "重复")
}

// numOf 从 map 里取第一个存在的数值字段。
func numOf(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case json.Number:
			if f, err := v.Float64(); err == nil {
				return f
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSuffix(v, " credits"), 64); err == nil {
				return f
			}
		}
	}
	return 0
}

// trimNum 去掉小数末尾的 0（10.00 → 10）。
func trimNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// TrimNum 是对外暴露的整数化显示（去掉小数末尾的 0）。
func TrimNum(f float64) string { return trimNum(f) }
