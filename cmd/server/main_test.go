package main

// issue #130 —— server 二进制的子命令可发现性与「拼错不静默起服」。
//
// 复现：
//   - `pigeonbox bakup export --out x`：标准库 flag 遇到第一个非旗标参数即停止
//     解析且不报错 ⇒ 整个位置参数被丢弃 ⇒ bootstrap.Bootstrap() **起整个服务**
//     （期望的备份没做、可能还跑了迁移）；
//   - `pigeonbox --help`：文件里从未设置 flag.Usage ⇒ 只打印 -config 一个旗标，
//     backup/restore/db 三个离线子命令没有任何可发现入口。
//
// 判据：
//   - validateStartupArgs：白名单外首参 ⇒ 用法错误（调用方 exit 2）；
//   - serverUsage：--help 必须列出全部离线子命令与示例；
//   - 子进程端到端：真实退出码 2 / 0（复现路径本身，不只测辅助函数）。

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestServerUsageListsOfflineCommands —— --help 必须披露 backup/restore/db。
func TestServerUsageListsOfflineCommands(t *testing.T) {
	var buf strings.Builder
	serverUsage(&buf)
	got := buf.String()
	for _, want := range []string{"backup", "restore", "db", "-config"} {
		if !strings.Contains(got, want) {
			t.Fatalf("--help 用法必须包含 %q（issue #130：三个离线子命令此前不可发现）；实际输出：\n%s", want, got)
		}
	}
}

// TestValidateStartupArgs —— 白名单校验（拼错/错位子命令不得静默起服）。
func TestValidateStartupArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"无位置参数（正常起服）", nil, false},
		{"拼错子命令", []string{"bakup"}, true},
		{"已知子命令写在旗标之后", []string{"backup"}, true},
		{"已知子命令 db 错位", []string{"db"}, true},
		{"未知首参", []string{"serve"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStartupArgs(tc.args)
			if tc.wantErr && err == nil {
				t.Fatalf("args=%q 必须判为用法错误（否则静默起服）", tc.args)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("args=%q 不应报错: %v", tc.args, err)
			}
		})
	}
}

// serverMainHelperEnv 子进程复现用的环境变量开关。
const serverMainHelperEnv = "PB_SERVER_MAIN_HELPER_ARG"

// TestServerMainHelper 仅由 runServerMainArg 以子进程调起：替换 os.Args 后
// 直接跑 main()，从而拿到**真实**退出码与用法输出（主测试进程里会 Skip）。
func TestServerMainHelper(t *testing.T) {
	arg, ok := os.LookupEnv(serverMainHelperEnv)
	if !ok {
		t.Skip("helper 模式专用（由 runServerMainArg 以子进程调起）")
	}
	os.Args = []string{"pigeonbox", arg}
	main()
	os.Exit(0) // main() 不会走到这里；走到说明路径没按预期 exit
}

// runServerMainArg 以子进程跑 main()，返回 (合并输出, 退出码)。
func runServerMainArg(t *testing.T, arg string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestServerMainHelper$")
	cmd.Env = append(os.Environ(), serverMainHelperEnv+"="+arg)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("子进程执行失败: %v", err)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// TestServerMainRejectsTypoSubcommand —— 拼错子命令必须 exit 2 且不进 bootstrap。
func TestServerMainRejectsTypoSubcommand(t *testing.T) {
	out, code := runServerMainArg(t, "bakup")
	if code != 2 {
		t.Fatalf("拼错子命令必须退出码 2（用法错），实际 %d；输出：\n%s", code, out)
	}
	if !strings.Contains(out, "bakup") {
		t.Fatalf("必须点名报出拼错的子命令；输出：\n%s", out)
	}
	for _, want := range []string{"backup", "restore", "db"} {
		if !strings.Contains(out, want) {
			t.Fatalf("必须同时给出子命令用法（含 %q）；输出：\n%s", want, out)
		}
	}
	if strings.Contains(out, "Server starting") {
		t.Fatalf("不得起服（issue #130：拼错子命令静默起服）；输出：\n%s", out)
	}
}

// TestServerMainHelpListsOfflineCommands —— --help 披露子命令且退出码 0。
func TestServerMainHelpListsOfflineCommands(t *testing.T) {
	out, code := runServerMainArg(t, "--help")
	if code != 0 {
		t.Fatalf("--help 应退出 0，实际 %d；输出：\n%s", code, out)
	}
	for _, want := range []string{"backup", "restore", "db"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--help 必须列出子命令 %q；输出：\n%s", want, out)
		}
	}
}
