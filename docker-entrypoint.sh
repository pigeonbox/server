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

exec ./server --config ./config/config.yaml
