#!/bin/sh
# go-mod-download-retry.sh 的单测（pigeonbox/server#112 验收判据）。
#
# 判据映射：
#   · 「模拟 503：第一次失败、之后成功 ⇒ 构建成功」→ 用例 1（假 go 前两次
#     exit 1 模拟 503、第三次 exit 0）⇒ 退出 0 且 go 恰好被调 3 次；
#   · 「有界」→ 用例 2（假 go 恒失败）⇒ 退出码非 0 且恰好尝试 3 次（不无限）；
#   · 「成功不重试」→ 用例 3（假 go 首次即成功）⇒ 恰好调 1 次；
#   · 「RETRY_DELAYS 控制重试间隔」→ 用例 4/5（假 sleep 记录每次间隔参数）⇒
#     逐次间隔与表一致；条目不足沿用最后一个值。
#
# set -e 语义边界（任务要求显式验证的两条）：
#   · 「循环体失败不逃逸循环」：用例 1 的 go 调用次数=3 是直接证据——
#     若 set -e 在第一次失败时就退出脚本，次数会是 1 且退出码非 0；
#   · 「最终失败不被静默吞掉」：用例 2 退出码非 0。
#
# 全部用例经 RETRY_DELAYS=0 注入零延迟，且 sleep 被 mock ⇒ 测试零真睡。

set -eu

OK=0
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
RETRY="$SCRIPT_DIR/go-mod-download-retry.sh"

tmp_root=$(mktemp -d "${TMPDIR:-/tmp}/goretry-test.XXXXXX")
# EXIT trap 用 OK 哨兵形态（hub 惯例）：set -u 失败路径上 macOS bash 3.2 的
# trap 入口 $? 可能是 0 ⇒ 不能只信 rc，还要看「跑到了结尾」的哨兵。
trap 'rc=$?; rm -rf -- "$tmp_root"; if [ "$rc" -ne 0 ]; then exit "$rc"; fi; if [ "$OK" -ne 1 ]; then exit 1; fi; exit 0' EXIT

MOCK_BIN="$tmp_root/mock/bin"
mkdir -p "$MOCK_BIN"

# 假 go：每次调用计数 +1（FAKE_GO_COUNT_FILE），按 FAKE_GO_MODE 决定退出码
cat > "$MOCK_BIN/go" <<'MOCK'
#!/bin/sh
count_file=${FAKE_GO_COUNT_FILE:?FAKE_GO_COUNT_FILE required}
mode=${FAKE_GO_MODE:?FAKE_GO_MODE required}
count=$(cat "$count_file" 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > "$count_file"
case "$mode" in
fail-twice-then-ok)
	[ "$count" -ge 3 ] && exit 0
	exit 1
	;;
always-ok)
	exit 0
	;;
*)
	exit 1
	;;
esac
MOCK

# 假 sleep：把间隔参数记进 FAKE_SLEEP_LOG 后立即返回 ⇒ 测试零真睡，
# 且 RETRY_DELAYS 的逐次间隔可被精确断言
cat > "$MOCK_BIN/sleep" <<'MOCK'
#!/bin/sh
printf '%s\n' "$1" >> "${FAKE_SLEEP_LOG:?FAKE_SLEEP_LOG required}"
exit 0
MOCK

chmod 0755 "$MOCK_BIN/go" "$MOCK_BIN/sleep"

pass=0
fail=0

assert_eq() {
	desc=$1
	actual=$2
	expected=$3
	if [ "$actual" = "$expected" ]; then
		echo "PASS: $desc"
		pass=$((pass + 1))
	else
		echo "FAIL: $desc —— 期望 [$expected] 实际 [$actual]" >&2
		fail=$((fail + 1))
	fi
}

assert_ne() {
	desc=$1
	actual=$2
	expected=$3
	if [ "$actual" != "$expected" ]; then
		echo "PASS: $desc"
		pass=$((pass + 1))
	else
		echo "FAIL: $desc —— 不应等于 [$expected]" >&2
		fail=$((fail + 1))
	fi
}

