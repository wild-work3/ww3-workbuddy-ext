package wb

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 会话头族的语义（旧版 upstream/convheaders.go 实测结论）：
//
//	X-Conversation-ID          会话级：同一条对话恒定（第一条 user 文本指纹派生）
//	X-Conversation-Request-ID  轮级：一次 user send 一个（末条 user 文本指纹派生），
//	                           工具回调/同轮重试必须复用，否则上游视为新轮
//	X-Conversation-Message-ID  调用级：每次 HTTP 请求都换
//	X-Root-Request-ID          本轮 Request-ID
//
// 国际版自 2026-10-16 起抽样校验该族，缺失返回 400 code=11-128 "conversation id missing"。
const maxAnchorBody = 8 << 20 // 超过 8MB 的消息体不做指纹（避免大请求反复哈希）

// conversationAnchors 从请求体 messages 取「会话锚点」与「轮次锚点」。
// 会话锚点 = 首条 user 文本指纹；轮次锚点 = 末条 user 文本指纹；无 user 退化首条消息文本。
func conversationAnchors(messages []json.RawMessage) (conv, turn string) {
	var firstUser, lastUser, firstAny string
	for _, raw := range messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		txt := contentText(m.Content)
		if txt == "" {
			continue
		}
		if firstAny == "" {
			firstAny = txt
		}
		if m.Role == "user" {
			if firstUser == "" {
				firstUser = txt
			}
			lastUser = txt
		}
	}
	conv = convFingerprint(firstUser)
	if conv == "" {
		conv = convFingerprint(firstAny)
	}
	turn = convFingerprint(lastUser)
	if turn == "" {
		turn = convFingerprint(firstAny)
	}
	return conv, turn
}

// contentText 把 content 归一成纯文本（字符串或分片数组里的 text 字段）。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	b := make([]byte, 0, 128)
	for _, p := range parts {
		b = append(b, p.Text...)
	}
	return string(b)
}

// convFingerprint 是 16 hex 的短指纹（sha256 前 8 字节）。
func convFingerprint(text string) string {
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// conversationID 派生会话 UUID：掺 uid 是刻意的——同一对话被不同账号服务不能共用 id。
func conversationID(uid, convAnchor string) string {
	if convAnchor == "" {
		return randomUUIDv4()
	}
	sum := sha256.Sum256([]byte("wb-conv\x00" + uid + "\x00" + convAnchor))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant RFC4122
	return formatUUID(u)
}

// turnIDOf 派生轮次 id（32 hex）。
func turnIDOf(uid, conv, turnAnchor string) string {
	if turnAnchor == "" {
		return newMessageID()
	}
	sum := sha256.Sum256([]byte("wb-turn\x00" + uid + "\x00" + conv + "\x00" + turnAnchor))
	return hex.EncodeToString(sum[:16])
}

// newMessageID 是随机 32 hex（crypto/rand 失败退回时间戳派生，绝不 panic）。
func newMessageID() string { return hex.EncodeToString(randBytes(16)) }

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("wb-ts-%d", time.Now().UnixNano())))
		copy(b, sum[:n])
	}
	return b
}

func randomUUIDv4() string {
	var u [16]byte
	copy(u[:], randBytes(16))
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return formatUUID(u)
}

func formatUUID(u [16]byte) string {
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(u[0:4]), hex.EncodeToString(u[4:6]),
		hex.EncodeToString(u[6:8]), hex.EncodeToString(u[8:10]), hex.EncodeToString(u[10:16]))
}

// injectConversationHeaders 写入会话头族与 B3 追踪头。
func injectConversationHeaders(h http.Header, uid, convAnchor, turnAnchor string) {
	convID := conversationID(uid, convAnchor)
	turnID := turnIDOf(uid, convAnchor, turnAnchor)
	msgID := newMessageID()

	h.Set("X-Conversation-ID", convID)
	h.Set("X-Conversation-Request-ID", turnID)
	h.Set("X-Conversation-Message-ID", msgID)
	h.Set("X-Request-ID", msgID)
	h.Set("X-Root-Request-ID", turnID)
	h.Set("X-B3-TraceId", msgID)
	h.Set("X-B3-SpanId", msgID[:16])
	h.Set("X-B3-Sampled", "1")
}
