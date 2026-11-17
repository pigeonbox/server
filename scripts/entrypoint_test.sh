#!/bin/sh
# entrypoint 端口探测测试（server#136）：--config 指向 8080 时，
# /tmp/pb-health-port 必须写 8080，且探针输出同一值。
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

cat > "$TMP/cfg.yaml" <<'EOF'
server:
  host: "0.0.0.0"
  port: 8080
EOF

fail=0
check() {
    label="$1"
    expected="$2"
    shift 2
    actual=$(env -u PB_HEALTH_PORT -u PB_SERVER_PORT -u PORT -u PB_DEFAULT_PORT \
        PB_ADMIN_PASSWORD=test-only PB_HEALTH_PORT_PROBE=1 \
        PB_HEALTH_PORT_FILE="$TMP/port" sh "$DIR/docker-entrypoint.sh" "$@" 2>/dev/null | tail -1)
    if [ "$actual" = "$expected" ]; then
        printf '  ✓ %s = %s\n' "$label" "$actual"
    else
        printf '  ✗ %s: 期望 %s，实际 %s\n' "$label" "$expected" "$actual" >&2
        fail=1
    fi
    file=$(cat "$TMP/port" 2>/dev/null || true)
    if [ "$file" = "$expected" ]; then
        printf '  ✓ %s 写入端口文件 = %s\n' "$label" "$file"
    else
        printf '  ✗ %s 端口文件: 期望 %s，实际 %s\n' "$label" "$expected" "$file" >&2
        fail=1
    fi
}

echo "── entrypoint 实际端口落盘"
check "--config 8080" 8080 --config "$TMP/cfg.yaml"
check "--config=8080" 8080 "--config=$TMP/cfg.yaml"

# 环境变量覆盖优先于 --config（与 core 一致）
actual=$(env -u PB_HEALTH_PORT -u PORT -u PB_DEFAULT_PORT PB_SERVER_PORT=9090 \
    PB_ADMIN_PASSWORD=test-only PB_HEALTH_PORT_PROBE=1 \
    PB_HEALTH_PORT_FILE="$TMP/port" sh "$DIR/docker-entrypoint.sh" --config "$TMP/cfg.yaml" 2>/dev/null | tail -1)
if [ "$actual" = 9090 ]; then
    printf '  ✓ PB_SERVER_PORT 覆盖 --config = %s\n' "$actual"
else
    printf '  ✗ PB_SERVER_PORT 覆盖 --config: 期望 9090，实际 %s\n' "$actual" >&2
    fail=1
fi

if [ "$fail" -ne 0 ]; then
    echo "✗ entrypoint 端口探测测试失败" >&2
    exit 1
fi
echo "✓ entrypoint 端口探测测试通过"