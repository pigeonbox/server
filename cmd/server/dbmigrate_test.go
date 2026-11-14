package main

// 换库子命令的用法守卫（2026-10-31）。
//
// ## 为什么要有
//
// 换库会**写目标库** ⇒ 参数错必须**在初始化之前**就拒绝：
//
//	· `--out` 与 `--from` 必须**恰好给一个**（两个都给 = 意图不明 ⇒ 拒；
//	  都不给 = 无动作 ⇒ 拒）；
//	· 导入必须给 `--target-driver`（不知道往哪导 ⇒ 拒）；
//	· 未知参数 ⇒ 拒（而不是静默忽略 ⇒ 拼错 `--target-dsn` 会让人以为导到别处了）。
//
// ⇒ 一律**用法错误**（退出码 2），且**不进入初始化流程**
//（别为了报个用法错就去连 DB、起存储单例）。

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/pigeonbox/core/app/dbmigrate"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDBMigrateCommand_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"无子命令", nil},
		{"未知子命令", []string{"exports"}},
		{"同时给 --out 与 --from", []string{"export", "--out", "/tmp/x.json", "--from", "/tmp/y.json"}},
		{"--out 缺值", []string{"export", "--out"}},
		{"--from 缺值", []string{"import", "--from"}},
		{"--config 缺值", []string{"export", "--out", "/tmp/x.json", "--config"}},
		{"--target-driver 缺值", []string{"import", "--from", "/tmp/x.json", "--target-driver"}},
		{"--target-dsn 缺值", []string{"import", "--from", "/tmp/x.json", "--target-driver", "postgres", "--target-dsn"}},
		{"未知参数", []string{"export", "--out", "/tmp/x.json", "--wat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runDBMigrateCommand(tc.args)
			if err == nil {
				t.Fatalf("期望用法错误，实际 nil")
			}
			if !errors.Is(err, errDBMigrateUsage) {
				t.Fatalf("期望用法错误（errDBMigrateUsage），实际: %v", err)
			}
		})
	}
}

// TestDBMigrateCommand_ImportNeedsConfirm —— 导入必须显式确认。
//
// ⚠️ 这条在**参数解析之后、连库之前**判 ⇒ 不会误伤数据。
func TestDBMigrateCommand_ImportNeedsConfirm(t *testing.T) {
	err := runDBMigrateCommand([]string{
		"import",
		"--from", "/tmp/does-not-exist.json",
		"--target-driver", "postgres",
		"--target-dsn", "host=127.0.0.1 port=1 user=x password=y dbname=z sslmode=disable",
	})
	if err == nil {
		t.Fatalf("期望确认错误，实际 nil")
	}
	if !errors.Is(err, dbmigrate.ErrImportNeedsConfirm) {
		t.Fatalf("期望 ErrImportNeedsConfirm，实际: %v", err)
	}
}

// TestDBMigrateCommand_UnsupportedTargetDriver —— 不支持的目标方言必须拒。
func TestDBMigrateCommand_UnsupportedTargetDriver(t *testing.T) {
	err := runDBMigrateCommand([]string{
		"import",
		"--from", "/tmp/does-not-exist.json",
		"--target-driver", "oracle",
		"--target-dsn", "whatever",
		"--yes",
	})
	if err == nil {
		t.Fatalf("期望错误，实际 nil")
	}
	if !errors.Is(err, errDBMigrateUsage) {
		t.Fatalf("期望用法错误（errDBMigrateUsage），实际: %v", err)
	}
}

// ===== issue #132：targetHasData 的 fail-closed =====
//
// 复现：HasTable 已为 true 的前提下 Count 报错（SELECT 权限被拒 / 视图 /
// 驱动异常）——原实现把**任意**错误当「表还没建」continue ⇒ 该表被跳过 ⇒
// 其余表皆空时判「空库」放行 `pigeonbox db import --yes` ⇒ 与目标库既有数据
// 混库（该命令头注释明令「目标库必须为空」）。
//
// 注入方式：gorm 的 HasTable 走 Row 回调、Count 走 Query 回调 ⇒ 只在 Query
// 回调上对目标表注入错误，即可复现「表在、但查不动行数」的组合（真库的天然
// 形态造不出来——SQLite 没有 SELECT 拒权）。

// openTargetTestDB 起一个临时 SQLite 目标库（守卫必须在真实 GORM 通路上验）。
func openTargetTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "target.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("打开临时目标库失败: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("取底层连接失败: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// firstModelTable 取一张领域表名（判据对任意领域表一致，不写死表名）。
func firstModelTable(t *testing.T) string {
	t.Helper()
	names := modelTableNames()
	if len(names) == 0 {
		t.Fatal("modelTableNames() 为空——注册表口径变了，测试夹具要跟上")
	}
	return names[0]
}

func TestTargetHasData_FreshLibraryIsEmpty(t *testing.T) {
	db := openTargetTestDB(t)
	got, err := targetHasData(db)
	if err != nil {
		t.Fatalf("全新库（无表）不应报错: %v", err)
	}
	if got {
		t.Fatalf("全新库应判空，实际 got=%v", got)
	}
}

func TestTargetHasData_TableWithoutRowsIsEmpty(t *testing.T) {
	db := openTargetTestDB(t)
	table := firstModelTable(t)
	if err := db.Exec("CREATE TABLE \"" + table + "\" (id INTEGER)").Error; err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	got, err := targetHasData(db)
	if err != nil {
		t.Fatalf("建了表但没数据：不应报错: %v", err)
	}
	if got {
		t.Fatalf("空表应判空，实际 got=%v", got)
	}
}

func TestTargetHasData_RowsMeanNonEmpty(t *testing.T) {
	db := openTargetTestDB(t)
	table := firstModelTable(t)
	if err := db.Exec("CREATE TABLE \"" + table + "\" (id INTEGER)").Error; err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Exec("INSERT INTO \"" + table + "\" (id) VALUES (1)").Error; err != nil {
		t.Fatalf("插行失败: %v", err)
	}
	got, err := targetHasData(db)
	if err != nil {
		t.Fatalf("有数据时不应报错: %v", err)
	}
	if !got {
		t.Fatalf("有数据必须判非空（否则放行混库导入），实际 got=%v", got)
	}
}

// TestTargetHasData_CountErrorFailsClosed —— issue #132 的红→绿判据。
func TestTargetHasData_CountErrorFailsClosed(t *testing.T) {
	db := openTargetTestDB(t)
	table := firstModelTable(t)
	if err := db.Exec("CREATE TABLE \"" + table + "\" (id INTEGER)").Error; err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 注入故障：只让 Count（Query 回调）对该表失败；HasTable 走 Row 回调不受影响。
	if err := db.Callback().Query().Before("gorm:query").Register("test:count_fail", func(tx *gorm.DB) {
		if tx.Statement.Table == table {
			tx.AddError(errors.New("模拟 SELECT 被拒"))
		}
	}); err != nil {
		t.Fatalf("注册回调失败: %v", err)
	}

	got, err := targetHasData(db)
	if err == nil {
		t.Fatalf("Count 失败必须 fail-closed 报错中止（旧实现把故障当空库 ⇒ 放行混库导入），实际 got=%v err=nil", got)
	}
	if got {
		t.Fatalf("出错时不得同时报「有数据」，got=%v", got)
	}
}
