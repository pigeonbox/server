#!/bin/sh
# 解析本容器**实际监听端口**，供 HEALTHCHECK 使用（server#136）。
#
# 优先级与 core 的配置覆盖顺序一致（pb_server_port > PORT > yaml）：
#   1. PB_HEALTH_PORT   显式健康检查覆盖（运维特殊场景）
#   2. PB_SERVER_PORT   core 的 server.port 环境变量覆盖
#   3. PORT             兼容 PORT 习惯用法（core 同样识别）
#   4. yaml 配置里的 server.port（路径参数 / CONFIG_PATH / 默认）
#   5. PB_DEFAULT_PORT  镜像构建期默认（Dockerfile ARG/ENV，默认 12345）
#
# 输出：纯端口数字（无换行以外的修饰）。解析失败时回退默认值，绝不输出空。
set -u

CFG="${1:-${CONFIG_PATH:-./config/config.yaml}}"

PORT="${PB_HEALTH_PORT:-${PB_SERVER_PORT:-${PORT:-}}}"

if [ -z "$PORT" ] && [ -f "$CFG" ]; then
    PORT=$(awk '
        /^[[:space:]]*#/ { next }
        /^[[:space:]]*$/ { next }
        /^[^[:space:]]/ {
            in_server = ($1 == "server:")
            next
        }
        in_server && $1 == "port:" {
            v = $2
            gsub(/[^0-9]/, "", v)
            if (v != "") { print v; exit }
        }
    ' "$CFG")
fi

# 只接受十进制端口；配置写错（如 "abc"）时回退而不是让 HEALTHCHECK 打空 URL。
case "$PORT" in
    ''|*[!0-9]*) PORT="${PB_DEFAULT_PORT:-12345}" ;;
esac

printf '%s\n' "$PORT"