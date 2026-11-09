package wb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 登录实现：WorkBuddy 系是 device_poll 范式（服务端签发 state，插件轮询，无本地端口 → 天然跨机）。
//
// 两区共用一套实现，仅 Base/Origin/DefaultDomain 不同（见 EndpointsFor）。
// 旧版参考：dev/internal/login/login.go（国内版 + 国际版通用）。

// errPending 表示浏览器尚未完成登录（上游业务 code 非 0）。
var errPending = fmt.Errorf("login pending")

// flow 是一次在途登录流程的状态。
//
// W3-20：渠道插件无常驻进程，auth.begin 与后续的 auth.status 落在不同进程里，
// 因此 flow 必须落盘（<WW3_DATA>/creds/workbuddy/flows/<flow_id>.json）。
// 旧版把 flow 放在 daemon 内存 map，撤 daemon 后跨进程查不到，这里改为文件即状态。
// 并发风险很低：一个 flow 只会被 core 单线程轮询（core 侧按 NextPollMS 串行驱动），
// 且写操作是低频的（begin 建文件、status 成功后删文件）。
type flow struct {
	ID        string    `json:"id"`
	Region    string    `json:"region"`
	Proxy     string    `json:"proxy,omitempty"`
	BaseURL   string    `json:"base_url,omitempty"`
	State     string    `json:"state"` // 上游签发的 state
	CreatedAt time.Time `json:"created_at"`
	LastErr   string    `json:"last_err,omitempty"`
}

// LoginManager 管理在途登录流程（落盘，无常驻）。
type LoginManager struct {
	dir   string // <WW3_DATA>/creds/workbuddy/flows
	store *Store
}

// NewLoginManager 创建登录流程管理器。
func NewLoginManager(store *Store) *LoginManager {
	return &LoginManager{dir: filepath.Join(store.Dir, "flows"), store: store}
}

// flowPath 返回某流程的状态文件路径。
func (m *LoginManager) flowPath(id string) string { return filepath.Join(m.dir, id+".json") }

// saveFlow 原子写流程状态。
func (m *LoginManager) saveFlow(f *flow) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dst := m.flowPath(f.ID)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// loadFlow 读流程状态（不存在返回 ok=false）。
func (m *LoginManager) loadFlow(id string) (*flow, bool) {
	if id == "" {
		return nil, false
	}
	b, err := os.ReadFile(m.flowPath(id))
	if err != nil {
		return nil, false
	}
	var f flow
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, false
	}
	return &f, true
}

// dropFlow 删除流程状态文件。
func (m *LoginManager) dropFlow(id string) { _ = os.Remove(m.flowPath(id)) }

// gcFlows 清理 15 分钟前的僵尸流程（begin 时顺手调用，避免目录无限增长）。
func (m *LoginManager) gcFlows() {
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		if f, ok := m.loadFlow(id); ok && time.Since(f.CreatedAt) > 15*time.Minute {
			m.dropFlow(id)
		}
	}
}

// Begin 发起登录：POST /v2/plugin/auth/state?platform=CLI，拿到 state + authUrl，流程落盘。
// 只发起、不轮询（轮询由 core 按 NextPollMS 驱动 auth.status，每次一次进程）。
func (m *LoginManager) Begin(ctx context.Context, region, proxy, baseURL string) (flowID, authURL, userCode string, err error) {
	ep := EndpointsFor(region)
	c, err := NewClient(region, proxy)
	if err != nil {
		return "", "", "", err
	}
	if baseURL != "" {
		ep = OverrideBase(ep, baseURL)
		c.Endpoints = ep
	}
	h := http.Header{}
	commonHeaders(h, ep)
	// 登录发起阶段不允许携带任何身份：显式声明「没有」。
	h.Set("X-No-Authorization", "1")
	h.Set("X-No-User-Id", "1")
	h.Set("X-No-Enterprise-Id", "1")
	h.Set("X-No-Department-Info", "1")

	var data struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := c.doJSON(ctx, http.MethodPost, ep.authStateURL(), h, map[string]any{}, &data); err != nil {
		return "", "", "", err
	}
	if data.State == "" || data.AuthURL == "" {
		return "", "", "", &Error{Kind: "server", Msg: "上游未返回 state 或 authUrl"}
	}
	// 跟随 1~5 跳重定向，让浏览器直开最终登录页（copilot.tencent.com → codebuddy.cn）。
	finalURL := resolveAuthURL(c.HTTP, data.AuthURL, ep)

	f := &flow{ID: randHex(8), Region: NormalizeRegion(region), Proxy: proxy, BaseURL: baseURL,
		State: data.State, CreatedAt: time.Now()}
	if err := m.saveFlow(f); err != nil {
		return "", "", "", err
	}
	m.gcFlows()
	return f.ID, finalURL, "", nil
}

