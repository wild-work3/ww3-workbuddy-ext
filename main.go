// workbuddy-ext：WorkBuddy 双区渠道插件（国内版 workbuddy + 国际版 workbuddyai 合并，
// 设备流扫码登录 / device_poll 范式）。
//
// 命令行形态（契约见 protocol/docs/PLUGIN_CLI.md）：
//
//	workbuddy-ext.exe <command> --data <dir> [--switch value ...]
//
//	call      协议本体：stdin 读一帧 invoke → 事件 + result → 退出（core 的调用方式）
//	stdio     协议本体：JSONL RPC 循环直到 EOF（自测/调试）
//	manifest  输出 manifest.json
//	ping      控制面逐项自检
//	login     发起扫码登录，--wait N 则轮询到成功
//	status    查一次登录状态（--flow）
//	accounts  列出已保存账号（脱敏，不含 token）
//	models    拉模型列表（带区域前缀）
//	chat      直接对话，正文打到 stdout
//
// 数据目录（W3-22）：<data>/{auths,logs,stat}。
// 凭据文件 <data>/auths/<区前缀>-<uid>.json 与旧版 wild-work.exe 完全兼容。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"wild-work3/workbuddy-ext/internal/channel"
	"wild-work3/workbuddy-ext/internal/protocol/chat"
	"wild-work3/workbuddy-ext/internal/protocol/cli"
	"wild-work3/workbuddy-ext/internal/protocol/plugin"
	"wild-work3/workbuddy-ext/internal/wb"
)

// dataDirGlobal 是本次进程解析出的数据目录（便利命令要显式传给子进程）。
var dataDirGlobal string

