package wb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Store 是插件的凭据仓库（W3-07：凭据绝不进 IPC、绝不进日志、绝不落工作目录）。
//
//	<data>/auths/<auth_file>.json        ← 与旧版 wild-work.exe 的 auths/ 完全一致
//
// 文件名即「凭据标识」（无 .json 后缀），同时是 core 的 account_uid 与 `--auth` 参数
// （PLUGIN_CLI §5.2）。命名 `<区前缀>-<上游uid>`，例如 `workbuddy-wb-uid-001`、
// `workbuddyai-wb-uid-001`——同一个自然人两区 uid 相同也不会互相覆盖，这正是旧版
// `auths/workbuddy-*.json` 与 `auths/workbuddyai-*.json` 并存的做法。
//
// 旧的 `<data>/creds/workbuddy/<uid>@<region>.json` 仍在读取路径上兜底（只读兼容），
// 下次写入时自动迁到 auths/。
type Store struct{ Dir string }

// NewStore 返回凭据仓库；dataDir 即插件数据目录（core 通过 --data 传，缺省 exe 目录）。
func NewStore(dataDir string) *Store {
	return &Store{Dir: filepath.Join(dataDir, "auths")}
}

// authFile 是磁盘上的凭据文件结构（旧版 auth/auth.go saveAtomicLocked 的嵌套形）。
type authFile struct {
	Auth    json.RawMessage `json:"auth"`
	Account authAccount     `json:"account"`
}

type authAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

// AuthFileName 计算凭据标识：优先用已有的 AuthFile（改名保持稳定），
// 否则 `<区前缀>-<上游uid>`（与旧版命名习惯一致）。
func AuthFileName(a *Auth) string {
	if a.AuthFile != "" {
		return a.AuthFile
	}
	uid := strings.TrimSpace(a.UID)
	if uid == "" {
		return ""
	}
	return a.RegionOf() + "-" + sanitizeName(uid)
}

// sanitizeName 把 uid 里不适合做文件名的字符替换掉（上游 uid 可能是 URL/邮箱形态）。
func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (s *Store) Path(name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	if name == "" {
		return ""
	}
	return filepath.Join(s.Dir, name+".json")
}

