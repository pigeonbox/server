#!/usr/bin/env python3
"""image_tags.py 的自测（变异测试：判据失效必须红）。

⚠️ 这段逻辑决定「镜像被推到哪个 registry、钉了哪些 tag」⇒ 写错的后果是
镜像推错名字或漏 tag ⇒ 生态应用包内嵌的镜像会拉错。
⇒ 判据必须**有区分力**：每个用例都对应一种真实会犯的错。

用法：python3 scripts/image_tags_selftest.py
"""
from __future__ import annotations

import importlib.util
import os
import sys

spec = importlib.util.spec_from_file_location(
    "image_tags", os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                "image_tags.py"))
it = importlib.util.module_from_spec(spec)
spec.loader.exec_module(it)

FAILS: list[str] = []


def check(label: str, got, want) -> None:
    if got != want:
        FAILS.append(f"{label}: got {got!r}, want {want!r}")


# ── 1) major.minor 提取 ───────────────────────────────────────────────
check("major_minor(v4.5.2)", it.major_minor("v4.5.2"), "4.5")
check("major_minor(v4.5.0)", it.major_minor("v4.5.0"), "4.5")
check("major_minor(4.5.2)", it.major_minor("4.5.2"), "4.5")
# 三段版本必须**只**取前两段（4.5.2 → 4.5，不是 4.5.2）
try:
    it.major_minor("v4")
    FAILS.append("major_minor('v4') 应报错（无 major.minor）")
except ValueError:
    pass

# ── 2) Docker Hub tag 集：version / major.minor / latest ─────────────
check("hub_tags 完整集",
      it.hub_tags("pigeonbox/server", "v4.5.2", True),
      ["docker.io/pigeonbox/server:4.5.2",
       "docker.io/pigeonbox/server:4.5",
       "docker.io/pigeonbox/server:latest"])
# ⚠️ tag 里**不能**带 v 前缀（Docker Hub 惯例是 4.5.2 而非 v4.5.2）
for t in it.hub_tags("pigeonbox/server", "v4.5.2", True):
    if ":v" in t:
        FAILS.append(f"Docker Hub tag 带 v 前缀：{t}")

# ── 3) 禁用时只保留 ghcr（不产生任何 docker.io tag）──────────────────
ghcr_s = "ghcr.io/pigeonbox/server:4.5.2"
ghcr_f = "ghcr.io/pigeonbox/frontend:4.5.2"
off_s = it.compose(ghcr_s, False, "pigeonbox/server", "v4.5.2", False)
check("禁用时只保留 ghcr", off_s, [ghcr_s])
if any("docker.io" in t for t in off_s):
    FAILS.append("禁用时仍产出 docker.io tag")

# ── 4) 启用时 ghcr + docker.io 都在 ──────────────────────────────────
on_s = it.compose(ghcr_s, True, "pigeonbox/server", "v4.5.2", True)
check("启用时 ghcr 在", ghcr_s in on_s, True)
check("启用时 docker.io 在", any("docker.io/pigeonbox/server" in t for t in on_s), True)
check("启用时总数", len(on_s), 4)

# ── 5) ⚠️ 核心不变量：两个镜像的 tag 集**不得串味** ────────────────────
# 回归：2026-10-31 实施时曾把 frontend 的 ghcr tag 写成 server 的
# ⇒ 会把 server 镜像推到 frontend 名下（内容错但流水线全绿）。
on_f = it.compose(ghcr_f, True, "pigeonbox/frontend", "v4.5.2", True)
if any("pigeonbox/server" in t for t in on_f):
    FAILS.append(f"★ frontend 的 tag 集混进了 server：{on_f}")
if any("pigeonbox/frontend" in t for t in on_s):
    FAILS.append(f"★ server 的 tag 集混进了 frontend：{on_s}")
# 两边都应各自含自己的 ghcr 引用
check("server 集含 server ghcr", "ghcr.io/pigeonbox/server:4.5.2" in on_s, True)
check("frontend 集含 frontend ghcr", "ghcr.io/pigeonbox/frontend:4.5.2" in on_f, True)
# hub 仓库名也各归各
if any("docker.io/pigeonbox/frontend" in t for t in on_s):
    FAILS.append(f"★ server 的 tag 集混进了 frontend 的 hub 仓库：{on_s}")

# ── 6) 启用但没给仓库名 ⇒ 必须报错（不能默默产出半套 tag）────────────
try:
    it.compose(ghcr_s, True, "", "v4.5.2", True)
    FAILS.append("启用但无 hub 仓库名时未报错")
