#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把「刚构建好的插件产物 + conformance 摘要」合成一条注册表条目（<id>.json，schema:2）。

设计纪律（docs/PLUGIN_REGISTRY.md）：
  * 注册表只强制 repo/version/file/sha256；conformance/capabilities 是可选附加值，
    拿不到摘要就照常生成（字段省略，不写空壳）。
  * 已有条目里的历史字段（更早的 versions、人工补的 gateway 门、changelog）一律保留：
    本脚本只做 upsert，不做重建。
  * 内容真没变时**连 updated_at 都不动**，否则每次 CI 都产生空 diff 的 PR。

用法：
  python scripts/make_registry_entry.py \
      --manifest dist/manifest.json \
      --zip dist/workbuddy-1.0.0.zip \
      --summary conformance-summary.json \
      --repo https://github.com/wild-work3/ww3-workbuddy-ext \
      --os windows --arch amd64 \
      --base .registry/workbuddy.json --out .registry/workbuddy.json \
      [--changelog "..."]

退出码：0 成功（含「内容无变化」），1 参数/文件错误。
"""
import argparse
import hashlib
import json
import os
import sys
from datetime import datetime, timezone

# CI 里（GitHub Actions 的 Windows runner）stdout 是 cp1252，直接 print 中文会
# UnicodeEncodeError: 'charmap' codec can't encode ...（实测踩过，退出码 1 掩盖了真实进度）
for _s in (sys.stdout, sys.stderr):
    try:
        _s.reconfigure(encoding="utf-8", errors="replace")
    except Exception:  # noqa: BLE001  非 tty / 老版本 Python 忽略即可
        pass

SCHEMA = 2

# 注册表条目的规范字段序（与 ext-registry 现有文件一致，便于 PR diff）
TOP_ORDER = ["schema", "id", "type", "name", "description", "author", "repo",
             "status", "icon", "color", "conformance", "capabilities",
             "updated_at", "versions"]
VER_ORDER = ["version", "file", "sha256", "size", "os", "arch",
             "protocol", "gateway", "changelog"]


def load_json(path, required=True):
    if not path or not os.path.isfile(path):
        if required:
            raise SystemExit("缺少文件: %s" % path)
        return None
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def sha256_of(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def ordered(d, order):
    """按规范顺序输出，未知字段追加在末尾（前向兼容：别人加的字段不丢）。"""
    out = {k: d[k] for k in order if k in d}
    for k, v in d.items():
        out.setdefault(k, v)
    return out


def build(man, zpath, summary, repo, osname, arch, base, changelog):
    ver = man.get("version") or ""
    pid = man.get("id") or ""
    if not ver or not pid:
        raise SystemExit("manifest 缺 id/version")

    old = dict(base) if base else {}
    old_vers = {x.get("version"): x for x in (old.get("versions") or [])}

    e = {
        "schema": SCHEMA,
        "id": pid,
        "type": "hook" if man.get("kind") == "hook" else "channel",
        "name": man.get("name") or pid,
        "description": man.get("description") or old.get("description") or "",
        "author": old.get("author") or "wild-work3",
        "repo": repo.rstrip("/"),
        "status": old.get("status") or "verified",
        "icon": man.get("icon") or old.get("icon") or "",
        "color": man.get("color") or old.get("color") or "",
    }

    # 准入摘要（可选）：直接搬 conformance-summary.json 里的两个子对象，
    # 形状与注册表字段完全一致（W3-51 §3.3 就是为此设计的）。
    # 本次没有摘要就沿用旧的，而不是抹掉——摘要不该因为一次没跑就消失。
    if summary and summary.get("conformance"):
        e["conformance"] = summary["conformance"]
    elif old.get("conformance"):
        e["conformance"] = old["conformance"]
    if summary and summary.get("capabilities"):
        e["capabilities"] = summary["capabilities"]
    elif old.get("capabilities"):
        e["capabilities"] = old["capabilities"]

    v = {
        "version": ver,
        "file": os.path.basename(zpath),
        "sha256": sha256_of(zpath),
        "size": os.path.getsize(zpath),
        "os": osname,
        "arch": arch,
        "protocol": man.get("protocol") or {"min": 1, "max": 1},
    }
    prev = old_vers.get(ver) or {}
    if prev.get("gateway"):
        v["gateway"] = prev["gateway"]           # 版本门是人工维护的，脚本不改它
    v["changelog"] = changelog or prev.get("changelog") or ("首个版本" if not old_vers else "")

    vers = [x for k, x in old_vers.items() if k != ver]
    vers.append(v)
    vers.sort(key=lambda x: x.get("version") or "", reverse=True)  # 新版本在前
    e["versions"] = [ordered(x, VER_ORDER) for x in vers]

    # updated_at 只在版本条目真的变了时才刷新
    same = old.get("versions") and old_vers.get(ver) == v
    e["updated_at"] = (old.get("updated_at") if same else
                       datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
    return ordered({k: val for k, val in e.items() if val != ""}, TOP_ORDER)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--manifest", required=True)
    ap.add_argument("--zip", required=True)
    ap.add_argument("--summary", default="", help="conformance-summary.json（可选）")
    ap.add_argument("--repo", required=True, help="插件仓地址，如 https://github.com/o/r")
    ap.add_argument("--os", default="windows")
    ap.add_argument("--arch", default="amd64")
    ap.add_argument("--base", default="", help="已有注册表条目（保留其历史字段）")
    ap.add_argument("--out", required=True)
    ap.add_argument("--changelog", default="")
    a = ap.parse_args()

    man = load_json(a.manifest)
    entry = build(man, a.zip, load_json(a.summary, required=False),
                  a.repo, a.os, a.arch, load_json(a.base, required=False), a.changelog)

    os.makedirs(os.path.dirname(os.path.abspath(a.out)), exist_ok=True)
    text = json.dumps(entry, ensure_ascii=False, indent=2) + "\n"
    old = ""
    if os.path.isfile(a.out):
        with open(a.out, "r", encoding="utf-8") as f:
            old = f.read()
    with open(a.out, "w", encoding="utf-8", newline="\n") as f:
        f.write(text)

    changed = old.strip() != text.strip()
    print("[registry] %s %s -> %s (%s)" % (entry["id"], entry["versions"][0]["version"],
                                           a.out, "已更新" if changed else "无变化"))


if __name__ == "__main__":
    main()
