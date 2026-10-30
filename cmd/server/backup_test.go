package main

import (
	"errors"
	"testing"
)

// 离线子命令的用法守卫（2026-10-31）。
//
// ## 为什么要有
//
// 备份/恢复是**破坏性**操作（恢复会替换数据库文件并重写对象）：
//
//	· `--from` 缺值 ⇒ 若被静默吞掉，程序会拿空路径去恢复 ⇒ 要么莫名失败，
//	  要么（更糟）恢复到了**错的包**；
//	· 未显式 `--yes` ⇒ 绝不能执行（core 侧也会再挡一层 ErrRestoreNeedsConfirm）。
//
// ⇒ 缺值/未知参数一律**用法错误**（退出码 2），且**不得**进入初始化流程。
func TestOfflineCommand_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		mode string
		args []string
	}{
		{"backup 缺子命令", "backup", nil},
		{"backup 未知子命令", "backup", []string{"exports"}},
		{"export 缺 --out 值", "backup", []string{"export", "--out"}},
		{"export 缺 --config 值", "backup", []string{"export", "--out", "/tmp/x.tar.gz", "--config"}},
		{"restore 缺 --from 值", "backup", []string{"restore", "--from"}},
		{"未知参数", "backup", []string{"export", "--out", "/tmp/x.tar.gz", "--wat"}},
		{"restore 模式缺 --from", "restore", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runOfflineCommand(tc.mode, tc.args)
			if err == nil {
				t.Fatalf("期望用法错误，实际 nil")
			}
			if !errors.Is(err, errOfflineUsage) {
				t.Fatalf("期望用法错误（errOfflineUsage），实际: %v", err)
			}
		})
	}
}
