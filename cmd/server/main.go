package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oekit/core/lifecycle"
	"github.com/pigeonbox/core/app/dbmigrate"
	"github.com/pigeonbox/core/bootstrap"
	"github.com/pigeonbox/core/pkg/logger"
	"go.uber.org/zap"
)

func main() {
	// 离线子命令（backup / restore）：**不启动 HTTP 服务**。
	//
	// 导出/恢复是**同机**运维操作（要读本地 DB 与本地存储）；恢复还会**替换** DB
	// 文件 ⇒ 必须停服 ⇒ 让它走「另一个进程 + 显式 --yes」，而不是在服务端进程里
	// 自我替换（那会读到半旧半新的数据）。
	if len(os.Args) > 1 && isOfflineCommand(os.Args[1]) {
		switch os.Args[1] {
		case "backup", "restore":
			if err := runOfflineCommand(os.Args[1], os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				if errors.Is(err, errOfflineUsage) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			return
		case "db":
			// 换库（导出 / 导入）：只搬**数据库数据**，不碰存储对象；
			// 与 backup 的分工见 dbmigrate.go 顶部。
			if err := runDBMigrateCommand(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "%v\n", err)
				if errors.Is(err, errDBMigrateUsage) || errors.Is(err, dbmigrate.ErrImportNeedsConfirm) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			return
		}
	}

	// 解析命令行参数。
	// --config 指定配置文件路径；亦可通过 CONFIG_PATH 环境变量设置（flag 优先）。
	//
	// --help 的用法文本自定义（issue #130）：标准库默认只打印旗标表，
	// backup/restore/db 三个离线子命令**没有任何可发现入口**。
	flag.Usage = func() { serverUsage(flag.CommandLine.Output()) }
	configPath := flag.String("config", "", "配置文件路径（默认 configs/config.yaml，可用 CONFIG_PATH 环境变量覆盖）")
	flag.Parse()

	// 拼错/错位的子命令不再静默起服（issue #130）：
	// 标准库 flag 遇到第一个非旗标参数即**停止解析且不报错** ⇒ 原实现里
	// `pigeonbox bakup export --out x` 的全部参数被丢弃、直接 bootstrap
	// 起整个服务（期望的备份没做、可能还跑了迁移）。⇒ 位置参数非空即报
	// 用法错（退出码 2）并打印子命令用法。
	if err := validateStartupArgs(flag.Args()); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n\n", err)
		flag.Usage()
		os.Exit(2)
	}

	h, err := bootstrap.Bootstrap(*configPath)
	if err != nil {
		// bootstrap 早期失败时 logger 可能尚未初始化,先落 stderr 保证错误可见
		fmt.Fprintf(os.Stderr, "bootstrap failed: %v\n", err)
		logger.Fatal("bootstrap failed", zap.Error(err))
	}
	defer bootstrap.Cleanup()

	// 监听失败必须 fail-fast（2026-11-06 P1）。
	//
	// 缺陷：hertz v0.10.6 的 Spin() 在 ListenAndServe 出错时**只 Errorf 后
	// return**，不退出进程（见 pkg/app/server/hertz.go）。此前它跑在裸 goroutine
	// 里、返回值被丢弃 ⇒ 端口被占用 / 无监听权限 / 地址不可用时，进程照旧活着：
	// systemd / 容器认为服务「已启动且运行中」，健康检查却永远连不上，只能靠人
	// 发现。⇒ 现在盯住 Spin 的返回：非停机期提前返回即致命退出（非零退出码让
	// 编排层重启/告警）。
	//
	// ⚠️ 这里**只能有一处裸 go**（禁裸 go 门禁按「文件+函数」放行主服务协程，
	// 且每函数只许 1 处）⇒ 信号等待不用协程，改用 signal.NotifyContext（stdlib
	// 自带内部监听）。这条规则不是挡路：主流程等信号本来就不需要额外协程。
	spinDone := make(chan struct{})
	go func() {
		defer close(spinDone)
		logger.Info("Server starting...")
		h.Spin()
	}()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	// 优雅停机（oekit core/lifecycle，与 p2pd / core Cleanup 同款）：
	// http 排水拿独立 15s 预算——此前 context.Background() 无界，一个挂死
	// 连接即卡死整个停机流程，只能靠容器运行时 SIGKILL 兜底；排完后 main
	// 返回，后台任务排水等核心清理经 defer bootstrap.Cleanup() 接力。
	mgr := lifecycle.New(20 * time.Second)
	mgr.Add("http", func(ctx context.Context) error { return h.Shutdown(ctx) }, 15*time.Second)

	shutdown := func() {
		mgr.Shutdown(context.Background())
		// Spin 可能还在返回途中：给它同一份停机预算，避免与 defer Cleanup 并发收尾。
		select {
		case <-spinDone:
		case <-time.After(5 * time.Second):
			logger.Warn("HTTP 服务未在 5s 内退出，继续清理")
		}
		logger.Info("Server stopped")
	}

	select {
	case <-sigCtx.Done():
		shutdown()
	case <-spinDone:
		// Spin 先返回：只有「同一瞬间也收到了信号」才算正常停机——shutdown 会让
		// Spin 正常返回，两者可能同时就绪 ⇒ select 随机选中其一，故在此再判一次。
		select {
		case <-sigCtx.Done():
			shutdown()
		default:
			logger.Fatal("HTTP 服务异常退出：监听失败（端口被占用 / 无权限 / 地址不可用）")
		}
	}
}

