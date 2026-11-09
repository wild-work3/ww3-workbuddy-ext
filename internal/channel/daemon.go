// Package channel 是 workbuddy-ext 的插件实现：控制面 + 会话面同一份方法表。
//
// 双区合并：一个插件同时服务国内版（前缀 workbuddy）与国际版（前缀 workbuddyai），
// 由渠道配置的 region 决定实例属于哪个区；两区共用 internal/wb 的上游实现。
//
// 运行形态（W3-20）：`call` 一次性（stdin 一条 invoke → 事件 + result → 退出）
// 与 `stdio` 常连；两者共用 registry()，core 一律用 call，无常驻进程。
package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"wild-work3/workbuddy-ext/internal/protocol/authflow"
	"wild-work3/workbuddy-ext/internal/protocol/chat"
	"wild-work3/workbuddy-ext/internal/protocol/cli"
	"wild-work3/workbuddy-ext/internal/protocol/errkind"
	"wild-work3/workbuddy-ext/internal/protocol/meta"
	"wild-work3/workbuddy-ext/internal/protocol/plugin"
	"wild-work3/workbuddy-ext/internal/protocol/stat"
	"wild-work3/workbuddy-ext/internal/wb"
)

// PluginID 是插件 ID（两个前缀共用同一个插件，靠 config.region 区分实例）。
const PluginID = "workbuddy"

// Log 是插件进程级日志器（main 里初始化；nil 时静默）。
var Log *cli.Logger

// Config 是 core 通过 channel_config 下发的渠道配置。
type Config struct {
	Region  string `json:"region"`             // workbuddy（国内版）| workbuddyai（国际版）
	Proxy   string `json:"proxy,omitempty"`    // 代理
	Timeout int    `json:"timeout,omitempty"`  // 秒
	BaseURL string `json:"base_url,omitempty"` // 高级：覆盖上游基址（联调/自建反代用）
}

// regionPrefix 返回该配置对应的模型命名空间前缀。
func (c Config) regionPrefix() string { return wb.NormalizeRegion(c.Region) }

// isGlobal 该配置是否国际版。
func (c Config) isGlobal() bool { return wb.IsGlobal(c.Region) }

// Manifest 返回插件清单（双前缀在 models 上体现，插件 ID 只有一个）。
func Manifest() meta.Manifest {
	return meta.Manifest{
		ID:   PluginID,
		Name: "WorkBuddy（国内版 / 国际版）",
		DefaultChannels: []meta.DefaultChannel{
			{ID: "workbuddy", Name: "WorkBuddyCN", Config: map[string]any{"region": "workbuddy"}},
			{ID: "workbuddyai", Name: "WorkBuddyAI", Config: map[string]any{"region": "workbuddyai"}},
		},
		Version: "1.0.0",
		Kind:    "channel",
		Protocol: meta.ProtocolRng{
			Min: 1, Max: 1,
		},
		Execution: meta.Execution{
			// W3-20：控制面与会话面都走一次性进程（core 每次调用 spawn 一个，结束即回收）。
			// MaxWorkers 限制同一插件的并发进程数，防止面板批量操作瞬间拉起一串进程。
			Control:            plugin.ModeCall,
			Conversation:       plugin.ModeCall,
			MaxWorkers:         4,
			IdleTimeoutSeconds: 0, // 无常驻进程，无空闲回收概念
		},
		Capabilities: meta.Capabilities{
			Control: []string{
				"models.list", "config.schema", "config.get", "config.set",
				"auth.methods", "auth.begin", "auth.status", "auth.finish", "auth.cancel",
				"accounts.list", "accounts.resource_detail", "accounts.refresh", "accounts.checkin",
				"credits.get", "stats.get", "health.check", "ping",
			},
			Conversation: []string{"chat.generate"},
		},
		Icon:  "workbuddy",
		Color: "#2f6bff",
		Description: "WorkBuddy 双区渠道：扫码登录（设备流 state 轮询），" +
			"国内版前缀 workbuddy、国际版前缀 workbuddyai。",
	}
}

// Daemon 是控制面状态（凭据仓库 + 登录流程目录 + 统计）。
//
// W3-20：没有常驻进程，所以这里只持有「磁盘位置」，不缓存配置——
// 每次调用的 config 都由 core 在 params 里下发。
type Daemon struct {
	store *wb.Store
	login *wb.LoginManager
	stat  *stat.Store
}

// NewDaemon 创建控制面（dataDir 由 core 通过 --data 注入）。
func NewDaemon(dataDir string) *Daemon {
	st := wb.NewStore(dataDir)
	return &Daemon{store: st, login: wb.NewLoginManager(st), stat: stat.Open(dataDir)}
}

