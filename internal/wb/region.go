// Package wb 是 WorkBuddy 双区（国内版 codebuddy / 国际版 workbuddy.ai）共用的上游实现。
//
// 一个实现 + Region 切 host/Origin/DefaultDomain，避免旧版把国内版放在
// internal/upstream、国际版放在 internal/workbuddyai 的两份重复代码。
package wb

import (
	"strings"
	"time"
)

// 区域标识（同时是模型命名空间前缀）。
const (
	RegionCN     = "workbuddy"   // 国内版：copilot.tencent.com / codebuddy.cn
	RegionGlobal = "workbuddyai" // 国际版：www.workbuddy.ai
)

// 上游基址。
const (
	BaseCN     = "https://copilot.tencent.com"
	BaseGlobal = "https://www.workbuddy.ai"
	// OriginCN 是国内版的浏览器站点（登录页与 Origin/Referer）。
	OriginCN     = "https://www.codebuddy.cn"
	OriginGlobal = "https://www.workbuddy.ai"

	// clientUA 与聊天链同一客户端身份（官方桌面 5.8.3 内嵌 CLI 2.161.0，R117）。
	clientUA = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
	// clientVer 是 X-IDE-Version 取值。
	clientVer = "5.8.3"
	// productCode 是计费接口的产品码。
	productCode = "p_tcaca"
)

// Endpoints 一次请求/登录流程用到的端点集合。
type Endpoints struct {
	Base          string // API 基址
	Origin        string // 浏览器站点（Origin/Referer）
	DefaultDomain string // token 响应缺 domain 时的兜底（国际版必须非空，否则账号被判 CN）
}

// EndpointsFor 按区域返回端点：region 为 workbuddyai/global 走国际版，其余走国内版。
func EndpointsFor(region string) Endpoints {
	if IsGlobal(region) {
		return Endpoints{Base: BaseGlobal, Origin: OriginGlobal, DefaultDomain: "www.workbuddy.ai"}
	}
	return Endpoints{Base: BaseCN, Origin: OriginCN}
}

// IsGlobal 判断区域是否国际版。空值按国内版（与旧版 login.EndpointsForRegion 一致）。
func IsGlobal(region string) bool {
	r := strings.ToLower(strings.TrimSpace(region))
	return r == RegionGlobal || r == "global" || r == "intl" || r == "www.workbuddy.ai"
}

// NormalizeRegion 把用户输入/旧配置归一到两个合法前缀之一。
func NormalizeRegion(region string) string {
	if IsGlobal(region) {
		return RegionGlobal
	}
	return RegionCN
}

// modelsPath 是模型目录接口路径（两区路径不同）。
func (e Endpoints) modelsPath() string {
	if e.Base == BaseGlobal {
		return "/v2/enterprises/personal/models"
	}
	return "/console/enterprises/personal/models"
}

// 登录/认证端点（两区路径相同，仅 host 不同）。
func (e Endpoints) authStateURL() string { return e.Base + "/v2/plugin/auth/state?platform=CLI" }
func (e Endpoints) authTokenURL() string { return e.Base + "/v2/plugin/auth/token?state=" }
func (e Endpoints) loginAcctURL() string { return e.Base + "/v2/plugin/login/account?state=" }

// 其它端点。
const (
	epChat          = "/v2/chat/completions"
	epRefresh       = "/v2/plugin/auth/token/refresh"
	epUserResource  = "/v2/billing/meter/get-user-resource"
	epDailyCheckin  = "/v2/billing/meter/daily-checkin"
	epCheckinStatus = "/v2/billing/meter/checkin-activity-status"
)

// softRateResetLoc 是上游墙钟时间的固定时区（UTC+8）。
// 不能用 time.Local：非 UTC+8 机器解析会差一天。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// wallClockLayout 是上游日期格式。
const wallClockLayout = "2006-01-02 15:04:05"

// OverrideBase 覆盖上游基址（联调/自建反代用），Origin/DefaultDomain 保持区域原值。
func OverrideBase(ep Endpoints, base string) Endpoints {
	if base = strings.TrimRight(strings.TrimSpace(base), "/"); base != "" {
		ep.Base = base
	}
	return ep
}
