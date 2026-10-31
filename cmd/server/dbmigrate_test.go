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
	"testing"

	"github.com/pigeonbox/core/app/dbmigrate"
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
