#!/bin/sh
# 容器 HEALTHCHECK（server#136）：端口必须跟实际监听端口一致。
#
# 端口解析顺序：
#   1. PB_HEALTH_PORT（显式覆盖）
#   2. /tmp/pb-health-port（entrypoint 启动时写入的实际端口）
#   3. PB_SERVER_PORT / PORT（core 的环境变量覆盖）
#   4. PB_DEFAULT_PORT（Dockerfile 构建期默认，缺省 12345）
#
# 注意：不能在 HEALTHCHECK 里写死 12345——用户用 --config / PB_SERVER_PORT
# 改端口后，容器会永远 unhealthy。
set -u

PORT="${PB_HEALTH_PORT:-}"

if [ -z "$PORT" ] && [ -r /tmp/pb-health-port ]; then
    PORT=$(cat /tmp/pb-health-port 2>/dev/null || true)
fi

if [ -z "$PORT" ]; then
    PORT="${PB_SERVER_PORT:-${PORT:-${PB_DEFAULT_PORT:-12345}}}"
fi

case "$PORT" in
    ''|*[!0-9]*)
        echo "healthcheck: 端口非法: ${PORT:-<empty>}" >&2
        exit 1
        ;;
esac

# wget --spider 发 HEAD，/live 未注册 HEAD 会 404 导致健康检查永不通过，须显式 GET
exec wget -q -O /dev/null "http://127.0.0.1:${PORT}/live" || exit 1