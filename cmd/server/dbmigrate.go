package main

// dbmigrate.go —— 换库子命令（2026-10-31）：把实例数据从当前库搬到另一个库。
//
// # 形态
//
//	pigeonbox db export --out <file> [--config <path>]
//	pigeonbox db import --from <file> --target-driver <sqlite|postgres|mysql> \
//	                    [--target-dsn <dsn>] --yes
//
// # 与 backup 子命令的分工（别混淆）
//
//	· `backup export/restore` = **物理快照**：同库同机，DB 文件 + 存储对象
//	  ⇒ 换机器 / 灾备。
//	· `db export/import`       = **逻辑搬运**：跨方言（SQLite → PG/MySQL），
//	  只搬**数据库数据**，**不碰存储对象**（对象在文件系统/对象存储里，与库无关）。
//
// # 为什么换库**不需要停服**
//
// 换库是「读源库 → 写目标库」⇒ 源库只被**读**⇒ 运行中的实例不受影响。
// （对比 `backup restore`：它**替换**源库的 DB 文件 ⇒ 必须停服。）
// ⇒ 停机切换是**换库之后**的独立动作（改配置 + 重启），不在本命令范围内。
//
// # 安全语义
//
//	· 导入会写目标库 ⇒ **破坏性操作** ⇒ 必须显式 `--yes`（否则退出码 2）。
//	· 目标库**必须为空**（否则会把两套数据混在一起）⇒ 非空时拒绝，
//	  除非显式 `--allow-nonempty`。
//	· 导入后会**对齐自增序列**（否则下一次新建行撞主键）⇒ 见 core/app/dbmigrate。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/app/dbmigrate"
	"github.com/pigeonbox/core/bootstrap"
	"github.com/pigeonbox/core/conf"
	"github.com/pigeonbox/core/repo/db/model"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// errDBMigrateUsage 用法错误（退出码 2）。
var errDBMigrateUsage = errors.New("用法: pigeonbox db export --out <file> | pigeonbox db import --from <file> --target-driver <driver> --target-dsn <dsn> --yes")

// runDBMigrateCommand 换库子命令的命令行入口。
//
// 返回值：nil = 成功；用法错误 = errDBMigrateUsage（退出码 2）；其它 = 运行错误（退出码 1）。
func runDBMigrateCommand(args []string) error {
	var configPath, out, from string
	var targetDriver, targetDSN string
	confirm := false
	allowNonEmpty := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config":
			if i+1 >= len(args) {
				return fmt.Errorf("--config 缺少取值（%w）", errDBMigrateUsage)
			}
			configPath = args[i+1]
			i++
		case "--out":
			if i+1 >= len(args) {
				return fmt.Errorf("--out 缺少取值（%w）", errDBMigrateUsage)
			}
			out = args[i+1]
			i++
		case "--from":
			if i+1 >= len(args) {
				return fmt.Errorf("--from 缺少取值（%w）", errDBMigrateUsage)
			}
			from = args[i+1]
			i++
		case "--target-driver":
			if i+1 >= len(args) {
				return fmt.Errorf("--target-driver 缺少取值（%w）", errDBMigrateUsage)
			}
			targetDriver = args[i+1]
			i++
		case "--target-dsn":
			if i+1 >= len(args) {
				return fmt.Errorf("--target-dsn 缺少取值（%w）", errDBMigrateUsage)
			}
			targetDSN = args[i+1]
			i++
		case "--yes":
			confirm = true
		case "--allow-nonempty":
			allowNonEmpty = true
		default:
			return fmt.Errorf("未知参数: %s（%w）", args[i], errDBMigrateUsage)
		}
	}

	// **动作前校验**：必须且只能给一个动作（导出 --out / 导入 --from）。
	if (out == "") == (from == "") {
		return errDBMigrateUsage
	}
	// 导入必须给目标库信息。
	if from != "" && targetDriver == "" {
		return fmt.Errorf("导入必须指定 --target-driver（sqlite/postgres/mysql）（%w）", errDBMigrateUsage)
	}
	// 目标方言必须受支持（**在初始化之前**判：别为了报个参数错就去连库）。
	if from != "" {
		if err := validateTargetDriver(targetDriver); err != nil {
			return err
		}
	}
	// 导入必须显式确认（**破坏性操作**）——同样前置：没确认就不该连任何库。
	if from != "" && !confirm {
		return fmt.Errorf("%w（--yes 显式确认）", dbmigrate.ErrImportNeedsConfirm)
	}

	// 初始化源库（配置 → 全局配置 → 数据库）。
	cfg, err := bootstrap.InitConfig(configPath)
	if err != nil {
		return fmt.Errorf("初始化配置失败: %w（离线命令同样要过配置校验：把实例的 PB_JWT_SECRET 一并带上即可）", err)
	}
	conf.SetGlobalConfig(cfg)
	source, err := bootstrap.InitDatabase(&cfg.Database)
	if err != nil {
		return fmt.Errorf("初始化数据库失败: %w", err)
	}
	defer bootstrap.Cleanup()

	ctx := context.Background()

	switch {
	case out != "":
		// ── 导出：源库 → 行数据文件 ──
		dump, err := dbmigrate.NewExporter(source).Export(ctx)
		if err != nil {
			return fmt.Errorf("导出失败: %w", err)
		}
		f, err := os.Create(out)
		if err != nil {
			return fmt.Errorf("创建导出文件失败: %w", err)
		}
		defer func() { _ = f.Close() }()
		if err := dump.WriteJSON(f); err != nil {
			return fmt.Errorf("写出导出文件失败: %w", err)
		}
		fmt.Printf("数据库导出已写出: %s\n", out)
		fmt.Printf("  格式版本: %d  源库方言: %s  导出时间: %s\n",
			dump.DumpFormat, dump.SourceDriver, dump.CreatedAt)
		fmt.Printf("  表: %d 张  行: %d 条\n", len(dump.Tables), dump.TotalRows())
		return nil

	case from != "":
		// ── 导入：行数据文件 → 目标库 ──
		f, err := os.Open(from)
		if err != nil {
			return fmt.Errorf("打开导出文件失败: %w", err)
		}
		defer func() { _ = f.Close() }()
		dump, err := dbmigrate.ReadDump(f)
		if err != nil {
			return err
		}

		target, err := openTargetDB(targetDriver, targetDSN)
		if err != nil {
			return err
		}
		defer func() {
			if sqlDB, err := target.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}()

		if !allowNonEmpty {
			nonEmpty, err := targetHasData(target)
			if err != nil {
				return fmt.Errorf("检查目标库是否为空失败: %w", err)
			}
			if nonEmpty {
				return fmt.Errorf("目标库已有数据（拒绝导入：会把两套数据混在一起）⇒ 请换空库，或显式 --allow-nonempty")
			}
		}

		report, err := dbmigrate.NewImporter(target, true).Import(ctx, dump)
		if err != nil {
			return fmt.Errorf("导入失败: %w", err)
		}
		fmt.Printf("导入完成: %s（格式版本 %d）\n", from, dump.DumpFormat)
		fmt.Printf("  目标库方言: %s\n", target.Name())
		fmt.Printf("  已导入 %d 行 / %d 张表\n", report.TotalRows, len(report.Tables))
		fmt.Printf("  已对齐自增序列: %d 张表\n", len(report.SequencesAligned))
		fmt.Println("  ⚠ 切到新库还需：改配置里的 database.driver/dsn → 重启服务")
		return nil

	default:
		return errDBMigrateUsage
	}
}

