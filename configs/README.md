# configs/ - 配置文件目录

server 的配置模板目录。本目录**只有 YAML 模板，不含 Go 代码**——配置结构定义与
加载逻辑在 core 仓 [`conf/`](https://github.com/pigeonbox/core/tree/main/conf)。

## 文件说明

| 文件 | 用途 |
|------|------|
| `config.yaml` | 默认/开发配置 |
| `config.example.yaml` | 带完整注释的示例模板 |
| `config.prod.yaml` | 生产配置模板 |

## 关键默认值

- 端口：`server.port: 12345`（env `PB_SERVER_PORT` 可覆盖）
- 数据库：默认 SQLite（`./data/fileCodeBox.db`），可切 mysql/postgresql
- Redis：可选；多副本（public/admin 模式）与配置广播场景必配
- 存储后端：`storage.type` 支持 local/s3 等共 14 种

## 配置加载与覆盖优先级

```
--config 启动参数 / CONFIG_PATH env 指定文件 → yaml 默认值 → PB_* 环境变量覆盖（优先级最高）
```

敏感项（JWT secret/数据库/Redis/管理员密码等）一律用环境变量注入，
完整清单见 hub 仓 [`docs/ENVIRONMENT_VARIABLES.md`](https://github.com/pigeonbox/pigeonbox/blob/main/docs/ENVIRONMENT_VARIABLES.md)。

## 镜像 HEALTHCHECK 与端口（server#136）

镜像不再硬编码 HEALTHCHECK 端口 12345：entrypoint 按
`PB_SERVER_PORT` / `PORT` / `--config`（含 `CONFIG_PATH`）解析实际监听端口并写入
`/tmp/pb-health-port`，HEALTHCHECK 读取同一文件后请求 `/live`。

| 变量 | 默认 | 说明 |
|---|---|---|
| `PB_HEALTH_PORT` | 空 | 显式覆盖 HEALTHCHECK 探测端口（空=按实际监听端口） |
| `PB_DEFAULT_PORT` | 12345 | 镜像构建期默认端口（Dockerfile ARG；必须与 `config.yaml` 的 `server.port` 一致） |
| `PB_HEALTH_PORT_FILE` | `/tmp/pb-health-port` | entrypoint 写入实际端口的文件路径（一般无需修改） |

`docker run -e PB_SERVER_PORT=8080 ...` 或用 `--config` 改端口后，容器健康状态仍能正确变绿。

## 部署模式（多副本）

三个模板均含 `deployment` 注释段：`deployment.mode`（env `PB_DEPLOY_MODE`）支持
`standalone`（默认，单进程全功能）/ `public`（公开面副本，可多实例）/ `admin`（管理面单实例）。
public/admin 硬约束：mysql/postgresql + Redis 必配。

设计详见 hub 仓 `docs/specs/2026-10-06-multi-replica-deployment-modes.md`；
Kubernetes 拓扑用 charts 仓 `pigeonbox` chart 2.0+（`replicaCount>1` 自动拆分双 Deployment）。

## 注意

- 不要将真实密码提交到版本控制；模板中的默认值仅为占位
- 管理后台在线改的站点配置持久化在 DB `system_configs` 表，不回写本目录 yaml
