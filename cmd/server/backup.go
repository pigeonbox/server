// backup.go —— 离线备份 / 恢复子命令（2026-10-31）。
//
// # 形态
//
//	pigeonbox backup export --out <file> [--config <path>]
//	pigeonbox backup restore --from <file> --yes [--config <path>] [--dry-run]
//	pigeonbox restore --from <file> --yes        （别名）
//
// # 为什么是「server 二进制的离线子命令」而不是 `pb` 子命令
//
//	· `pb` 是**纯 HTTP 客户端**（刻意不链接 core）⇒ 让 `pb backup` 能落地就得把
//	  core 拖进这个轻量 CLI（体积 + 数十 MB，且把「同机文件操作」搬上网络）。
//	· 导出/恢复天然是**同机**操作（要读本地 DB 与本地存储）⇒ 做成 HTTP 端点会让
//	  「服务端正在运行 ⇒ 恢复砸自己的脚」成为可能。
//	· 恢复**必须停服**（DB 文件被替换时，运行中的实例会读到半旧半新的数据）
//	  ⇒ 走独立进程 + 显式 `--yes`，语义更干净。
//
// # 安全语义
//
//	· 恢复是**破坏性操作**（替换 DB + 重写对象）⇒ **必须显式 --yes**，
//	  否则拒绝执行（退出码 2）；--dry-run 只校验不落盘。
//	· 备份包内的条目名逐个过**路径穿越**校验；每个对象逐个校验 sha256；
//	  不认识的格式版本直接拒绝 ⇒ 来路不明的包不会往机器上写任意文件。
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/pigeonbox/core/app/backup"
	"github.com/pigeonbox/core/bootstrap"
	"github.com/pigeonbox/core/conf"
)

// offlineUsage 用法错误（退出码 2）。
var errOfflineUsage = errors.New("用法: pigeonbox backup export --out <file> | pigeonbox backup restore --from <file> --yes")

