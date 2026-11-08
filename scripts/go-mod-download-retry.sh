#!/bin/sh
# go mod download 有界重试（pigeonbox/server#112）。
#
# 为什么需要（issue #112 根因，2026-11-08 实录 run 42396086185）：
#   goproxy.cn 对刚推送的 tag 有分钟级同步延迟，窗口内对该模块返回 503。
#   Go 的 GOPROXY 逗号分隔多源**回退只对 404/410 生效**（官方语义：404/410 =
#   「该源不存在此模块」⇒ 试下一源），**503 视为真错误直接失败** ⇒
#   `GOPROXY=goproxy.cn,proxy.golang.org` 也救不了同步窗口内的构建
#   ⇒ CI 红、rerun 自愈（每次撞上都要人肉重跑一次）。
#   ⇒ 构建期对 `go mod download` 做有界重试，覆盖分钟级同步窗口：
#     默认 3 次尝试 × 间隔 15s。真错误（invalid version 等）也会重试，
#     多花 ~30s 可接受（issue 提案 1：最小改）。
#
# 用法（在含 go.mod 的模块目录内）：
#   sh go-mod-download-retry.sh
#
# 环境变量：
#   RETRY_DELAYS  逗号分隔的重试间隔秒数，默认 "15,15"
#                 （第 1/2 次重试前各睡 15s；条目不足时沿用最后一个值）。
#                 测试可传 "0,0"（或 "0"）注入零延迟免真睡。
#
# set -e 语义说明（为什么用 while+if 而不是 for+&&）：
#   `go mod download` 放在 if 条件位置 ⇒ 其失败不触发 set -e
#   （POSIX：条件上下文中的命令失败不退出）⇒ 失败后由循环体显式决定
#   重试或 `exit 1`。不存在「循环体失败逃逸循环」（循环内失败被吞成
#   提前退出）或「最终失败仍 exit 0」（静默吞错）两种失效形态；
#   scripts/go-mod-download-retry_test.sh 用假 go 对两条形态都有断言。

set -eu

attempts=3
delays="${RETRY_DELAYS:-15,15}"

# pick_delay <k>：第 k 次重试前应睡的秒数。
# 从逗号分隔的 $delays 取第 k 项；条目不足沿用最后一项；表为空回落 15。
# 计数器用 j（与主循环的 i 不同名）：本函数在 $() 子 shell 里跑，但同名
# 会让人误以为能改主循环变量，换个名字杜绝这个重构陷阱。
pick_delay() {
	k=$1
	j=1
	last=""
	while [ "$j" -le "$k" ]; do
		v=$(printf '%s\n' "$delays" | cut -d, -f"$j")
		if [ -n "$v" ]; then
			last=$v
		fi
		j=$((j + 1))
	done
	printf '%s' "${last:-15}"
}

i=1
while [ "$i" -le "$attempts" ]; do
	if go mod download; then
		exit 0
	fi
	if [ "$i" -eq "$attempts" ]; then
		echo "go mod download: ${attempts} 次尝试均失败，放弃（真错误见上方 go 输出）" >&2
		exit 1
	fi
	echo "go mod download failed (attempt ${i}/${attempts}), retrying in $(pick_delay "$i")s ..." >&2
	sleep "$(pick_delay "$i")"
	i=$((i + 1))
done
