package channel

import (
	"context"
	"encoding/json"

	"wild-work3/workbuddy-ext/internal/protocol/chat"
	"wild-work3/workbuddy-ext/internal/protocol/errkind"
	"wild-work3/workbuddy-ext/internal/protocol/plugin"
	"wild-work3/workbuddy-ext/internal/wb"
)

// generate 执行一次上游流式对话，逐事件转发（W3-13 归一化事件）。
//
// 一请求一进程（W3-20）：凭据从插件私有目录按 core 下发的 account_uid 取，
// core 既不持有也不经 IPC 传凭据（W3-07）。
func (d *Daemon) generate(ctx context.Context, p Raw, emit plugin.Emit) (Raw, *Err) {
	var req chat.GenerateRequest
	if err := json.Unmarshal(p, &req); err != nil {
		return nil, errf(errkind.BadParams, errkind.ScopeRequest, "请求体解析失败: %v", err)
	}
	var cfg Config
	if len(req.ChannelConfig) > 0 {
		if err := json.Unmarshal(req.ChannelConfig, &cfg); err != nil {
			return nil, errf(errkind.BadParams, errkind.ScopeRequest, "渠道配置解析失败: %v", err)
		}
	}
	if cfg.Region == "" {
		cfg.Region = wb.NormalizeRegion(req.Model) // 前缀即区域（workbuddy/xxx、workbuddyai/xxx）
	}
	// 双区隔离校验：请求前缀与渠道实例区域不一致时直接报错。
	// 不能默默用本区账号去跑对方的模型——那会把用量/额度记到错的区，
	// 而两个区是两套独立配额（登录也是两套凭据）。
	if pfx := wb.ModelPrefixOf(req.Model); pfx != "" && pfx != cfg.regionPrefix() {
		return nil, errf(errkind.ModelBlocked, errkind.ScopeModel,
			"模型 %s 属于 %s 区，当前渠道实例是 %s 区，请把模型路由到对应渠道",
			req.Model, pfx, cfg.regionPrefix())
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 300
	}

	acct, c, err := d.account(ctx, cfg, req.AccountUID)
	if err != nil {
		k := wb.ErrKindOf(err)
		return nil, errf(string(k), scopeOfKind(k), "%v", err)
	}
	if Log != nil {
		// 零 token 落盘红线：只记 uid/nickname/model，绝不打印 accessToken。
		Log.Infof("上游对话 uid=%s region=%s model=%s msgs=%d tools=%d",
			acct.UID, acct.RegionOf(), req.Model, len(req.Messages), len(req.Tools))
	}

	// plugin.Emit 的 payload 是 any，wb.Emitter 是 map；这里桥接一层（插件内部统一用 map 组装字段）。
	u, err := c.ChatStream(ctx, acct, &req, wb.Emitter(func(event string, payload map[string]any) error {
		return emit(event, payload)
	}))
	if err != nil {
		if ctx.Err() != nil {
			return nil, errf(errkind.Cancel, errkind.ScopeRequest, "请求已取消")
		}
		k := wb.ErrKindOf(err)
		// 已发出正文后不得重放（W3-10）：core 负责判定，插件只如实上报错误类别。
		d.recordChat(acct, req.Model, u, false, string(k))
		return nil, errf(string(k), scopeOfKind(k), "%v", err)
	}
	if u == nil {
		u = &chat.Usage{}
	}
	d.recordChat(acct, req.Model, u, true, "")
	return mustJSON(u), nil
}