// runOfflineCommand 离线备份/恢复的命令行入口。
//
// 返回值：nil = 成功；用法错误 = offlineUsage（退出码 2）；其它 = 运行错误（退出码 1）。
func runOfflineCommand(mode string, args []string) error {
	var configPath, out, from string
	confirm := false
	dryRun := false

	rest := args
	if mode == "backup" {
		if len(rest) == 0 {
			return errOfflineUsage
		}
		switch rest[0] {
		case "export", "restore":
			rest = rest[1:]
		default:
			return fmt.Errorf("未知 backup 子命令: %s（%w）", rest[0], errOfflineUsage)
		}
	}

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--config":
			if i+1 >= len(rest) {
				return fmt.Errorf("--config 缺少取值（%w）", errOfflineUsage)
			}
			configPath = rest[i+1]
			i++
		case "--out":
			if i+1 >= len(rest) {
				return fmt.Errorf("--out 缺少取值（%w）", errOfflineUsage)
			}
			out = rest[i+1]
			i++
		case "--from":
			if i+1 >= len(rest) {
				return fmt.Errorf("--from 缺少取值（%w）", errOfflineUsage)
			}
			from = rest[i+1]
			i++
		case "--yes":
			confirm = true
		case "--dry-run":
			dryRun = true
		default:
			return fmt.Errorf("未知参数: %s（%w）", rest[i], errOfflineUsage)
		}
	}

	// **动作前校验**（与 pb 的旗标守卫同纪律）：必须且只能给一个动作
	// （导出 --out / 恢复 --from）⇒ 缺值或两个都给 ⇒ 用法错误，
	// 且**不进入初始化**（别为了报个用法错就去连 DB、起存储单例）。
	if (out == "") == (from == "") {
		return errOfflineUsage
	}

	// 初始化：配置 → 全局配置 → 数据库 → 备份服务（与 bootstrap 同款顺序）。
	cfg, err := bootstrap.InitConfig(configPath)
	if err != nil {
		return fmt.Errorf("初始化配置失败: %w", err)
	}
	conf.SetGlobalConfig(cfg)
	database, err := bootstrap.InitDatabase(&cfg.Database)
	if err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}
	defer bootstrap.Cleanup()

	svc, err := bootstrap.NewOfflineBackupService(cfg, database)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch {
	case out != "":
		// 导出
		f, err := os.Create(out)
		if err != nil {
			return fmt.Errorf("创建备份文件失败: %w", err)
		}
		defer func() { _ = f.Close() }()
		manifest, err := svc.Export(ctx, f)
		if err != nil {
			return fmt.Errorf("导出备份包失败: %w", err)
		}
		fmt.Printf("备份包已写出: %s\n", out)
		fmt.Printf("  格式版本: %d  导出时间: %s\n", manifest.BackupFormat, manifest.CreatedAt)
		fmt.Printf("  对象: %d 个 / %d 字节  分享记录: %d 条  存储后端: %s\n",
			manifest.ObjectCount, manifest.ObjectBytes, manifest.FileCount, manifest.StorageType)
		return nil

	case from != "":
		// 恢复
		if !dryRun {
			if err := ensureServiceStopped(cfg); err != nil {
				return err
			}
		}
		f, err := os.Open(from)
		if err != nil {
			return fmt.Errorf("打开备份包失败: %w", err)
		}
		defer func() { _ = f.Close() }()
		opts := backupRestoreOptions(cfg, confirm, dryRun)
		res, err := svc.Restore(ctx, f, opts)
		if err != nil {
			return fmt.Errorf("恢复失败: %w", err)
		}
		fmt.Printf("备份包: %s（格式版本 %d，导出时间 %s）\n", from, res.Manifest.BackupFormat, res.Manifest.CreatedAt)
		if dryRun {
			fmt.Printf("  [dry-run] 校验通过，未落盘；将恢复对象 %d 个 / %d 字节\n",
				res.Manifest.ObjectCount, res.Manifest.ObjectBytes)
		} else {
			fmt.Printf("  已恢复对象 %d 个 / %d 字节  数据库已替换: %v\n",
				res.ObjectsRestored, res.BytesRestored, res.DBRestored)
		}
		if len(res.MissingObjects) > 0 {
			fmt.Printf("  ⚠ %d 个对象在 DB 快照里有引用但包里没有（导出期间新上传？）: %v\n",
				len(res.MissingObjects), res.MissingObjects)
		}
		return nil

	default:
		return errOfflineUsage
	}
}

// backupRestoreOptions 组装恢复选项（目标 DB 路径取自配置）。
//
// 只支持 SQLite：非 SQLite 的**一致性快照**不在本格式内（core 侧会明确报错），
// 且换库（SQLite → PG/MySQL）是另一条专门的流程（规划稿 §4.1）。
func backupRestoreOptions(cfg *conf.AppConfiguration, confirm, dryRun bool) backup.RestoreOptions {
	opts := backup.RestoreOptions{Confirm: confirm, DryRun: dryRun}
	if cfg.Database.Driver == "sqlite" && cfg.Database.DBName != "" && cfg.Database.DBName != ":memory:" {
		opts.DBPath = cfg.Database.DBName
	}
	return opts
}

// ensureServiceStopped 恢复前检查：本地端口还在监听 ⇒ 服务在跑 ⇒ 拒绝恢复。
//
// # 为什么要有
//
// 恢复会**替换** DB 文件 ⇒ 运行中的实例会读到「半旧半新」的数据（DB 文件被换、
// 但已打开的连接还指着旧 inode）⇒ 表现为随机的「记录不见了/取件码失效」。
// ⇒ 机械拦住，而不是靠人记得先停服。
//
// 注：docker compose 场景下恢复跑在**另一个容器**里 ⇒ 端口探测看不到主容器
// ⇒ 那条路径靠提示（"请确保已停服"）兜底。
func ensureServiceStopped(cfg *conf.AppConfiguration) error {
	port := cfg.Server.Port
	if port <= 0 {
		return nil
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 800*time.Millisecond)
	if err != nil {
		return nil // 没人监听 ⇒ 端口没被占用（或探测不到）⇒ 放行
	}
	_ = conn.Close()
	return fmt.Errorf("检测到本机 %d 端口仍在监听（服务可能在跑）⇒ 恢复会替换数据库文件，请**先停服**再恢复", port)
}
