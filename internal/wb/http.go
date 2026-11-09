package wb

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// newTransport 与旧版一致：禁用 HTTP/2（上游对 h2 行为不稳），并给出保守的
// 连接/响应头超时；流式请求靠 idle 看门狗而非整体 Timeout。
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 禁 h2
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Client 是一次渠道会话的上游客户端（每个 worker 进程独立，不共享连接池状态）。
type Client struct {
	HTTP      *http.Client // 普通 JSON 请求（30s~120s）
	Stream    *http.Client // 流式请求：Timeout=0，整体由 ctx + idle 看门狗控制
	Idle      time.Duration
	Endpoints Endpoints
	Region    string
	Proxy     string
}

// NewClient 按区域与代理构造客户端。
func NewClient(region, proxy string) (*Client, error) {
	ep := EndpointsFor(region)
	tr := newTransport()
	if proxy != "" {
		u, err := parseProxy(proxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &Client{
		HTTP:      &http.Client{Transport: tr, Timeout: 120 * time.Second},
		Stream:    &http.Client{Transport: tr, Timeout: 0},
		Idle:      300 * time.Second,
		Endpoints: ep,
		Region:    NormalizeRegion(region),
		Proxy:     proxy,
	}, nil
}

// Auth 是一个账号的凭据事实（绝不写日志，绝不进 IPC 明文）。
//
// 字段名与旧版 wild-work.exe 的 auths/*.json 完全一致（PLUGIN_CLI §5.1），
// 两版可以互换读写同一个文件：
//   - ExpiresAt 是 **Unix 秒**（旧版就是这样；早期 ww3 写成 RFC3339 属不兼容，已改正）
//   - 嵌套形外层是 {"auth":{...},"account":{...}}，见 authstore.go
//
// AuthFile 是本地凭据标识（文件名去后缀），不写进 JSON，运行时由 Store 回填。
type Auth struct {
	AccessToken   string `json:"accessToken"`
	RefreshToken  string `json:"refreshToken"`
	ExpiresAt     int64  `json:"expiresAt"` // Unix 秒；0 = 未知
	Domain        string `json:"domain"`
	APIHost       string `json:"apiHost"`
	MachineID     string `json:"machineId"`
	DeviceID      string `json:"deviceId"`
	MachineToken  string `json:"machineToken"`
	MachineType   string `json:"machineType"`
	SigningSecret string `json:"signingSecret"`
	ConsoleCookie string `json:"consoleCookie"`
	BaizhiCookie  string `json:"baizhiCookie"`
	UID           string `json:"uid"`
	EnterpriseID  string `json:"enterpriseId"`
	Nickname      string `json:"nickname"`
	Region        string `json:"region,omitempty"` // 本地派生（不属于上游凭据，旧版靠文件名区分）
	AuthFile      string `json:"-"`                // 本地凭据标识（文件名）
}

// ExpiresAtText 把 Unix 秒格式化成 RFC3339（供上报给 core 展示）；0 返回空串（未知）。
func (a *Auth) ExpiresAtText() string {
	if a.ExpiresAt <= 0 {
		return ""
	}
	return time.Unix(a.ExpiresAt, 0).Format(time.RFC3339)
}

// Unixtime 便于外部直接读秒值。
func (a *Auth) Unixtime() int64 { return a.ExpiresAt }

// Region 返回该凭据实际所属区域（domain 优先，缺省用登录时的区域）。
func (a *Auth) RegionOf() string {
	d := strings.ToLower(a.Domain)
	if strings.Contains(d, "workbuddy.ai") {
		return RegionGlobal
	}
	if d != "" {
		return RegionCN
	}
	return NormalizeRegion(a.Region)
}

// commonHeaders 是所有请求共用的身份头（CLI 形态，R117）。
func commonHeaders(h http.Header, ep Endpoints) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-CodeBuddy-Request", "1")
	h.Set("Origin", ep.Origin)
	h.Set("Referer", ep.Origin+"/")
	h.Set("User-Agent", clientUA)
	if ep.Base == BaseGlobal {
		h.Set("Accept-Language", "en-US,en;q=0.9")
	} else {
		h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	}
}

// chatHeaders 组装对话请求头：鉴权 + 租户 + 归属标识 + 会话头族。
// 安全红线：绝不携带 X-Refresh-Token。
func (c *Client) chatHeaders(a *Auth, conv, turn string) http.Header {
	h := http.Header{}
	commonHeaders(h, c.Endpoints)
	if a.AccessToken != "" {
		h.Set("Authorization", "Bearer "+a.AccessToken)
	} else {
		h.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	} else {
		h.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		h.Set("X-No-Enterprise-Id", "1")
	}
	if a.Domain != "" {
		h.Set("X-Domain", a.Domain)
	} else {
		h.Set("X-No-Department-Info", "1")
	}
	injectAttribution(h)
	injectConversationHeaders(h, a.UID, conv, turn)
	return h
}

// billingHeaders 是计费接口的头（与对话头不同：少归属与会话族，多 X-Tenant-Id）。
func (c *Client) billingHeaders(a *Auth) http.Header {
	h := http.Header{}
	commonHeaders(h, c.Endpoints)
	h.Set("Authorization", "Bearer "+a.AccessToken)
	if a.UID != "" {
		h.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", a.EnterpriseID)
		h.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		h.Set("X-Domain", a.Domain)
	}
	return h
}

// refreshHeaders 只有刷新端点才带 X-Refresh-Token。
func (c *Client) refreshHeaders(a *Auth) http.Header {
	h := http.Header{}
	commonHeaders(h, c.Endpoints)
	h.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	h.Set("X-Auth-Refresh-Source", "plugin")
	return h
}

// injectAttribution 是归属标识族（上游据此判定客户端身份，缺失可能被 WAF 拦）。
func injectAttribution(h http.Header) {
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Purpose", "conversation")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", clientVer)
	h.Set("X-Product", "SaaS")
	h.Set("X-Private-Data", "true")
}

// apiEnvelope 是上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次 JSON 请求并解信封；HTTP >= 300 与 code != 0 都转成 *Error。
func (c *Client) doJSON(ctx context.Context, method, fullURL string, h http.Header, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return err
	}
	req.Header = h
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return &Error{Kind: kindOfNetErr(err), Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: fmt.Sprintf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 300))}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return &Error{Kind: kindOfNetErr(err), Status: resp.StatusCode, Msg: "响应解析失败: " + truncate(string(raw), 200)}
	}
	if env.Code != 0 {
		return &Error{Kind: Classify(resp.StatusCode, env.Msg), Status: resp.StatusCode,
			Msg: fmt.Sprintf("code=%d msg=%s", env.Code, env.Msg)}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return &Error{Kind: kindOfNetErr(err), Msg: "data 解析失败: " + truncate(string(env.Data), 200)}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// parseProxy 解析代理地址，非法时返回可读错误（由调用方归类成 BadParams）。
func parseProxy(p string) (*url.URL, error) {
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return nil, &Error{Kind: "bad_params", Msg: "代理地址非法: " + p}
	}
	return u, nil
}