// Serve 运行插件（call 或 stdio 两种形态，共用同一份方法表）。
//
// dataDir 是插件数据目录（<data>/{auths,logs,stat}）；core 通过 --data 传入插件根目录。
func Serve(ctx context.Context, mode, dataDir string, in io.Reader, out io.Writer) error {
	r := registry(NewDaemon(dataDir))
	switch mode {
	case plugin.ModeCall:
		return r.ServeOnce(ctx, in, out)
	default:
		return r.ServeStdio(ctx, in, out)
	}
}

// registry 组装完整方法表（控制面 + 会话面；两种运行形态共用）。
func registry(d *Daemon) *plugin.Registry {
	r := plugin.New()
	// 生命周期四件套（initialize/capabilities.list/ping/health.check/shutdown）统一由 SDK 注册
	r.Lifecycle(Manifest())

	r.Handle("config.schema", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(configSchema()), nil
	})
	// 配置随每次调用下发（渠道配置 core 持有），插件侧无常驻状态
	r.Handle("config.get", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(map[string]any{}), nil
	})
	r.Handle("config.set", func(ctx context.Context, _ Raw) (Raw, *Err) {
		return mustJSON(map[string]any{"ok": true}), nil
	})

	// ---- 认证（authflow 六范式：本渠道是 device_poll）----
	r.Handle("auth.methods", d.authMethods)
	r.Handle("auth.begin", d.authBegin)
	r.Handle("auth.status", d.authStatus)
	r.Handle("auth.finish", d.authFinish)
	r.Handle("auth.cancel", d.authCancel)

	// ---- 账号 / 模型 / 额度 ----
	r.Handle("accounts.list", d.accountsList)
	r.Handle("accounts.resource_detail", d.resourceDetail)
	r.Handle("accounts.refresh", d.accountRefresh)
	r.Handle("accounts.checkin", d.accountCheckin)
	r.Handle("models.list", d.modelsList)
	r.Handle("credits.get", d.creditsGet)
	r.Handle("stats.get", d.statsGet)

	// ---- 会话面：流式对话 ----
	r.HandleStream("chat.generate", d.generate)
	return r
}

// ---- 认证实现 ----

// authMethods 声明本插件支持的登录方式。两种区域各一条，均为 device_poll。
//
// device_poll 的要点（AUTH_FLOW.md §2）：插件自轮询上游 state，不监听本地端口，
// 因此天然可跨机（远端浏览器登录，网关侧轮询拿 token）。
func (d *Daemon) authMethods(ctx context.Context, p Raw) (Raw, *Err) {
	region := wb.NormalizeRegion(regionOf(p))
	ms := []authflow.Method{
		{
			ID:          "device",
			Type:        authflow.TypeDevicePoll,
			Label:       map[bool]string{true: "扫码登录（国际版）", false: "扫码登录（国内版）"}[wb.IsGlobal(region)],
			Description: "在浏览器打开登录页完成登录，插件自动轮询获取凭据；可在任意机器上完成登录。",
			Region:      region,
			Default:     true,
		},
	}
	return mustJSON(authflow.MethodsResult{
		Region:  region,
		Methods: ms,
		Note:    "登录成功后凭据保存在插件私有目录，不会经 IPC 传输。",
	}), nil
}

func (d *Daemon) authBegin(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.BeginRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "参数解析失败: %v", err)
	}
	region := wb.NormalizeRegion(orDefault(req.Region, regionOf(p)))
	cfg := parseConfig(p)
	flowID, authURL, userCode, err := d.login.Begin(ctx, region, cfg.Proxy, cfg.BaseURL)
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "发起登录失败: %v", err)
	}
	return mustJSON(authflow.BeginResult{
		FlowID:           flowID,
		Type:             authflow.TypeDevicePoll,
		Method:           orDefault(req.Method, "device"),
		AuthURL:          authURL,
		UserCode:         userCode,
		State:            string(authflow.StateWaitingUser),
		Prompt:           "请在弹出的页面完成登录，完成后回到面板即可",
		CanPasteCallback: false, // device_poll 无回调，不需要粘贴兜底
		NextPollMS:       2000,
		ExpiresAt:        time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	}), nil
}