// Save 原子写凭据（0600，tmp+rename），写入固定用旧版嵌套形。
func (s *Store) Save(a *Auth) error {
	name := AuthFileName(a)
	if name == "" {
		return &Error{Kind: "bad_params", Msg: "凭据缺少 uid，无法确定文件名"}
	}
	a.AuthFile = name
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	// 所有字段都写键（空的写空串/null），旧版读回时不丢信息（对齐 saveAtomicLocked）
	inner := map[string]any{
		"accessToken": a.AccessToken, "refreshToken": a.RefreshToken,
		"expiresAt": a.ExpiresAt, "domain": a.Domain,
		"apiHost": a.APIHost, "machineId": a.MachineID, "deviceId": a.DeviceID,
		"machineToken": a.MachineToken, "machineType": a.MachineType,
		"signingSecret": a.SigningSecret, "consoleCookie": a.ConsoleCookie,
		"baizhiCookie": a.BaizhiCookie, "extra": nil,
	}
	b, err := json.MarshalIndent(authFile{
		Auth: mustRaw(inner),
		Account: authAccount{
			UID: a.UID, EnterpriseID: a.EnterpriseID, Nickname: a.Nickname,
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	dst := s.Path(name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// Load 按凭据标识（或裸 uid）读一个账号。
//
// 查找顺序：auths/<name>.json → auths/<区前缀>-<name>.json → 旧目录 creds/workbuddy/<name>@<region>.json。
func (s *Store) Load(name string) (*Auth, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	if name == "" {
		return nil, os.ErrNotExist
	}
	cands := []string{s.Path(name)}
	for _, region := range []string{RegionCN, RegionGlobal} {
		cands = append(cands, s.Path(region+"-"+name))
	}
	for _, p := range cands {
		if a, err := s.readFile(p); err == nil {
			return a, nil
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	// 旧版 ww3 早期目录（<data>/creds/workbuddy/<uid>@<region>.json）只读兼容
	legacyDir := filepath.Join(filepath.Dir(s.Dir), "creds", "workbuddy")
	for _, region := range []string{RegionCN, RegionGlobal} {
		if a, err := s.readFile(filepath.Join(legacyDir, name+"@"+region+".json")); err == nil {
			return a, nil
		}
	}
	return nil, os.ErrNotExist
}

// readFile 读一个凭据文件（同时兼容嵌套形与扁平形）。
func (s *Store) readFile(p string) (*Auth, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	a, err := parseAuth(b)
	if err != nil {
		return nil, &Error{Kind: "account_fault", Msg: "凭据文件损坏: " + filepath.Base(p)}
	}
	if a.AuthFile == "" {
		a.AuthFile = strings.TrimSuffix(filepath.Base(p), ".json")
	}
	return a, nil
}

// parseAuth 解析凭据：旧版嵌套形（{auth:{...},account:{...}}）或扁平形（CPA 面板格式）。
func parseAuth(b []byte) (*Auth, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, err
	}
	a := &Auth{}
	var acct authAccount
	if raw, ok := probe["auth"]; ok && len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, a); err != nil {
			return nil, err
		}
		if raw2, ok := probe["account"]; ok {
			_ = json.Unmarshal(raw2, &acct)
		}
	} else {
		// 扁平形：顶层就是 auth 字段
		if err := json.Unmarshal(b, a); err != nil {
			return nil, err
		}
		if raw2, ok := probe["account"]; ok && len(raw2) > 0 && raw2[0] == '{' {
			_ = json.Unmarshal(raw2, &acct)
		}
	}
	if a.UID == "" {
		a.UID = acct.UID
	}
	if a.EnterpriseID == "" {
		a.EnterpriseID = acct.EnterpriseID
	}
	if a.Nickname == "" {
		a.Nickname = acct.Nickname
	}
	// 扁平形里的 account 键也可能是字符串（旧版混用），这里保持宽松
	if a.UID == "" {
		var flat struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		}
		if json.Unmarshal(b, &flat) == nil && flat.UID != "" {
			a.UID, a.Nickname = flat.UID, orDefaultStr(a.Nickname, flat.Nickname)
		}
	}
	if a.AccessToken == "" {
		return nil, &Error{Kind: "account_fault", Msg: "凭据缺少 accessToken"}
	}
	return a, nil
}

// List 列出全部凭据（按区域、标识排序；坏文件跳过不阻断）。
func (s *Store) List() []Auth {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil
	}
	out := make([]Auth, 0, len(ents))
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		a, err := s.readFile(filepath.Join(s.Dir, name))
		if err != nil {
			continue
		}
		out = append(out, *a)
	}
	// 旧目录兜底（只在 auths/ 为空时补，避免与迁移后的新文件重复）
	if len(out) == 0 {
		legacyDir := filepath.Join(filepath.Dir(s.Dir), "creds", "workbuddy")
		if ls, err := os.ReadDir(legacyDir); err == nil {
			for _, e := range ls {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
					continue
				}
				if a, err := s.readFile(filepath.Join(legacyDir, e.Name())); err == nil {
					out = append(out, *a)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RegionOf() != out[j].RegionOf() {
			return out[i].RegionOf() < out[j].RegionOf()
		}
		return out[i].AuthFile < out[j].AuthFile
	})
	return out
}

// Delete 删除凭据（按标识，兼容裸 uid 与旧目录）。
func (s *Store) Delete(name string) error {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".json")
	if name == "" {
		return nil
	}
	cands := []string{s.Path(name)}
	for _, region := range []string{RegionCN, RegionGlobal} {
		cands = append(cands, s.Path(region+"-"+name))
	}
	legacyDir := filepath.Join(filepath.Dir(s.Dir), "creds", "workbuddy")
	for _, region := range []string{RegionCN, RegionGlobal} {
		cands = append(cands, filepath.Join(legacyDir, name+"@"+region+".json"))
	}
	for _, p := range cands {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// FindByUID 按上游 uid 找凭据（面板只给了 uid 时的兜底；同 uid 多区时返回第一条）。
func (s *Store) FindByUID(uid string) (*Auth, error) {
	for _, a := range s.List() {
		if a.UID == uid || a.AuthFile == uid {
			cp := a
			return &cp, nil
		}
	}
	return nil, os.ErrNotExist
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func orDefaultStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// unixOrZero 把 Unix 秒转 time.Time（0 = 未知，返回零值）。
func unixOrZero(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// parseExpiresAt 兼容三种历史形态：Unix 秒数字、数字字符串、RFC3339 字符串。
// 返回 Unix 秒（0 = 未知/不可解析，不臆造过期时间）。
func parseExpiresAt(raw json.RawMessage) int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if strings.HasPrefix(s, "\"") {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return 0
		}
		if n, err := strconv.ParseInt(strings.TrimSpace(str), 10, 64); err == nil {
			return n
		}
		if t, err := time.Parse(time.RFC3339, str); err == nil {
			return t.Unix()
		}
	}
	return 0
}
