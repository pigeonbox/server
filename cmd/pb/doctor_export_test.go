package main

// 诊断包导出（hub#385）单测：信封结构 + 旗标解析。
//
// 断言是结构事实：文件可解析、schema/字段存在、缺值即用法错误。

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDoctorExportEnvelope(t *testing.T) {
	srv := doctorServer(t, healthyDiagnostics(time.Now()), `{}`)
	useTestServer(t, srv)
	path := filepath.Join(t.TempDir(), "pb-diagnostics.json")

	out, err := captureDoctor(t, []string{"--export", path})
	if err != nil {
		t.Fatalf("导出不应报错（健康服务端）: %v\n%s", err, out)
	}
	if !strings.Contains(out, "诊断包已导出") {
		t.Fatalf("应提示导出路径:\n%s", out)
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("诊断包未落盘: %v", rerr)
	}
	var env doctorExport
	if uerr := json.Unmarshal(raw, &env); uerr != nil {
		t.Fatalf("诊断包必须可解析: %v\n%s", uerr, raw)
	}
	if env.Schema != doctorExportSchema {
		t.Fatalf("schema = %q", env.Schema)
	}
	if env.ExportedAt.IsZero() {
		t.Fatalf("exported_at 缺失")
	}
	if env.Report.Server == "" || env.Report.Summary.Total == 0 {
		t.Fatalf("report 段不完整: %+v", env.Report)
	}
	if env.Diagnostics == nil || env.Diagnostics.Version == "" {
		t.Fatalf("diagnostics 段缺失或版本为空: %+v", env.Diagnostics)
	}
	// 诊断包是用户主动导出的支持材料：0600（只有本用户可读）。
	info, serr := os.Stat(path)
	if serr != nil {
		t.Fatalf("stat: %v", serr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("诊断包权限应为 0600, got %o", info.Mode().Perm())
	}
}

func TestDoctorExportFlagParsing(t *testing.T) {
	f, err := parseDoctorFlags([]string{"--export", "out.json"})
	if err != nil || f.exportPath != "out.json" {
		t.Fatalf("--export 取值解析失败: %+v %v", f, err)
	}
	f, err = parseDoctorFlags([]string{"--export=out.json"})
	if err != nil || f.exportPath != "out.json" {
		t.Fatalf("--export= 解析失败: %+v %v", f, err)
	}
	if _, err := parseDoctorFlags([]string{"--export"}); err == nil {
		t.Fatal("--export 缺值必须是用法错误")
	} else {
		var ue *usageErr
		if !errors.As(err, &ue) {
			t.Fatalf("应为 usageErr, got %T", err)
		}
	}
	if _, err := parseDoctorFlags([]string{"--export="}); err == nil {
		t.Fatal("--export= 空值必须是用法错误")
	}
}