// Status 轮询一次登录状态（幂等，可反复调用；每次都是一次独立进程）。
// 返回 state ∈ waiting_user/success/failed/expired。
func (m *LoginManager) Status(ctx context.Context, flowID string) (state string, account *Auth, hint string, err error) {
	f, ok := m.loadFlow(flowID)
	if !ok {
		return "expired", nil, "登录流程不存在或已过期，请重新发起", nil
	}
	if time.Since(f.CreatedAt) > 10*time.Minute {
		m.dropFlow(flowID)
		return "expired", nil, "登录超时（10 分钟），请重新发起", nil
	}
	// 每区独立 client（cookiejar 不共享，多账号不串会话）
	c, err := NewClient(f.Region, f.Proxy)
	if err != nil {
		return "failed", nil, "", err
	}
	ep := EndpointsFor(f.Region)
	if f.BaseURL != "" {
		ep = OverrideBase(ep, f.BaseURL)
	}
	c.Endpoints = ep
	a, err := pollOnce(ctx, c, ep, f.State)
	if err == errPending {
		return "waiting_user", nil, "请在浏览器中完成登录", nil
	}
	if err != nil {
		f.LastErr = err.Error()
		_ = m.saveFlow(f) // 失败也保留流程（下一次轮询可能恢复），只记录原因
		return "failed", nil, "", err
	}
	// 成功：落盘凭据后清理流程（终态回放由 core 缓存，插件侧无需保留）
	if err := m.store.Save(a); err != nil {
		return "failed", nil, "", err
	}
	m.dropFlow(flowID)
	return "success", a, "登录成功", nil
}

// Cancel 取消在途流程（删除流程文件；不存在也算取消成功，保持幂等）。
func (m *LoginManager) Cancel(flowID string) bool {
	m.dropFlow(flowID)
	return true
}

// pollOnce 查一次 token；未完成返回 errPending。
func pollOnce(ctx context.Context, c *Client, ep Endpoints, state string) (*Auth, error) {
	h := http.Header{}
	commonHeaders(h, ep)
	h.Set("X-No-Authorization", "1")
	h.Set("X-No-User-Id", "1")
	h.Set("X-No-Enterprise-Id", "1")
	h.Set("X-No-Department-Info", "1")

	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := c.doJSON(ctx, http.MethodGet, ep.authTokenURL()+url.QueryEscape(state), h, nil, &tok); err != nil {
		// 上游 pending 表现为 HTTP 200 + 业务 code 非 0（国内版 code!=0、国际版 code=11217）。
		// 沿用旧版 isPending 启发式：错误串里带 "code=" 或 "login"，且不是 http 层/解析失败。
		if isPendingErr(err) {
			return nil, errPending
		}
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errPending
	}
	a := &Auth{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
		Region:       NormalizeRegion(regionOfEndpoints(ep)),
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix() // Unix 秒，对齐旧版凭据格式
	}
	// domain 缺失按区域兜底：国际版必须，否则账号会被判成国内版。
	if a.Domain == "" {
		a.Domain = ep.DefaultDomain
	}
	// 账号资料失败不影响登录（旧版同语义）。
	if acct, err := fetchAccount(ctx, c, ep, state, tok.AccessToken); err == nil {
		a.UID, a.EnterpriseID, a.Nickname = acct.UID, acct.EnterpriseID, acct.Nickname
	}
	if a.UID == "" {
		return nil, &Error{Kind: "account_fault", Msg: "登录成功但上游未返回账号 uid"}
	}
	return a, nil
}