except ValueError:
    pass

# ── 7) emit 格式：GitHub Actions 多行输出 ────────────────────────────
out = it.emit("server_tags", ["a", "b"])
if not out.startswith("server_tags<<EOF\n") or not out.endswith("\nEOF"):
    FAILS.append(f"emit 格式不对：{out!r}")
if "a\nb" not in out:
    FAILS.append(f"emit 内容不对：{out!r}")

# ── 8) latest 仅当本次 tag 是全仓最高 semver（issue #131）───────────────
# 回归场景：给**旧线补 hotfix tag**（4.9.x 与 5.0.0 同日发版的实录存在）
# ⇒ 旧实现无条件打 latest ⇒ ghcr/Docker Hub 的 latest 与 GitHub Release 的
# 「Latest」全部指回旧版 ⇒ 跟随 latest 的部署被**静默降级**。
check("is_highest 新高版本", it.is_highest("v6.0.0", ["v5.8.0", "v4.9.1"]), True)
check("is_highest 旧线 hotfix 不得打 latest", it.is_highest("v4.9.1", ["v5.8.0", "v4.9.0"]), False)
check("is_highest 同 tag 重跑", it.is_highest("v5.8.0", ["v5.8.0"]), True)
# 预发布只在**同数字段**时更低：5.8.1-rc.1 < 5.8.1，但 > 5.8.0（semver 正确）
check("is_highest 预发布低于同号正式", it.is_highest("v5.8.1-rc.1", ["v5.8.1"]), False)
check("is_highest 预发布高于低一版", it.is_highest("v5.8.1-rc.1", ["v5.8.0"]), True)
check("is_highest 正式高于同号预发布", it.is_highest("v5.8.1", ["v5.8.1-rc.1"]), True)
check("is_highest 非 semver 不打 latest", it.is_highest("v-nightly", ["v5.8.0"]), False)
check("is_highest 空列表", it.is_highest("v1.0.0", []), True)
check("is_highest 更高段位数字比较", it.is_highest("v4.10.0", ["v4.9.9"]), True)
# git ls-remote --tags 原始形态：sha + refs/tags/ 前缀 + 注解 tag 的 ^{} 剥离
ls_remote = "abc123\trefs/tags/v5.8.0\ndef456\trefs/tags/v5.8.0^{}\n"
check("is_highest 解析 ls-remote 输出",
      it.is_highest("v6.0.0", it.parse_tag_lines(ls_remote)), True)
check("parse_tag_lines 纯清单形态", it.parse_tag_lines("v5.8.0\nv4.9.1\n"), ["v5.8.0", "v4.9.1"])
# hub_tags 的 latest 受开关控制（默认不打 = 忘传也只少打、不打错）
check("hub_tags latest 开关开", it.hub_tags("pigeonbox/server", "v4.5.2", True)[-1],
      "docker.io/pigeonbox/server:latest")
check("hub_tags latest 开关关",
      it.hub_tags("pigeonbox/server", "v4.5.2", False),
      ["docker.io/pigeonbox/server:4.5.2", "docker.io/pigeonbox/server:4.5"])
# compose 也要受控：latest=False 时最终集里不得出现 :latest
no_latest = it.compose(ghcr_s, True, "pigeonbox/server", "v4.5.2", False)
if any(t.endswith(":latest") for t in no_latest):
    FAILS.append(f"latest 开关关闭时仍产出 :latest：{no_latest}")

# ── 9) --check-latest CLI 形态（workflow 的实际调用方式）──────────────────
import subprocess
cli_out = subprocess.run(
    [sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "image_tags.py"),
     "--tag", "v4.9.1", "--check-latest",
     "--existing-tags", "a\trefs/tags/v5.8.0\nb\trefs/tags/v4.9.0"],
    capture_output=True, text=True)
check("--check-latest 旧线输出 false", cli_out.stdout.strip(), "false")
cli_out2 = subprocess.run(
    [sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "image_tags.py"),
     "--tag", "v6.0.0", "--check-latest",
     "--existing-tags", "a\trefs/tags/v5.8.0"],
    capture_output=True, text=True)
check("--check-latest 新高输出 true", cli_out2.stdout.strip(), "true")

for f in FAILS:
    print(f"FAIL {f}")
if FAILS:
    print(f"\n{len(FAILS)} 条判据失败")
    sys.exit(1)
print("image_tags selftest OK")