// ===== issue #130：子命令可发现性与「拼错不静默起服」 =====

// offlineCommands 离线子命令白名单。
//
// ⚠️ main() 里的 switch 是**分发器**、这里是**白名单**：新增离线子命令时两处
// 同改（分发器决定行为，白名单决定「非空位置参数」是放行还是报用法错）。
var offlineCommands = []string{"backup", "restore", "db"}

// isOfflineCommand 判断首参是否离线子命令。
func isOfflineCommand(name string) bool {
	for _, c := range offlineCommands {
		if c == name {
			return true
		}
	}
	return false
}

// serverUsage server 二进制的用法（flag.Usage 与子命令报错共用同一份文本）。
//
// issue #130：此前文件里从未设置 flag.Usage ⇒ `--help` 只打印 -config 一个
// 旗标，三个离线子命令没有任何可发现入口。
func serverUsage(w io.Writer) {
	fmt.Fprint(w, `用法:
  pigeonbox [-config <路径>]                        启动 HTTP 服务
  pigeonbox backup export --out <文件> [--config <路径>]
                                                   物理快照导出（DB + 存储对象，同库同机）
  pigeonbox backup restore --from <文件> [--yes]    物理快照恢复（需停服）
  pigeonbox db export --out <文件> [--config <路径>]
                                                   逻辑换库导出（只搬数据库数据）
  pigeonbox db import --from <文件> --target-driver <sqlite|postgres|mysql>
                     [--target-dsn <dsn>] --yes [--allow-nonempty]
                                                   逻辑换库导入（目标库必须为空）

旗标:
  -config <路径>   配置文件路径（默认 configs/config.yaml，可用 CONFIG_PATH 环境变量覆盖）

示例:
  pigeonbox --config ./config/config.yaml
  pigeonbox backup export --out /backup/pigeonbox.tgz
  pigeonbox db import --from dump.json --target-driver sqlite --target-dsn /data/pb.db --yes
`)
}

// validateStartupArgs flag.Parse 之后的位置参数校验（issue #130）。
//
// 返回 nil = 可以继续起服；非 nil = 用法错误（调用方打印用法并 exit 2）。
// 依赖 Go flag 的标准契约：解析遇到第一个非旗标参数即停止且不报错 ⇒
// 「拼错子命令」不会自己浮出来，必须显式检查 flag.Args()。
func validateStartupArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	if isOfflineCommand(args[0]) {
		return fmt.Errorf("子命令 %s 必须写在旗标之前（如 pigeonbox %s ...）", args[0], args[0])
	}
	return fmt.Errorf("未知子命令: %s", args[0])
}
