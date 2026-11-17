#!/bin/sh
# resolve-health-port.sh 单测（server#136）：端口优先级必须与 core 一致。
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
RES="$DIR/resolve-health-port.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

fail=0
assert_eq() {
    expected="$1"
    actual="$2"
    label="$3"
    if [ "$expected" = "$actual" ]; then
        printf '  ✓ %s = %s\n' "$label" "$actual"
    else
        printf '  ✗ %s: 期望 %s，实际 %s\n' "$label" "$expected" "$actual" >&2
        fail=1
    fi
}

write_cfg() {
    cat > "$1" <<'EOF'
# 测试配置
database:
  port: 3306
server:
  host: "0.0.0.0"
  port: 8080   # 注释与行尾注释都要能解析
redis:
  port: 6379
EOF
}

noenv() {
    env -u PB_HEALTH_PORT -u PB_SERVER_PORT -u PORT -u PB_DEFAULT_PORT "$@"
}

echo "── resolve-health-port 端口优先级"
write_cfg "$TMP/cfg.yaml"

assert_eq 8080 "$(noenv sh "$RES" "$TMP/cfg.yaml")" "yaml server.port"
assert_eq 9090 "$(noenv PB_SERVER_PORT=9090 sh "$RES" "$TMP/cfg.yaml")" "PB_SERVER_PORT 覆盖 yaml"
assert_eq 7070 "$(noenv PORT=7070 sh "$RES" "$TMP/cfg.yaml")" "PORT 覆盖 yaml"
assert_eq 9090 "$(noenv PB_SERVER_PORT=9090 PORT=7070 sh "$RES" "$TMP/cfg.yaml")" "PB_SERVER_PORT 优先于 PORT"
assert_eq 6060 "$(noenv PB_HEALTH_PORT=6060 PB_SERVER_PORT=9090 sh "$RES" "$TMP/cfg.yaml")" "PB_HEALTH_PORT 最高优先"

printf 'server:\n  port: 12345\n' > "$TMP/default.yaml"
assert_eq 12345 "$(noenv sh "$RES" "$TMP/default.yaml")" "默认配置 12345"

assert_eq 54321 "$(noenv PB_DEFAULT_PORT=54321 sh "$RES" "$TMP/missing.yaml")" "配置缺失回退 PB_DEFAULT_PORT"
assert_eq 54321 "$(noenv PB_DEFAULT_PORT=54321 "PB_SERVER_PORT=abc" sh "$RES" "$TMP/missing.yaml")" "非法端口回退 PB_DEFAULT_PORT"
assert_eq 12345 "$(noenv sh "$RES" "$TMP/missing.yaml")" "环境与配置全缺失回退 12345"

if [ "$fail" -ne 0 ]; then
    echo "✗ resolve-health-port 单测失败" >&2
    exit 1
fi
echo "✓ resolve-health-port 单测通过"