func (d *Daemon) authStatus(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.StatusRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "参数解析失败: %v", err)
	}
	state, acct, hint, err := d.login.Status(ctx, req.FlowID)
	if err != nil {
		k := wb.ErrKindOf(err)
		return mustJSON(authflow.StatusResult{
			State: authflow.StateFailed,
			Hint:  hint,
			Error: &authflow.Error{Kind: string(k), Message: err.Error()},
		}), nil
	}
	res := authflow.StatusResult{State: authflow.State(state), Hint: hint}
	if res.State == authflow.StatePending || res.State == authflow.StateWaitingUser {
		res.State = authflow.StateWaitingUser
		res.NextPollMS = 2000
	}
	if acct != nil {
		// 只上报账号事实，凭据永不过 IPC（W3-07）。
		// auth_file 是账号的唯一稳定标识（两区可能同名 uid，见 PLUGIN_CLI §5.2）。
		res.Account = &authflow.Account{
			UID:       acct.UID,
			Nickname:  orDefault(acct.Nickname, acct.UID),
			ChannelID: PluginID,
			Region:    acct.RegionOf(),
			ExpiresAt: acct.ExpiresAtText(),
			LoginKind: "device_poll",
			AuthFile:  authFileOf(acct),
		}
	}
	return mustJSON(res), nil
}

// authFinish 是人工兜底入口。device_poll 无需粘贴回调，这里保持契约形状并明确拒绝。
func (d *Daemon) authFinish(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.FinishRequest
	_ = json.Unmarshal(p, &req)
	return nil, errf(errkind.BadParams, errkind.ScopeRequest,
		"该登录方式（device_poll）不需要粘贴回调地址，请等待自动轮询完成")
}

func (d *Daemon) authCancel(ctx context.Context, p Raw) (Raw, *Err) {
	var req authflow.CancelRequest
	_ = json.Unmarshal(p, &req)
	ok := d.login.Cancel(req.FlowID)
	return mustJSON(map[string]any{"ok": ok}), nil
}

// ---- 账号 / 模型 / 额度 ----

// accountsList 上报账号事实（凭据内容绝不上报）。
//
// uid 字段填**凭据标识**（auths/ 文件名去后缀）而不是上游 uid：
// 同一自然人国内版与国际版 uid 相同，只有掺了区前缀的文件名才唯一。
// 上游 uid 放在 nickname 之外的 extra 语义里没有位置，故用 RemoteUID 字段——
// meta.AccountInfo 没有该字段，因此这里 uid=标识、并用 nickname 兜底展示。
func (d *Daemon) accountsList(ctx context.Context, p Raw) (Raw, *Err) {
	cfgu := parseConfig(p)
	out := []meta.AccountInfo{}
	for _, a := range d.store.List() {
		if cfgu.Region != "" && a.RegionOf() != cfgu.regionPrefix() {
			continue // 只报该实例所属区域的账号
		}
		status := "ok"
		if a.Expired() {
			status = "expired"
		}
		out = append(out, meta.AccountInfo{
			UID:       authFileOf(&a),
			Nickname:  orDefault(a.Nickname, a.UID),
			Status:    status,
			LoginKind: "device_poll",
			ExpiresAt: a.ExpiresAtText(),
			ChannelID: PluginID,
			Region:    a.RegionOf(),
			Prefixes:  []string{a.RegionOf()}, // 该账号只服务自己那一区的模型命名空间
			AuthFile:  authFileOf(&a),
		})
	}
	return mustJSON(map[string]any{"accounts": out}), nil
}

// authFileOf 返回凭据标识（登录流程刚落盘时就已有 AuthFile，缺省则按命名规则算）。
func authFileOf(a *wb.Auth) string { return wb.AuthFileName(a) }

// account 取出（必要时刷新）指定账号；name 为空时取该区域第一个账号。
//
// name 是**凭据标识**（auths/ 文件名去后缀，即 core 的 account_uid），
// 也容忍直接传上游 uid（兼容旧调用与手工调试）。
func (d *Daemon) account(ctx context.Context, cfg Config, name string) (*wb.Auth, *wb.Client, error) {
	reg := cfg.regionPrefix()
	var picked *wb.Auth
	if name != "" {
		if a, err := d.store.Load(name); err == nil && a.RegionOf() == reg {
			picked = a
		}
	}
	if picked == nil {
		for _, a := range d.store.List() {
			if a.RegionOf() != reg {
				continue
			}
			if name != "" && a.AuthFile != name {
				continue
			}
			cp := a
			picked = &cp
			break
		}
	}
	if picked == nil {
		return nil, nil, &wb.Error{Kind: errkind.Auth, Msg: "该渠道尚未登录，请先在面板完成登录"}
	}
	c, err := wb.NewClient(reg, cfg.Proxy)
	if err != nil {
		return nil, nil, err
	}
	if cfg.BaseURL != "" {
		c.Endpoints = wb.OverrideBase(c.Endpoints, cfg.BaseURL)
	}
	if picked.Expired() && picked.RefreshToken != "" {
		na, err := c.Refresh(ctx, picked)
		if err != nil {
			return nil, nil, err
		}
		if err := d.store.Save(na); err != nil {
			Log.Errorf("刷新后写回凭据失败: %v", err)
		}
		picked = na
	}
	return picked, c, nil
}

