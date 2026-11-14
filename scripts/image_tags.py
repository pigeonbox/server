#!/usr/bin/env python3
"""计算镜像双推的 tag 集（ghcr + 可选 Docker Hub）。

**为什么抽成脚本而不是写在 workflow 的 shell 里**：这段逻辑决定「哪些镜像
被推到哪个 registry、钉了哪些 tag」⇒ 写错的后果是「镜像推错名字」
（server 镜像被推到 frontend 名下）或「Docker Hub 少了某个 tag」
⇒ 生态应用包内嵌的镜像就会拉错。⇒ 必须有**可测**的实现。

**为什么不 build 两次**：多架构构建实测 ~20 分钟 ⇒ 构建两次 = 40 分钟；
更糟的是两次构建的层可能不一致（缓存命中差异）⇒ 两个 registry 上的
镜像内容可能不同 ⇒ 生态包里的镜像与 ghcr 上的不等价。
⇒ 一次构建、多 tag 推送（buildx 原生支持）。

用法：
  python3 scripts/image_tags.py --tag v4.5.2 --enabled true \\
      --ghcr-server "ghcr.io/pigeonbox/server:4.5.2,..." \\
      --ghcr-frontend "ghcr.io/pigeonbox/frontend:4.5.2,..." \\
      --hub-server-repo pigeonbox/server --hub-frontend-repo pigeonbox/frontend
  → 输出 GitHub Actions 可直接消费的 `server_tags<<EOF ... EOF` 形式。

纯标准库（无第三方依赖）⇒ 与仓内其它脚本一致。
"""
from __future__ import annotations

import argparse
import sys


def major_minor(tag: str) -> str:
    """v4.5.2 → 4.5（去掉 v 前缀，取前两段）。"""
    t = tag.lstrip("v")
    parts = t.split(".")
    if len(parts) < 2:
        raise ValueError(f"tag {tag!r} 不含 major.minor")
    return ".".join(parts[:2])


def parse_semver(tag: str) -> tuple[int, int, int, str] | None:
    """v4.5.2 → (4, 5, 2, "")；v5.8.1-rc.1 → (5, 8, 1, "rc.1")；非 semver → None。

    预发布串只做字面比较（同数字段时预发布 < 正式版），够判「谁更高」。
    """
    t = tag[1:] if tag.startswith("v") else tag
    core, _, _build = t.partition("+")
    main, _, pre = core.partition("-")
    parts = main.split(".")
    if len(parts) < 3:
        return None
    if len(parts) > 3:
        # v1.2.3.4 之类不是 semver（多一段）⇒ 不参与 latest 判定
        return None
    try:
        major, minor, patch = int(parts[0]), int(parts[1]), int(parts[2])
    except ValueError:
        return None
    return (major, minor, patch, pre)


def _cmp_semver(a: tuple, b: tuple) -> int:
    """两个 parse_semver 结果比较：数字基不同按数字比；同基时预发布更低。"""
    if a[:3] != b[:3]:
        return (a[:3] > b[:3]) - (a[:3] < b[:3])
    ap, bp = a[3], b[3]
    if ap == bp:
        return 0
    if ap == "":
        return 1
    if bp == "":
        return -1
    return (ap > bp) - (ap < bp)


def parse_tag_lines(text: str) -> list[str]:
    """把 git ls-remote --tags 的原始输出（或每行一个 tag 名）解析成 tag 名列表。

    ls-remote 行形态是「<sha> TAB refs/tags/v1.2.3」；注解 tag 还会多一行
    「<sha> TAB refs/tags/v1.2.3^{}」（剥掉 ^{} 即可，两者是同一个 tag）。
    """
    out: list[str] = []
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        name = line.split()[-1]
        if name.startswith("refs/tags/"):
            name = name[len("refs/tags/"):]
        if name.endswith("^{}"):
            name = name[: -len("^{}")]
        if name:
            out.append(name)
    return out