// validateTargetDriver 校验目标库方言是否受支持。
//
// 单独成函数是为了让「参数校验」（在初始化之前）与「连接目标库」**共用同一份
// 方言清单** ⇒ 两处不会漂移（两处各写一遍 switch = 迟早一个支持一个不支持）。
func validateTargetDriver(driver string) error {
	switch normalizeDriver(driver) {
	case "sqlite", "postgres", "postgresql", "mysql":
		return nil
	default:
		return fmt.Errorf("不支持的目标库方言: %s（支持 sqlite / postgres / mysql）（%w）", driver, errDBMigrateUsage)
	}
}

// normalizeDriver 归一化方言名（小写 + 去空白）。
func normalizeDriver(driver string) string {
	return strings.ToLower(strings.TrimSpace(driver))
}

// openTargetDB 按方言与 DSN 打开目标库连接。
//
// DSN 形态（与 GORM 驱动一致）：
//
//	· sqlite   → 文件路径（":memory:" 也行）
//	· postgres → host=... port=... user=... password=... dbname=... sslmode=...
//	· mysql    → user:pass@tcp(host:port)/db?charset=utf8mb4&parseTime=True&loc=Local
func openTargetDB(driver, dsn string) (*gorm.DB, error) {
	var dialector gorm.Dialector
	switch normalizeDriver(driver) {
	case "sqlite":
		if dsn == "" {
			return nil, fmt.Errorf("--target-dsn 缺少 SQLite 文件路径（%w）", errDBMigrateUsage)
		}
		dialector = sqliteDialector(dsn)
	case "postgres", "postgresql":
		if dsn == "" {
			return nil, fmt.Errorf("--target-dsn 缺少 Postgres DSN（%w）", errDBMigrateUsage)
		}
		dialector = postgres.Open(dsn)
	case "mysql":
		if dsn == "" {
			return nil, fmt.Errorf("--target-dsn 缺少 MySQL DSN（%w）", errDBMigrateUsage)
		}
		dialector = mysql.Open(dsn)
	default:
		return nil, fmt.Errorf("不支持的目标库方言: %s（支持 sqlite / postgres / mysql）（%w）", driver, errDBMigrateUsage)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, fmt.Errorf("连接目标库失败: %w", err)
	}
	return db, nil
}

// targetHasData 判断目标库是否已有数据。
//
// 判据：**逐张领域表查行数**（不是「看有没有表」）⇒
// 「建了表但没数据」不算非空（那是刚 AutoMigrate 过的空库）。
func targetHasData(db *gorm.DB) (bool, error) {
	for _, spec := range modelTableNames() {
		if !db.Migrator().HasTable(spec) {
			continue // 表还不存在 ⇒ 目标库是全新的
		}
		var n int64
		if err := db.Table(spec).Unscoped().Count(&n).Error; err != nil {
			// 目标库还没建表时 Count 会报「表不存在」⇒ 视为空。
			continue
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// sqliteDialector SQLite 方言（独立成函数只为 import 集中在一处）。
func sqliteDialector(dsn string) gorm.Dialector {
	return sqlite.Open(dsn)
}

// modelTableNames 返回全部领域表名（来自 core 的模型注册表 ⇒ 单一真相源）。
func modelTableNames() []string {
	return model.TableNames()
}
