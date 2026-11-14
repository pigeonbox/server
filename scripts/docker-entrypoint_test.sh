#!/bin/sh
# docker-entrypoint.sh 的单测（pigeonbox/server#126 验收判据）。
#
# 判据映射：
#   · 「无参时给默认起服命令（默认行为不变）」      → 用例 1；
#   · 「docker run <镜像> --config x 真生效」        → 用例 2（原实现完全不看参数）；
#   · 「docker run <镜像> pb doctor 真透传」         → 用例 3（原实现静默忽略）；
#   · 「显式程序路径原样透传（不被改写）」           → 用例 4。
#
# 负对照（复现）：把 ENTRYPOINT 指向改动前的脚本 ⇒ 用例 2/3/4 必须红：
#   ENTRYPOINT=/tmp/orig-entrypoint.sh sh scripts/docker-entrypoint_test.sh
#
# 隔离：只设 PB_JWT_SECRET / PB_ADMIN_PASSWORD 跳过首启便利逻辑（与本判据无关）；
# PATH 收窄到 /usr/bin:/bin，保证 command -v pb 在任何机器上都判「不在 PATH」。

set -eu

OK=0
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ENTRYPOINT=${ENTRYPOINT:-"$SCRIPT_DIR/../docker-entrypoint.sh"}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/entrypoint-test.XXXXXX")
# EXIT trap 用 OK 哨兵形态（hub 惯例）：set -u 失败路径上 trap 入口 $? 可能是 0。
trap 'rc=$?; rm -rf -- "$tmp"; if [ "$rc" -ne 0 ]; then exit "$rc"; fi; if [ "$OK" -ne 1 ]; then exit 1; fi; exit 0' EXIT

[ -f "$ENTRYPOINT" ] || { echo "FAIL 找不到被测脚本: $ENTRYPOINT"; exit 1; }

# ── 假 /app 布局：entrypoint 用相对路径 exec ./server（镜像 WORKDIR /app）────
cp "$ENTRYPOINT" "$tmp/docker-entrypoint.sh"
cat > "$tmp/server" <<'MOCK'
#!/bin/sh
printf 'server'
for a in "$@"; do printf ' <%s>' "$a"; done
printf '\n'
MOCK
cat > "$tmp/pb" <<'MOCK'
#!/bin/sh
printf 'pb'
for a in "$@"; do printf ' <%s>' "$a"; done
printf '\n'
MOCK
chmod +x "$tmp/server" "$tmp/pb" "$tmp/docker-entrypoint.sh"

# 跳过首启便利逻辑（JWT 生成 / 向导模式提示），只留参数透传判据
PB_JWT_SECRET=test-secret
PB_ADMIN_PASSWORD=test-password
PB_DISABLE_DEFAULT_ADMIN=true
export PB_JWT_SECRET PB_ADMIN_PASSWORD PB_DISABLE_DEFAULT_ADMIN

run_ep() {
	(cd "$tmp" && PATH=/usr/bin:/bin /bin/sh ./docker-entrypoint.sh "$@")
}

fails=0
expect() {
	# expect <用例名> <期望输出> <实际输出>
	if [ "$2" != "$3" ]; then
		echo "FAIL $1"
		echo "  got : $3"
		echo "  want: $2"
		fails=$((fails + 1))
	fi
}

# 用例 1：无参 ⇒ 默认起服命令（与 Dockerfile CMD 同形，直跑 entrypoint 行为不变）
out=$(run_ep)
expect "无参给默认起服命令" "server <--config> <./config/config.yaml>" "$out"

# 用例 2：首参是旗标（docker run <镜像> --config x ⇒ CMD 被整个替换掉）
out=$(run_ep --config /app/config/custom.yaml)
expect "自定义 --config 真生效" "server <--config> </app/config/custom.yaml>" "$out"

# 用例 3：子命令透传（docker run <镜像> pb doctor ⇒ ./pb 在 WORKDIR 下）
out=$(run_ep pb doctor)
expect "pb 子命令透传" "pb <doctor>" "$out"

# 用例 4：显式路径原样透传（不被改写成 ./server）
out=$(run_ep ./server --help)
expect "显式路径透传" "server <--help>" "$out"

if [ "$fails" -ne 0 ]; then
	echo "$fails 条判据失败"
	exit 1
fi
OK=1
echo "docker-entrypoint selftest OK"
