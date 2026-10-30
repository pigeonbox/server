package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/oekit/core/lifecycle"
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

	go func() {
		logger.Info("Server starting...")
		h.Spin()
	}()

	// 优雅停机（oekit core/lifecycle，与 p2pd / core Cleanup 同款）：
	// http 排水拿独立 15s 预算——此前 context.Background() 无界，一个挂死
	// 连接即卡死整个停机流程，只能靠容器运行时 SIGKILL 兜底；排完后 main
	// 返回，后台任务排水等核心清理经 defer bootstrap.Cleanup() 接力。
	mgr := lifecycle.New(20 * time.Second)
	mgr.Add("http", func(ctx context.Context) error { return h.Shutdown(ctx) }, 15*time.Second)
	mgr.Listen(syscall.SIGINT, syscall.SIGTERM)

	logger.Info("Server stopped")
}
