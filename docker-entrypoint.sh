#!/bin/sh
# PigeonBox server 镜像入口（2026-10-22 起下沉到镜像层）。
#
# ## 为什么要有这个脚本
#
# 首启的两种走法：
#   1. 预设密码：PB_ADMIN_PASSWORD 非空 → core 按该密码建 admin
#   2. 首启向导：PB_DISABLE_DEFAULT_ADMIN=true → 不建默认 admin，打开站点走 /setup
#
# core 的生产门禁是**对的**：生产模式下没有强密码就拒绝启动，绝不播种弱口令
# admin/admin123（ErrInsecureDefaultAdmin）。但「怎么进入向导模式」这件事
# 此前**只写在 compose 的 entrypoint 脚本里** ⇒ 直接 `docker run` 官方镜像的
# 用户（以及任何自建编排）会撞墙：
#
#   bootstrap failed: refusing to create default admin with known default
#   password in production mode: set PB_ADMIN_PASSWORD env ..., or set
#   PB_DISABLE_DEFAULT_ADMIN=true ...
#
# 同一张官方镜像、两种起法、行为不一致 —— 这是纯粹的易用性坑，与安全无关。
# 本脚本把「PB_ADMIN_PASSWORD 为空 ⇒ 自动切向导模式」这条**编排无关**的
# 便利逻辑下沉到镜像层，让 compose / docker run / k8s 三种起法行为一致。
#
# ## 语义
#
#   PB_ADMIN_PASSWORD 为空            → 自动切向导模式（导出 PB_DISABLE_DEFAULT_ADMIN=true）
#   PB_ADMIN_PASSWORD 非空            → 保持原样（按该密码建 admin）
#   用户显式设了 PB_DISABLE_DEFAULT_ADMIN → 原样尊重（不覆盖）
#
# 注意：这只处理**入口便利**；「生产模式必须有强密码」的门禁仍在 core 侧，
# 本脚本不放松它 —— 留空时是「不建默认账号、让你自己当场建一个」，不是
# 「建一个弱口令账号」。
#
# 另：JWT 密钥留空时容器首启自动生成强随机值并持久化到数据目录（.jwt_secret），
# 该逻辑同样在编排层（compose entrypoint），此处一并下沉，保证直跑镜像也具备。

set -e

# ── JWT 密钥：留空则首启生成并持久化（重启/升级不丢）────────────────────
if [ -z "${PB_JWT_SECRET:-}" ] && [ -f /app/data/.jwt_secret ]; then
    PB_JWT_SECRET="$(cat /app/data/.jwt_secret)"
    export PB_JWT_SECRET
fi
if [ -z "${PB_JWT_SECRET:-}" ]; then
    PB_JWT_SECRET="$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 48)"
    export PB_JWT_SECRET
    if [ -w /app/data ]; then
        printf '%s' "$PB_JWT_SECRET" > /app/data/.jwt_secret
        chmod 600 /app/data/.jwt_secret
    fi
fi

# ── 首启：密码留空 ⇒ 走 /setup 向导（不播种弱口令 admin/admin123）──────
if [ -z "${PB_ADMIN_PASSWORD:-}" ] && [ "${PB_DISABLE_DEFAULT_ADMIN:-}" != "true" ]; then
    PB_DISABLE_DEFAULT_ADMIN=true
    export PB_DISABLE_DEFAULT_ADMIN
    echo "[entrypoint] 未设 PB_ADMIN_PASSWORD：已切换到 /setup 首启向导模式（打开站点即可建管理员）" >&2
    if [ "${PB_PRODUCTION:-}" = "1" ]; then
        echo "[entrypoint] 公网部署建议再设 PB_SETUP_TOKEN=<任意非空值>，避免未初始化实例被他人抢先建号" >&2
    fi
fi

# ── 参数透传（issue #126）──────────────────────────────────────────────
# 此前末行硬编码 `exec ./server --config ./config/config.yaml`（没有 $@）：
# CMD 成了死参数，`docker run <镜像> --config /app/config/custom.yaml` 收下参数却
# 完全不看，仍按默认配置起服且无任何提示（用户以为换了配置实际没有）。
#
# 三种走法（默认行为与改动前一致，有参时才真透传）：
#   1. 无参（直接跑 entrypoint / CMD 被清空）→ 补默认起服命令（与 Dockerfile
#      CMD 同形）⇒ 直跑镜像的默认行为不变；
#   2. 首参是**旗标**（`docker run <镜像> --config x`）→ docker 会用用户参数
#      **整个替换 CMD** ⇒ 脚本收到的是旗标而非程序名 ⇒ 先补默认程序 ./server，
#      否则 exec 会去执行 "--config" 本身；
#   3. 其余（`docker run <镜像> pb doctor`、`docker run <镜像> ./server ...`）→
#      原样透传；非路径形式且不在 PATH 里、但 WORKDIR 下存在同名文件（./pb）
#      时补 ./ 前缀，否则 alpine 的 PATH 里没有 /app 会 exec 失败。
if [ "$#" -eq 0 ]; then
    set -- ./server --config ./config/config.yaml
else
    case "$1" in
        -*) set -- ./server "$@" ;;
        */*) ;;
        *)
            # 替换而不是前置：set -- "./$1" "$@" 会把原 $1 也留在参数表里
            # （实测得到 "pb pb doctor"）⇒ 先 shift 再补回。
            if [ -e "./$1" ] && ! command -v "$1" >/dev/null 2>&1; then
                prog="./$1"
                shift
                set -- "$prog" "$@"
            fi
            ;;
    esac
fi
exec "$@"