def is_highest(tag: str, existing: list[str]) -> bool:
    """本次 tag 是否为全仓最高 semver（issue #131：latest 只在这种情况下打）。

    给**旧线补 hotfix tag** 时（4.9.x 与 5.0.0 同日发版的实录存在）返回 False
    ⇒ latest 与 GitHub Release 的「Latest」不被指回旧版（跟随 latest 的部署不被
    静默降级）。非 semver（如 v-nightly）一律不打 latest；其它无法解析的 tag
    不参与比较（忽略，不让一个脏 tag 卡死发版）。
    """
    cur = parse_semver(tag)
    if cur is None:
        return False
    for other in existing:
        if other == tag:
            continue
        o = parse_semver(other)
        if o is None:
            continue
        if _cmp_semver(o, cur) > 0:
            return False
    return True


def hub_tags(repo: str, tag: str, latest: bool) -> list[str]:
    """Docker Hub 侧的 tag 集：version / major.minor / [latest]。

    与 metadata-action 的语义对齐；latest 受开关控制（issue #131）：
    只有本次 tag 是全仓最高 semver 时才推 :latest，否则旧线 hotfix 会把
    latest 指回旧版 ⇒ 跟随 latest 的部署被静默降级。
    """
    v = tag.lstrip("v")
    mm = major_minor(tag)
    tags = [
        f"docker.io/{repo}:{v}",
        f"docker.io/{repo}:{mm}",
    ]
    if latest:
        tags.append(f"docker.io/{repo}:latest")
    return tags


def compose(ghcr: str, enabled: bool, hub_repo: str | None, tag: str,
            latest: bool) -> list[str]:
    """合成最终 tag 列表。

    ⚠️ 两个镜像的 ghcr tag 必须**各用各的**（server 用 .../server、
    frontend 用 .../frontend）——混用会把 server 镜像推到 frontend 名下。
    latest 的 ghcr 侧由 metadata-action 按**同一开关**打（enable=），
    这里只管 Docker Hub 侧。
    """
    base = [t.strip() for t in ghcr.splitlines() if t.strip()]
    if not enabled:
        return base
    if not hub_repo:
        raise ValueError("启用 Docker Hub 时必须给出仓库名")
    return base + hub_tags(hub_repo, tag, latest)


def emit(name: str, tags: list[str]) -> str:
    """输出 GitHub Actions 的多行输出格式。"""
    body = "\n".join(tags)
    return f"{name}<<EOF\n{body}\nEOF"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", required=True, help="版本 tag，如 v4.5.2")
    ap.add_argument("--enabled", default="",
                    help="Docker Hub 凭据是否就位（true/false）；--check-latest 模式不需要")
    ap.add_argument("--ghcr-server", default="")
    ap.add_argument("--ghcr-frontend", default="")
    ap.add_argument("--hub-server-repo", default="")
    ap.add_argument("--hub-frontend-repo", default="")
    ap.add_argument("--is-latest", choices=["true", "false"], default="false",
                    help="本次 tag 是否全仓最高 semver（决定推不推 :latest）；"
                         "默认 false = 忘传时只少打 latest，不打错（issue #131）")
    ap.add_argument("--check-latest", action="store_true",
                    help="只判定「是否全仓最高 semver」并输出 true/false（配合 "
                         "--existing-tags），不产出 tag 集")
    ap.add_argument("--existing-tags", default="",
                    help="仓库已有 tag：git ls-remote --tags 原始输出或每行一个 tag 名")
    args = ap.parse_args()

    if args.check_latest:
        if not args.existing_tags.strip():
            print("错误：--check-latest 需要 --existing-tags（git ls-remote 输出）",
                  file=sys.stderr)
            return 1
        verdict = is_highest(args.tag, parse_tag_lines(args.existing_tags))
        print("true" if verdict else "false")
        return 0

    if args.enabled not in ("true", "false"):
        print("错误：--enabled 必须给 true/false", file=sys.stderr)
        return 1
    enabled = args.enabled == "true"
    latest = args.is_latest == "true"
    try:
        server_tags = compose(args.ghcr_server, enabled, args.hub_server_repo,
                              args.tag, latest)
        frontend_tags = compose(args.ghcr_frontend, enabled,
                                args.hub_frontend_repo, args.tag, latest)
    except ValueError as exc:
        print(f"错误：{exc}", file=sys.stderr)
        return 1

    print(emit("server_tags", server_tags))
    print(emit("frontend_tags", frontend_tags))
    return 0


if __name__ == "__main__":
    sys.exit(main())