func regionOfEndpoints(ep Endpoints) string {
	if ep.Base == BaseGlobal {
		return RegionGlobal
	}
	return RegionCN
}

// isPendingErr 判定「登录尚未完成」类错误（避免把 pending 当失败终止流程）。
func isPendingErr(err error) bool {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "http ") || strings.Contains(s, "parse") || strings.Contains(s, "解析失败") {
		return false
	}
	return strings.Contains(s, "code=") || strings.Contains(s, "login")
}

// fetchAccount 取账号资料（uid/enterpriseId/nickname）。
func fetchAccount(ctx context.Context, c *Client, ep Endpoints, state, token string) (*Auth, error) {
	h := http.Header{}
	commonHeaders(h, ep)
	h.Set("Authorization", "Bearer "+token)
	var data struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := c.doJSON(ctx, http.MethodGet, ep.loginAcctURL()+url.QueryEscape(state), h, nil, &data); err != nil {
		return nil, err
	}
	return &Auth{UID: data.UID, EnterpriseID: data.EnterpriseID, Nickname: data.Nickname}, nil
}

// resolveAuthURL 手动跟随至多 5 跳重定向，返回浏览器可直接打开的最终地址。
func resolveAuthURL(client *http.Client, raw string, ep Endpoints) string {
	cur, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	noFollow := &http.Client{
		Timeout:       15 * time.Second,
		Transport:     client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for i := 0; i < 5; i++ {
		req, err := http.NewRequest(http.MethodGet, cur.String(), nil)
		if err != nil {
			break
		}
		h := http.Header{}
		commonHeaders(h, ep)
		req.Header = h
		resp, err := noFollow.Do(req)
		if err != nil {
			break
		}
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
			break
		}
		next, err := cur.Parse(loc)
		if err != nil {
			break
		}
		cur = next
	}
	return cur.String()
}

// Refresh 刷新 accessToken（refreshToken 轮换必须写回；缺 expiresIn 保留旧过期时间防刷新风暴）。
func (c *Client) Refresh(ctx context.Context, a *Auth) (*Auth, error) {
	if a.RefreshToken == "" {
		return nil, &Error{Kind: "auth", Msg: "缺少 refreshToken，需要重新登录"}
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := c.doJSON(ctx, http.MethodPost, c.Endpoints.Base+epRefresh, c.refreshHeaders(a), map[string]any{}, &data); err != nil {
		return nil, err
	}
	out := *a
	if data.AccessToken != "" {
		out.AccessToken = data.AccessToken
	}
	if data.RefreshToken != "" {
		out.RefreshToken = data.RefreshToken
	}
	if data.Domain != "" {
		out.Domain = data.Domain
	}
	if data.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(data.ExpiresIn) * time.Second).Unix()
	}
	return &out, nil
}

// Expired 判断凭据是否已过期（留 60 秒余量）。ExpiresAt=0 视为未知，不阻断。
func (a *Auth) Expired() bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return time.Now().Add(60*time.Second).Unix() > a.ExpiresAt
}

// randHex 生成 n 字节随机数的 hex（流程 ID 用）。
func randHex(n int) string {
	b := randBytes(n)
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, hexd[x>>4], hexd[x&0xf])
	}
	return string(out)
}

// jsonRaw 便于把 map 塞进 any 结构。
func jsonRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// itoaM 是 strconv 的小包装（保留给上层拼接消息用）。
func itoaM(n int) string { return strconv.Itoa(n) }
