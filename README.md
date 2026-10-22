# server · 独立部署应用

[![CI](https://github.com/pigeonbox/server/actions/workflows/ci.yml/badge.svg)](https://github.com/pigeonbox/server/actions/workflows/ci.yml)
[![Tag](https://img.shields.io/github/v/tag/pigeonbox/server)](https://github.com/pigeonbox/server/tags)
[![License](https://img.shields.io/github/license/pigeonbox/server)](LICENSE)

PigeonBox 独立部署应用:**纯后端**薄壳入口(main.go)+ 配置模板 + Dockerfile。**不含业务代码**——业务全部来自 [`pigeonbox/core`](https://github.com/pigeonbox/core) 库。

> 0.9.0 起"内嵌前端"模式已剔除:前端由同版本的 [`pigeonbox/frontend`](https://github.com/pigeonbox/frontend) nginx 镜像分离提供;core 对缺失的 `./static` 优雅降级(探针/API 全正常,SPA 路径 404),单跑也安全。

> 🗂️ [PigeonBox 生态](https://github.com/orgs/pigeonbox)成员仓 · 总览与部署见 [装配仓 pigeonbox](https://github.com/pigeonbox/pigeonbox) · [架构图集](https://github.com/pigeonbox/pigeonbox/blob/main/docs/architecture.md)

## 发布镜像

打 `v*` tag 后 CI 自动构建多架构镜像推 ghcr,**前后端镜像同版本发布**:

| 镜像 | 说明 |
|------|------|
| `ghcr.io/pigeonbox/server` | 纯后端 API(监听 :12345,仅业务) |
| `ghcr.io/pigeonbox/frontend` | nginx-unprivileged 静态资源 + API 反代(**对外统一入口**) |

自托管推荐形态:compose 或 Helm 起前后端双容器,frontend 承接全部对外流量(见 [charts](https://github.com/pigeonbox/charts) 与 hub 仓 [docs/DEPLOY-COMPOSE.md](https://github.com/pigeonbox/pigeonbox/blob/main/docs/DEPLOY-COMPOSE.md))。

## 目录结构

```
├── cmd/server/main.go   # 信号处理 + bootstrap.Bootstrap() 拉起 core
├── cmd/pb/             # 官方 CLI 客户端(文本/文件分享、本地导入、我的分享管理、p2p 联邦;API Key 认证)
├── configs/             # config.yaml / config.example.yaml / config.prod.yaml
├── Dockerfile           # 两阶段:go-builder → runtime(Go 依赖经 module proxy 拉正式版本)
└── Makefile
```

## 本地开发

依赖 sibling 目录(checkout 到同一父目录,`replace` 链互指;或直接用 hub 仓的 go.work):

```
pigeonbox/            # 工作区根
├── contracts/           # github.com/pigeonbox/contracts
├── core/                # github.com/pigeonbox/core
└── server/              # 本仓库
```

```bash
make run                 # 起服务(默认 :12345,sqlite)
make test
```

## Docker 构建

构建上下文是**工作区根**(仅消费 `server/` 子目录;core/contracts 经 go.mod 正式版本从 module proxy 解析,无需本地 replace 链):

```bash
docker build -f server/Dockerfile -t pigeonbox-server ..   # 在 server/ 内执行
# 或 make docker-build
```

## 设备直传已独立为 PigeonBeam(信鸽直传)

设备直传(p2pc)已拆为独立应用 **PigeonBeam(信鸽直传)**,本仓**不再随包 p2pc
二进制、不再编排设备直传**——镜像里没有 `/app/p2pc`,`pb` CLI 也没有直传子命令。
设备直传请使用 PigeonBeam 客户端。

本仓 CLI 只保留**联邦**四支子命令(走 admin API Key 通道,服务端 AdminOrAdminKey
复核 admin 角色):

```
pb p2p peers               在线设备名录(node_id 用于定向邀请)
pb p2p invites             待收邀请列表
pb p2p accept <invite_id>  认领邀请并接收
pb p2p push <目标地址> <口令> [附言]   推送口令到另一实例(免口令交接)
```

## 与 fnos 的关系

两者是 core 的并列消费方:server 面向通用自托管(纯后端 + 分离前端);fnos 面向飞牛 NAS 应用化(独立仓库,单容器库式调用)。

## License

[Apache-2.0](LICENSE)
