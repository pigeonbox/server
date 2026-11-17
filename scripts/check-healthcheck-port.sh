#!/bin/bash
# HEALTHCHECK 端口一致性门禁（server#136）。
#
# 约束：
#   1) Dockerfile 的 HEALTHCHECK 不得硬编码端口字面量（12345/localhost:port），
#      必须调用 healthcheck.sh，由它解析实际监听端口；
#   2) 镜像构建期默认端口 PB_DEFAULT_PORT 必须与 configs/config.yaml 的
#      server.port 一致（默认 12345，保持兼容）；
#   3) entrypoint 必须把实际端口写入 /tmp/pb-health-port，healthcheck 必须读它；
#   4) EXPLOSE 默认值与 config 默认值一致（文档面不漂移）。
set -euo pipefail

cd "$(dirname "$0")/.."
DF="Dockerfile"
CFG="configs/config.yaml"
FAIL=0

chk() {
    local desc="$1"; shift
    if "$@"; then
        printf '  ✓ %s\n' "$desc"
    else
        printf '  ✗ %s\n' "$desc" >&2
        FAIL=1
    fi
}

# configs/config.yaml 的 server.port
cfg_port=$(awk '
    /^[[:space:]]*#/ { next }
    /^[[:space:]]*$/ { next }
    /^[^[:space:]]/ { in_server = ($1 == "server:"); next }
    in_server && $1 == "port:" { v = $2; gsub(/[^0-9]/, "", v); if (v != "") { print v; exit } }
' "$CFG")
arg_port=$(sed -n 's/^ARG PB_DEFAULT_PORT=\([0-9][0-9]*\).*/\1/p' "$DF" | head -1)

# HEALTHCHECK 指令块（含续行）
hc=$(awk '/^HEALTHCHECK/{flag=1} flag{print} flag && !/\\$/{exit}' "$DF")

echo "── HEALTHCHECK 端口一致性门禁"
chk "configs/config.yaml 有 server.port（当前 ${cfg_port:-缺}）" test -n "$cfg_port"
chk "Dockerfile 有 ARG PB_DEFAULT_PORT（当前 ${arg_port:-缺}）" test -n "$arg_port"
chk "构建期默认端口 = 配置默认端口（${arg_port:-?} = ${cfg_port:-?}）" test "$arg_port" = "$cfg_port"
chk "HEALTHCHECK 调用 /app/healthcheck.sh" bash -c "printf '%s' \"\$1\" | grep -Fq '/app/healthcheck.sh'" _ "$hc"
chk "HEALTHCHECK 不再硬编码 localhost:端口" bash -c "! printf '%s' \"\$1\" | grep -Eq 'localhost:[0-9]+'" _ "$hc"
chk "HEALTHCHECK 不再硬编码 127.0.0.1:端口" bash -c "! printf '%s' \"\$1\" | grep -Eq '127\\.0\\.0\\.1:[0-9]+'" _ "$hc"
chk "healthcheck.sh 读取 PB_HEALTH_PORT" grep -Fq 'PB_HEALTH_PORT' healthcheck.sh
chk "healthcheck.sh 读取 /tmp/pb-health-port" grep -Fq '/tmp/pb-health-port' healthcheck.sh
chk "entrypoint 写 /tmp/pb-health-port" grep -Fq '/tmp/pb-health-port' docker-entrypoint.sh
chk "entrypoint 调用 resolve-health-port.sh" grep -Fq 'resolve-health-port.sh' docker-entrypoint.sh
chk "resolver 与 core 同序识别 PB_SERVER_PORT" grep -Fq 'PB_SERVER_PORT' scripts/resolve-health-port.sh
chk "resolver 识别 yaml server.port" grep -Fq 'in_server && $1 == "port:"' scripts/resolve-health-port.sh
chk "EXPOSE 跟随 PB_DEFAULT_PORT（不再写死）" bash -c "grep -Fq 'EXPOSE \${PB_DEFAULT_PORT}' '$DF'"

if [ "$FAIL" -ne 0 ]; then
    echo "✗ HEALTHCHECK 端口一致性门禁失败" >&2
    exit 1
fi
echo "✓ HEALTHCHECK 端口一致性门禁通过"