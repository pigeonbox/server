package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/oekit/logx/shutdown"
	"github.com/pigeonbox/core/bootstrap"
	"github.com/pigeonbox/core/pkg/logger"
	"go.uber.org/zap"
)

func main() {
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

	go func() {
		logger.Info("Server starting...")
		h.Spin()
	}()

	// 优雅停机（oekit logx/shutdown，与 p2pd / core Cleanup 同款）：
	// http 排水拿独立 15s 预算——此前 context.Background() 无界，一个挂死
	// 连接即卡死整个停机流程，只能靠容器运行时 SIGKILL 兜底；排完后 main
	// 返回，后台任务排水等核心清理经 defer bootstrap.Cleanup() 接力。
	mgr := shutdown.New(20 * time.Second)
	mgr.Add("http", func(ctx context.Context) error { return h.Shutdown(ctx) }, 15*time.Second)
	mgr.Listen(syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Server stopped")
}