func main() {
	app := cli.NewApp("workbuddy-ext", "WorkBuddy 双区渠道插件（国内版 / 国际版）", "call")

	// ---- 协议本体 ----
	cCall := app.Cmd("call", "协议本体：一次性执行一条 invoke（core 的调用方式）")
	pCall := cCall.Str("data", "", "插件数据目录（缺省 $WW3_DATA，再缺省 exe 目录）")
	cStdio := app.Cmd("stdio", "协议本体：JSONL RPC 循环直到 stdin EOF（自测/调试）")
	pStdio := cStdio.Str("data", "", "插件数据目录")
	cManifest := app.Cmd("manifest", "输出 manifest.json")
	pManOut := cManifest.Str("out", "", "写到文件（缺省打到 stdout）")

	// ---- 便利命令 ----
	cPing := app.Cmd("ping", "控制面逐项自检")
	nPing := addNetFlags(cPing)
	cLogin := app.Cmd("login", "发起扫码登录（--wait N 则轮询到成功）")
	nLogin := addNetFlags(cLogin)
	loginWait := cLogin.Int("wait", 0, "轮询等待秒数（0=只打印 auth_url 与 flow_id）")
	cStatus := app.Cmd("status", "查询一次登录状态")
	nStatus := addNetFlags(cStatus)
	statusFlow := cStatus.Str("flow", "", "登录流程 ID（login 命令输出）")
	cAccounts := app.Cmd("accounts", "列出已保存账号（脱敏）")
	nAccounts := addNetFlags(cAccounts)
	cModels := app.Cmd("models", "拉取模型列表（带区域前缀）")
	nModels := addNetFlags(cModels)
	cChat := app.Cmd("chat", "发起一次对话，正文打到 stdout")
	nChat := addNetFlags(cChat)
	chatModel := cChat.Str("model", os.Getenv("WW3_MODEL"), "模型名，如 workbuddyai/hy3")
	chatPrompt := cChat.Str("prompt", "你好，请用一句话自我介绍", "提示词")
	chatRaw := cChat.Str("raw", "", "直接给出 chat.generate 的 JSON 请求体（调试用，覆盖 --prompt）")

	cmd, err := app.Parse(os.Args[1:])
	if err != nil {
		if err == cli.ErrHelp {
			return
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}

	var dataFlag string
	switch cmd.CmdName() {
	case "call":
		dataFlag = *pCall
	case "stdio":
		dataFlag = *pStdio
	case "ping":
		dataFlag = *nPing.data
	case "login":
		dataFlag = *nLogin.data
	case "status":
		dataFlag = *nStatus.data
	case "accounts":
		dataFlag = *nAccounts.data
	case "models":
		dataFlag = *nModels.data
	case "chat":
		dataFlag = *nChat.data
	}
	dataDirGlobal = cli.ResolveData(dataFlag)
	channel.Log, _ = cli.NewLogger("workbuddy", cli.LogPath(dataDirGlobal, "workbuddy"))
	if channel.Log != nil {
		defer channel.Log.Close()
	}

	switch cmd.CmdName() {
	case plugin.ModeCall, plugin.ModeStdio:
		if err := channel.Serve(context.Background(), cmd.CmdName(), dataDirGlobal, os.Stdin, os.Stdout); err != nil {
			fail("%s 退出: %v", cmd.CmdName(), err)
		}
	case "manifest":
		b, _ := json.MarshalIndent(channel.Manifest(), "", "  ")
		if *pManOut != "" {
			if err := os.WriteFile(*pManOut, append(b, '\n'), 0o644); err != nil {
				fail("写 %s 失败: %v", *pManOut, err)
			}
			fmt.Fprintf(os.Stderr, "已写出 %s\n", *pManOut)
			return
		}
		fmt.Println(string(b))
	case "login":
		runLogin(nLogin.cfg(), *loginWait)
	case "status":
		runStatus(nStatus.cfg(), *statusFlow)
	case "accounts":
		runAccounts(nAccounts.cfg())
	case "models":
		runModels(nModels.cfg())
	case "chat":
		runChat(nChat.cfg(), *nChat.auth, *chatModel, *chatPrompt, *chatRaw)
	case "ping":
		runPing(nPing.cfg())
	default:
		fail("未知命令: %s", cmd.CmdName())
	}
}

// netFlags 是 §3 契约网络开关。workbuddy 额外有 --region（多区渠道）。
type netFlags struct {
	data    *string
	config  *string
	region  *string
	auth    *string
	proxy   *string
	baseURL *string
	timeout *int
}

func addNetFlags(c *cli.Command) *netFlags {
	n := &netFlags{
		data:    c.Str("data", "", "插件数据目录（缺省 $WW3_DATA，再缺省 exe 目录）"),
		config:  c.Str("config", "", "渠道配置 JSON，或 @文件路径"),
		region:  c.Str("region", os.Getenv("WW3_REGION"), "区域：workbuddy（国内版）| workbuddyai（国际版）"),
		auth:    c.Str("auth", os.Getenv("WW3_AUTH"), "凭据标识（auths/ 文件名去后缀）"),
		proxy:   c.Str("proxy", os.Getenv("WW3_PROXY"), "出站代理"),
		baseURL: c.Str("base-url", os.Getenv("WW3_BASE_URL"), "上游基址覆盖（联调/自建反代）"),
	}
	n.timeout = c.Int("timeout", 300, "上游超时（秒）")
	return n
}

// cfg 组装渠道配置：--config 为底，其余显式开关覆盖。
func (n *netFlags) cfg() map[string]any {
	cfg := map[string]any{}
	if s := *n.config; s != "" {
		if err := json.Unmarshal([]byte(cli.ConfigArg(s)), &cfg); err != nil {
			fail("--config 解析失败: %v", err)
		}
	}
	cfg["region"] = wb.NormalizeRegion(*n.region)
	if v := *n.baseURL; v != "" {
		cfg["base_url"] = v
	}
	if v := *n.proxy; v != "" {
		cfg["proxy"] = v
	}
	if *n.timeout > 0 {
		cfg["timeout"] = *n.timeout
	}
	return cfg
}

// callOnce 起一个一次性子进程调一个方法（与 core 的调用方式完全一致，不存在 CLI 专用捷径）。
// --data 必须显式传给子进程，否则子进程按 $WW3_DATA / exe 目录解析，会与父进程分裂。
func callOnce(method string, params any) string {
	exe, err := os.Executable()
	if err != nil {
		fail("定位自身可执行文件失败: %v", err)
	}
	args := []string{plugin.ModeCall}
	if dataDirGlobal != "" {
		args = append(args, "--data", dataDirGlobal)
	}
	p, err := cli.Spawn("workbuddy-call", exe, args, channel.Log)
	if err != nil {
		fail("启动子进程失败: %v", err)
	}
	defer p.Close()
	res, err := p.InvokeSimple(context.Background(), method, params)
	if err != nil {
		fail("%s 失败: %v", method, err)
	}
	return string(res.Raw)
}

// runLogin 发起扫码登录，可选轮询到成功。
//
// 流程状态落盘在 <data>/auths/../flows（插件私有），所以每次 status 都是一次独立进程，
// 与 core 的调用方式完全一致（W3-20 无常驻）。
func runLogin(cfg map[string]any, waitSec int) {
	var br struct {
		FlowID  string `json:"flow_id"`
		AuthURL string `json:"auth_url"`
	}
	_ = json.Unmarshal([]byte(callOnce("auth.begin",
		map[string]any{"method": "device", "region": cfg["region"], "config": cfg})), &br)
	fmt.Fprintf(os.Stderr, "请打开以下地址完成登录：\n%s\n", br.AuthURL)
	fmt.Println(br.FlowID) // stdout 只输出 flow_id，便于脚本消费

	for waitSec > 0 {
		deadline := time.Now().Add(time.Duration(waitSec) * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			var st struct {
				State   string `json:"state"`
				Hint    string `json:"hint"`
				Account *struct {
					UID      string `json:"uid"`
					Nickname string `json:"nickname"`
					AuthFile string `json:"auth_file"`
				} `json:"account"`
			}
			_ = json.Unmarshal([]byte(callOnce("auth.status",
				map[string]any{"flow_id": br.FlowID, "config": cfg})), &st)
			fmt.Fprintf(os.Stderr, "  [status] %s %s\n", st.State, st.Hint)
			if st.State == "success" {
				if st.Account != nil {
					fmt.Fprintf(os.Stderr, "登录成功：%s（auth=%s）\n", st.Account.Nickname, st.Account.AuthFile)
				}
				return
			}
			if st.State == "failed" || st.State == "expired" || st.State == "cancelled" {
				fail("登录失败：%s", st.Hint)
			}
		}
		return
	}
}

func runStatus(cfg map[string]any, flowID string) {
	if flowID == "" {
		fail("缺少 --flow")
	}
	fmt.Fprintln(os.Stderr, cli.RedactJSON(callOnce("auth.status",
		map[string]any{"flow_id": flowID, "config": cfg})))
}

func runAccounts(cfg map[string]any) {
	// 账号事实本身不含 token，仍统一过一遍 RedactJSON 兜底（§9 红线）
	fmt.Println(cli.RedactJSON(callOnce("accounts.list", map[string]any{"config": cfg})))
}

func runModels(cfg map[string]any) {
	fmt.Println(cli.RedactJSON(callOnce("models.list", map[string]any{"config": cfg})))
}

func runPing(cfg map[string]any) {
	probes := []string{"config.schema", "auth.methods", "accounts.list", "credits.get",
		"accounts.resource_detail", "accounts.refresh", "stats.get"}
	for _, m := range probes {
		r, err := spawnInvoke(m, map[string]any{"config": cfg})
		if err != nil {
			fmt.Fprintf(os.Stderr, "✗ %s: %v\n", m, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "✓ %s → %s\n", m, truncateStr(r, 260))
	}
}

// spawnInvoke 起一次性子进程调一个方法并返回裸 result（ping 用，逐项报告失败）。
func spawnInvoke(method string, params any) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	args := []string{plugin.ModeCall}
	if dataDirGlobal != "" {
		args = append(args, "--data", dataDirGlobal)
	}
	p, err := cli.Spawn("workbuddy-call", exe, args, channel.Log)
	if err != nil {
		return "", err
	}
	defer p.Close()
	r, err := p.InvokeSimple(context.Background(), method, params)
	if err != nil {
		return "", err
	}
	return string(r.Raw), nil
}

// runChat 命令行直接对话：spawn 自身 call 子进程走真实 IPC，正文打到 stdout。
func runChat(cfg map[string]any, authName, model, prompt, rawBody string) {
	region := wb.NormalizeRegion(fmt.Sprint(cfg["region"]))
	if model == "" {
		model = region + "/hy3"
	}
	req := chat.GenerateRequest{Model: model, Stream: true, AccountUID: authName,
		ConversationID: fmt.Sprintf("cli-%d", os.Getpid())}
	if rawBody != "" {
		if err := json.Unmarshal([]byte(rawBody), &req); err != nil {
			fail("--raw 解析失败: %v", err)
		}
	} else {
		req.Messages = []chat.ChatMessage{{Role: "user", Content: prompt}}
	}
	req.ChannelConfig, _ = json.Marshal(cfg)

	exe, err := os.Executable()
	if err != nil {
		fail("定位自身可执行文件失败: %v", err)
	}
	args := []string{plugin.ModeCall}
	if dataDirGlobal != "" {
		args = append(args, "--data", dataDirGlobal)
	}
	p, err := cli.Spawn("workbuddy-call", exe, args, channel.Log)
	if err != nil {
		fail("启动子进程失败: %v", err)
	}
	defer p.Close()
	p.Out = os.Stdout // 正文实时输出到 stdout
	res, err := p.Invoke(context.Background(), cli.Step{Method: "chat.generate", Params: req, Stream: true})
	if err != nil {
		fail("chat.generate 失败: %v", err)
	}
	fmt.Fprintf(os.Stderr, "\n[结果] ok=%v finish=%s tokens=%d/%d 耗时=%dms 首字=%dms\n",
		res.OK, res.Finish, res.UsageInt("prompt_tokens"), res.UsageInt("completion_tokens"),
		res.UsageInt("time_ms"), res.UsageInt("first_token_ms"))
	if !res.OK {
		os.Exit(2)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
