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


def hub_tags(repo: str, tag: str) -> list[str]:
    """Docker Hub 侧的 tag 集：version / major.minor / latest。

    与 metadata-action 的语义对齐（server 与 frontend 两侧都带 latest）。
    """
    v = tag.lstrip("v")
    mm = major_minor(tag)
    return [
        f"docker.io/{repo}:{v}",
        f"docker.io/{repo}:{mm}",
        f"docker.io/{repo}:latest",
    ]


def compose(ghcr: str, enabled: bool, hub_repo: str | None, tag: str) -> list[str]:
    """合成最终 tag 列表。

    ⚠️ 两个镜像的 ghcr tag 必须**各用各的**（server 用 .../server、
    frontend 用 .../frontend）——混用会把 server 镜像推到 frontend 名下。
    """
    base = [t.strip() for t in ghcr.splitlines() if t.strip()]
    if not enabled:
        return base
    if not hub_repo:
        raise ValueError("启用 Docker Hub 时必须给出仓库名")
    return base + hub_tags(hub_repo, tag)


def emit(name: str, tags: list[str]) -> str:
    """输出 GitHub Actions 的多行输出格式。"""
    body = "\n".join(tags)
    return f"{name}<<EOF\n{body}\nEOF"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--tag", required=True, help="版本 tag，如 v4.5.2")
    ap.add_argument("--enabled", required=True, choices=["true", "false"],
                    help="Docker Hub 凭据是否就位")
    ap.add_argument("--ghcr-server", default="")
    ap.add_argument("--ghcr-frontend", default="")
    ap.add_argument("--hub-server-repo", default="")
    ap.add_argument("--hub-frontend-repo", default="")
    args = ap.parse_args()

    enabled = args.enabled == "true"
    try:
        server_tags = compose(args.ghcr_server, enabled, args.hub_server_repo, args.tag)
        frontend_tags = compose(args.ghcr_frontend, enabled,
                                args.hub_frontend_repo, args.tag)
    except ValueError as exc:
        print(f"错误：{exc}", file=sys.stderr)
        return 1

    print(emit("server_tags", server_tags))
    print(emit("frontend_tags", frontend_tags))
    return 0


if __name__ == "__main__":
    sys.exit(main())