// modelsList 拉模型目录，模型 ID 带区域前缀（workbuddy/xxx 或 workbuddyai/xxx）。
func (d *Daemon) modelsList(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	acct, c, err := d.account(ctx, cfg, "")
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "%v", err)
	}
	ms, err := c.FetchModels(ctx, acct)
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "拉取模型失败: %v", err)
	}
	prefix := cfg.regionPrefix()
	for i := range ms {
		ms[i].ID = wb.PublicModelID(prefix, ms[i].ID)
	}
	return mustJSON(map[string]any{"models": ms}), nil
}

// resourceDetail 上报额度明细（ResourceItem 裸数组，前端直接渲染）。
func (d *Daemon) resourceDetail(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	uid := uidOf(p)
	acct, c, err := d.account(ctx, cfg, uid)
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "%v", err)
	}
	items, err := c.Resources(ctx, acct)
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "查询额度失败: %v", err)
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	return mustJSON(items), nil
}

// creditsGet 返回余额与明细（null = 上游未提供 ≠ 0）。
func (d *Daemon) creditsGet(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	uid := uidOf(p)
	acct, c, err := d.account(ctx, cfg, uid)
	if err != nil {
		return mustJSON(map[string]any{"balance": nil, "items": []meta.ResourceItem{}}), nil
	}
	balance, items, err := c.Balance(ctx, acct)
	if err != nil {
		return mustJSON(map[string]any{"balance": nil, "items": []meta.ResourceItem{}}), nil
	}
	if items == nil {
		items = []meta.ResourceItem{}
	}
	return mustJSON(map[string]any{"balance": balance, "items": items, "unit": "credits"}), nil
}

// accountRefresh 刷新账号（token 续期 + 额度回读）。
func (d *Daemon) accountRefresh(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	uid := uidOf(p)
	acct, c, err := d.account(ctx, cfg, uid)
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "%v", err)
	}
	if acct.RefreshToken != "" {
		na, err := c.Refresh(ctx, acct)
		if err != nil {
			k := wb.ErrKindOf(err)
			return nil, errf(string(k), scopeOfKind(k), "刷新凭据失败: %v", err)
		}
		if err := d.store.Save(na); err != nil {
			return nil, errf(errkind.AccountFault, errkind.ScopeAccount, "写回凭据失败: %v", err)
		}
	}
	msg := "已刷新"
	if bal, _, err := c.Balance(ctx, acct); err == nil && bal != nil {
		msg = fmt.Sprintf("已刷新，剩余 %s 积分", wb.TrimNum(*bal))
	}
	return mustJSON(map[string]any{"ok": true, "message": msg}), nil
}

// accountCheckin 每日签到（uid 为空则对该区所有账号逐个签到）。
func (d *Daemon) accountCheckin(ctx context.Context, p Raw) (Raw, *Err) {
	cfg := parseConfig(p)
	uid := uidOf(p)
	if !cfg.isGlobal() && cfg.Region == "" {
		cfg.Region = wb.RegionCN
	}
	var targets []wb.Auth
	for _, a := range d.store.List() {
		if a.RegionOf() != cfg.regionPrefix() {
			continue
		}
		if uid != "" && a.AuthFile != uid && a.UID != uid {
			continue
		}
		targets = append(targets, a)
	}
	if len(targets) == 0 {
		return nil, errf(errkind.Auth, errkind.ScopeAccount, "该渠道尚未登录，请先在面板完成登录")
	}
	var msgs []string
	var failed int
	for i := range targets {
		acct, c, err := d.account(ctx, cfg, targets[i].AuthFile)
		if err != nil {
			failed++
			msgs = append(msgs, targets[i].UID+": "+err.Error())
			continue
		}
		m, err := c.Checkin(ctx, acct)
		if err != nil {
			failed++
			msgs = append(msgs, orDefault(acct.Nickname, acct.UID)+": "+err.Error())
			continue
		}
		msgs = append(msgs, orDefault(acct.Nickname, acct.UID)+": "+m)
	}
	if failed == len(targets) {
		return nil, errf(errkind.Client, errkind.ScopeAccount, "签到失败: %s", strings.Join(msgs, "; "))
	}
	return mustJSON(map[string]any{"ok": true, "message": strings.Join(msgs, "; ")}), nil
}

