package channel

import (
	"wild-work3/workbuddy-ext/internal/protocol/errkind"
	"wild-work3/workbuddy-ext/internal/protocol/jsonl"
)

// Raw / Err 是 internal/protocol/jsonl 的类型别名：方法签名里出现十几次，
// 用短名保持一屏内可读（call / stdio 两种运行形态共用同一份方法表）。
type Raw = jsonl.RawMessage
type Err = jsonl.ErrObj

// errf 构造协议错误对象。
func errf(kind errkind.Kind, scope errkind.Scope, format string, a ...any) *Err {
	return jsonl.Errorf(kind, scope, format, a...)
}
