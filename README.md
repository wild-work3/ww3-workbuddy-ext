# ww3-workbuddy-ext

ww3 **WorkBuddy 双区渠道插件** —— 扫码登录（设备流 state 轮询），国内版与国际版共用一套实现。

| 项 | 值 |
|---|---|
| 插件 ID | `workbuddy` |
| 类型 | `channel`（渠道插件） |
| 命名空间前缀 | `workbuddy`（国内版）、`workbuddyai`（国际版） |
| 认证范式 | `device_poll`（[AUTH_FLOW.md](https://github.com/wild-work3/ww3-protocol/blob/main/docs/AUTH_FLOW.md)） |
| 协议 | IPC 协议 v1（`protocol: {min:1, max:1}`） |
| 运行形态 | 一请求一进程（`execution.control = call`、`conversation = call`），不常驻 |

## 构建

本仓**独立可构建**，不依赖 ww3 主仓或工作区脚本：

```bash
bash build.sh                              # → dist/workbuddy-ext.exe（本机平台）
bash build.sh --zip                        # 另打 dist/workbuddy-<version>-<os>-<arch>.zip
bash build.sh --zip --target linux/arm64   # 交叉编译（CI 用；本地日常不需要）
```

包名带平台后缀是**强制约定**：同一个版本号为 6 个平台各发一个包，注册文件的 `versions[].file`
靠它区分。单一可执行文件在各平台的形态：Windows `.exe`（PE/COFF）、Linux 无扩展名（ELF）、
macOS 无扩展名（Mach-O）。

## 自测

### 契约符合性（conformance）

```bash
# 下载预编译的 ww3-conformance（无需 Go）
curl -L -o ww3-conformance.exe \
  https://github.com/wild-work3/ww3-conformance/releases/latest/download/ww3-conformance-windows-amd64.exe

# B 层：仓库自带 conformance.json，全部打本地 mock 上游，零真实请求
./ww3-conformance.exe --exe dist/workbuddy-ext.exe --descriptor conformance.json \
  --out conformance-report --json-summary conformance-summary.json --print-snippet
```

### CLI 自测

```bash
./dist/workbuddy-ext.exe manifest --out -
./dist/workbuddy-ext.exe ping --data ./data
./dist/workbuddy-ext.exe accounts --data ./data --config '{"region":"workbuddy"}'
```

## 数据目录

插件数据根由调用方（core）通过 `--data` 注入，缺省是 exe 所在目录：

```
<data>/auths/     凭据（与旧版 wild-work.exe 的 auths/ 格式可互换）
<data>/logs/      日志
<data>/stat/      插件自记流水
```

凭据按区域分文件（`auths/workbuddy-<标识>.json` / `auths/workbuddyai-<标识>.json`）——
只按账号标识存会让两区凭据互相覆盖。

## 发布

打 tag 触发 `.github/workflows/release.yml`：

1. **6 平台矩阵构建**（`windows`/`linux`/`darwin` × `amd64`/`arm64`，全在 `ubuntu-latest` 上交叉编译）；
2. 汇总发 GitHub Release（`workbuddy-<ver>-<os>-<arch>.zip` + `.sha256`）；
3. 下载最新版 `ww3-conformance`，对本机平台二进制跑契约校验（B 层，全打 mock）；
4. 生成注册表条目（每个平台一条 `versions[]`）并以 PR 形式提到
   [`ww3-ext-registry`](https://github.com/wild-work3/ww3-ext-registry)。

构建/校验/提 PR **全部在 CI 完成**，本地不做交叉编译（理由见仓库 `docs/渠道插件全平台发布说明.md`）。

注册表只强制要求 `repo` / `version` / `file` / `sha256`；准入摘要属**可选**信息，
第 3~4 步失败不会阻断发布（摘要只是让面板多显示一个徽章与能力矩阵）。

## 契约

- 插件协议：[`ww3-protocol/docs/PLUGIN_PROTOCOL.md`](https://github.com/wild-work3/ww3-protocol/blob/main/docs/PLUGIN_PROTOCOL.md)
- 对话流式契约：[`IPC_CHAT.md`](https://github.com/wild-work3/ww3-protocol/blob/main/docs/IPC_CHAT.md)
- CLI 契约：[`PLUGIN_CLI.md`](https://github.com/wild-work3/ww3-protocol/blob/main/docs/PLUGIN_CLI.md)
- Go SDK / 参考实现：[`ww3-protocol`](https://github.com/wild-work3/ww3-protocol)