// ---- 配置 schema ----
func configSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"region"},
		"properties": map[string]any{
			"region": map[string]any{
				"type":  "string",
				"title": "区域",
				"enum":  []string{wb.RegionCN, wb.RegionGlobal},
				"enumNames": []string{
					"国内版（前缀 workbuddy，copilot.tencent.com）",
					"国际版（前缀 workbuddyai，www.workbuddy.ai）",
				},
				"default": wb.RegionCN,
				"description": "该渠道实例所属区域；模型 ID 会带对应前缀（如 workbuddyai/hy3）。" +
					"同一区域可添加多个渠道实例以支持多账号。",
			},
			"proxy":    map[string]any{"type": "string", "title": "代理", "description": "如 http://127.0.0.1:10809"},
			"timeout":  map[string]any{"type": "integer", "title": "超时（秒）", "default": 300},
			"base_url": map[string]any{"type": "string", "title": "上游基址覆盖", "description": "高级：联调/自建反代用，留空使用官方地址"},
		},
	}
}

// ---- 小工具 ----

func parseConfig(p Raw) Config {
	var wrap struct {
		Config Config `json:"config"`
	}
	if len(p) > 0 {
		_ = json.Unmarshal(p, &wrap)
	}
	if wrap.Config.Timeout <= 0 {
		wrap.Config.Timeout = 300
	}
	if wrap.Config.Region == "" {
		wrap.Config.Region = wb.RegionCN
	}
	return wrap.Config
}

func regionOf(p Raw) string { return parseConfig(p).Region }

func uidOf(p Raw) string {
	var wrap struct {
		UID string `json:"uid"`
	}
	_ = json.Unmarshal(p, &wrap)
	return wrap.UID
}

func mustJSON(v any) Raw {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ---- 统计（W3-22 §6.3：能力 stats.get → stat.Summary）----

// statsGet 返回本渠道自己的用量摘要（渠道事实，供 core 聚合展示）。
//
// 口径严格执行 PLUGIN_CLI §6.3：只统计 kind=chat；未上报的 token 是 -1，不进分母。
// 双区双实例共享同一个 stat 目录，所以还要按 region 过滤：否则国内版渠道会
// 把国际版的用量也算进去（同一个插件、不同渠道实例）。
func (d *Daemon) statsGet(ctx context.Context, p Raw) (Raw, *Err) {
	var req struct {
		Days int `json:"days"`
	}
	_ = json.Unmarshal(p, &req)
	if req.Days <= 0 {
		req.Days = 7
	}
	if d.stat == nil {
		return mustJSON(stat.Summary{Days: []stat.Day{}}), nil
	}
	recs := d.stat.Query(req.Days)
	// 有 region 配置时只算本区（stats.jsonl 里已有 region 字段，直接过滤，不另存目录）
	if reg := parseConfig(p).regionPrefix(); reg != "" {
		kept := recs[:0]
		for _, r := range recs {
			if r.Region != "" && r.Region != reg {
				continue
			}
			kept = append(kept, r)
		}
		recs = kept
	}
	return mustJSON(stat.Summarize(recs)), nil
}

// recordChat 把一次对话结果写进 stat 流水（失败不阻断业务，fail-open）。
func (d *Daemon) recordChat(acct *wb.Auth, model string, u *chat.Usage, ok bool, kind string) {
	if d.stat == nil {
		return
	}
	r := stat.Record{Kind: stat.KindChat, Channel: PluginID, Model: model, OK: ok, ErrKind: kind}
	if acct != nil {
		r.Account = authFileOf(acct)
		r.Region = acct.RegionOf()
	}
	if u != nil {
		r.PromptTokens, r.CompletionTokens, r.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
		r.TimeMs, r.FirstTokenMs = int(u.TimeMs), int(u.FirstTokenMs)
		if u.Cache != nil {
			r.CachedTokens = u.Cache.Cached
			r.CachedReported = u.Cache.CachedReported || u.Cache.WriteReported
		}
	}
	d.stat.Append(r)
}

// scopeOfKind 把错误类别映射成作用域（core 据此决定冷却粒度）。
func scopeOfKind(k errkind.Kind) errkind.Scope {
	switch k {
	case errkind.HardCredit, errkind.SoftRate, errkind.SessionDead, errkind.Auth, errkind.AccountFault:
		return errkind.ScopeAccount
	case errkind.NotFound, errkind.ModelBlocked, errkind.PromptTooLong:
		return errkind.ScopeModel
	case errkind.Server, errkind.WafBlock, errkind.Conn:
		return errkind.ScopeChannel
	}
	return errkind.ScopeRequest
}