# run_retry <mode> <delays>：在临时目录里跑一次 retry 脚本
# （PATH 前置假 go/假 sleep），stdout 输出 "<退出码> <go调用次数> <间隔序列>"，
# 间隔序列 = 假 sleep 收到的参数按行逗号连接（无 sleep 则为空串）。
run_retry() {
	mode=$1
	delays=$2
	case_dir=$(mktemp -d "$tmp_root/case.XXXXXX")
	count_file="$case_dir/count"
	sleep_log="$case_dir/sleeps"
	: > "$count_file"
	: > "$sleep_log"
	rc=0
	(
		cd "$case_dir"
		PATH="$MOCK_BIN:$PATH"
		export PATH
		FAKE_GO_COUNT_FILE="$count_file"
		FAKE_GO_MODE="$mode"
		FAKE_SLEEP_LOG="$sleep_log"
		RETRY_DELAYS="$delays"
		export FAKE_GO_COUNT_FILE FAKE_GO_MODE FAKE_SLEEP_LOG RETRY_DELAYS
		exec sh "$RETRY"
	) || rc=$?
	calls=$(cat "$count_file")
	sleeps=$(tr '\n' ',' < "$sleep_log" | sed 's/,$//')
	rm -rf "$case_dir"
	echo "$rc $calls $sleeps"
}

# ── 用例 1（issue 验收判据）：模拟 503——前两次失败、第三次成功 ⇒ 构建成功
out=$(run_retry fail-twice-then-ok "0,0")
assert_eq "503 两次后成功 ⇒ 退出码 0" "${out%% *}" "0"
assert_eq "503 两次后成功 ⇒ go 恰好被调 3 次（set -e 不逃逸循环的直接证据）" \
	"$(printf '%s' "$out" | cut -d' ' -f2)" "3"
assert_eq "503 两次后成功 ⇒ 两次重试间隔均按表（0,0）" \
	"$(printf '%s' "$out" | cut -d' ' -f3)" "0,0"

# ── 用例 2：恒失败 ⇒ 有界放弃（退出码非 0，恰好 3 次，不无限重试）
out=$(run_retry always-fail "0,0")
assert_ne "恒失败 ⇒ 退出码非 0（最终失败不被静默吞掉）" "${out%% *}" "0"
assert_eq "恒失败 ⇒ 恰好尝试 3 次（有界）" \
	"$(printf '%s' "$out" | cut -d' ' -f2)" "3"
assert_eq "恒失败 ⇒ 重试间隔恰好 2 次（attempt 之间才睡）" \
	"$(printf '%s' "$out" | cut -d' ' -f3)" "0,0"

# ── 用例 3：首次即成功 ⇒ 不重试
out=$(run_retry always-ok "0,0")
assert_eq "首次成功 ⇒ 退出码 0" "${out%% *}" "0"
assert_eq "首次成功 ⇒ go 恰好被调 1 次（不重试）" \
	"$(printf '%s' "$out" | cut -d' ' -f2)" "1"
assert_eq "首次成功 ⇒ 零次 sleep" \
	"$(printf '%s' "$out" | cut -d' ' -f3)" ""

# ── 用例 4：RETRY_DELAYS 逐次间隔表生效（2s、7s——被假 sleep 记录，不真睡）
out=$(run_retry fail-twice-then-ok "2,7")
assert_eq "间隔表 2,7 ⇒ 退出码 0" "${out%% *}" "0"
assert_eq "间隔表 2,7 ⇒ 逐次间隔为 2 然后 7" \
	"$(printf '%s' "$out" | cut -d' ' -f3)" "2,7"

# ── 用例 5：RETRY_DELAYS 条目不足 ⇒ 沿用最后一个值
out=$(run_retry fail-twice-then-ok "0")
assert_eq "间隔表只有一项 ⇒ 退出码 0" "${out%% *}" "0"
assert_eq "间隔表只有一项 ⇒ 两次重试都沿用该项（0,0）" \
	"$(printf '%s' "$out" | cut -d' ' -f3)" "0,0"

echo "----"
echo "PASS=$pass FAIL=$fail"
if [ "$fail" -eq 0 ]; then
	OK=1
	exit 0
fi
exit 1
