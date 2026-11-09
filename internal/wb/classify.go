package wb

import (
	"context"
	"errors"
	"net"
	"strings"

	"wild-work3/workbuddy-ext/internal/protocol/errkind"
)

// Error 携带统一错误分类（errkind.Kind），core 据此决定冷却/换号/降级。
type Error struct {
	Kind   errkind.Kind
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

// 注意：这里刻意不实现 `Kind() string`——字段已叫 Kind，Go 不允许同名字段与方法；
// 错误跨 IPC 时走 ErrKindOf 取出枚举写进 jsonl.ErrObj.Kind，RPC 客户端用 rpc.KindOf 读回。

// ErrKindOf 提取统一错误分类。
func ErrKindOf(err error) errkind.Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return errkind.Conn
}

// kindOfNetErr 把传输层错误归为连接类（可换号重试）。
func kindOfNetErr(err error) errkind.Kind {
	if errors.Is(err, context.Canceled) {
		return errkind.Cancel
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errkind.Conn
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return errkind.Conn
	}
	return errkind.Client
}

// sessionDeadMarkers 表示上游会话已失效（需要换号或让用户重新登录）。
var sessionDeadMarkers = []string{
	"offline user session not found",
	"12153",
}

// hardMarkers 是额度耗尽（不该重试同号，但可换号）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required",
	"credit not enough", "not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// softMarkers 是限流（冷却后重试同号）。
var softMarkers = []string{
	"rate limit", "rate-limiting", "too many requests", "usage limit",
	"请求过于频繁", "限流",
}

// contentBlockedMarkers 是内容拦截（不换号，直接透传）。
var contentBlockedMarkers = []string{
	"blocked by security policy", "unapproved channel", "illegal api invocation",
}

// Classify 把上游 HTTP 状态 + 错误文本归到统一 ErrKind。
// 顺序关键（照旧版实测结论，勿随意调换）：
//   - 402/401 先判断（401 是会话失效，早于任何文案匹配）
//   - 429 里 14018「credits exhausted」是额度而非限流
//   - conversation id missing 必须排在 contentBlocked 之前（R116）
func Classify(status int, body string) errkind.Kind {
	lb := strings.ToLower(body)

	if status == 402 {
		return errkind.HardCredit
	}
	for _, m := range sessionDeadMarkers {
		if strings.Contains(lb, m) {
			return errkind.SessionDead
		}
	}
	if status == 401 {
		return errkind.SessionDead
	}
	if status == 429 {
		// 14018 后必须紧跟非数字，避免 140180 误伤
		if hasCode(body, 14018) || strings.Contains(lb, "credits exhausted") {
			return errkind.HardCredit
		}
		return errkind.SoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lb, m) {
			return errkind.HardCredit
		}
	}
	for _, m := range softMarkers {
		if strings.Contains(lb, m) {
			return errkind.SoftRate
		}
	}
	if status == 404 {
		return errkind.NotFound
	}
	if status >= 500 {
		return errkind.Server
	}
	if status >= 400 {
		// 会话头族缺失（国际版 2026-10-16 起抽样校验）——账号态问题，不是内容拦截
		if strings.Contains(lb, "conversation id missing") {
			return errkind.AccountFault
		}
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lb, m) {
				return errkind.ContentBlocked
			}
		}
		if (status == 400 || status == 404) &&
			(strings.Contains(lb, "prompt is too long") || hasCode(body, 11115)) {
			return errkind.PromptTooLong
		}
		if status == 403 && !strings.Contains(body, "\"code\"") {
			return errkind.WafBlock
		}
		if hasCode(body, 11101) || strings.Contains(lb, "unmarshal chat params failed") {
			return errkind.Client
		}
		if strings.Contains(lb, "request illegal") || strings.Contains(lb, "trial not activated") {
			return errkind.AccountFault
		}
		return errkind.Client
	}
	return errkind.NotFound
}

// hasCode 判断 body 里是否出现 `"code":<n>`（其后不得紧跟数字，防前缀误伤）。
func hasCode(body string, n int) bool {
	key := "\"code\":" + itoa(n)
	i := strings.Index(body, key)
	if i < 0 {
		return false
	}
	j := i + len(key)
	if j < len(body) {
		c := body[j]
		if c >= '0' && c <= '9' {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
