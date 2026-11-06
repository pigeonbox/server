package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	if len(os.Args) > 1 {
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
	configPath := flag.String("config", "", "配置文件路径（默认 configs/config.yaml，可用 CONFIG_PATH 环境变量覆盖）")
	flag.Parse()

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
