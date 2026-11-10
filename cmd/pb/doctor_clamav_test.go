package main

// 「病毒扫描」检查项的断言（core#121）。
//
// 核心判据有两条，缺一不可：
//  1. **静默失效必须可见**：clamd 不可达时必须出 [WARN] 行（issue 的核心——
//     fail-open 是对的，但没信号不对）；
//  2. **退出码不变**：扫描器挂掉时业务照常放行（fail-open），部署本身没坏
//     ⇒ 与「新版本」项同口径，CI/巡检不得因此恒红。
//
// 负对照锚点：把 clamavCheck 的 Advisory 改回 false ⇒ 用例三/四的
// 「退出码必须 0」判据转红（warn ⇒ doctorExitError）。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// diagWithModeration 在健康快照上挂一段 moderation。
func diagWithModeration(now time.Time, moderation string) string {
	base := strings.TrimSpace(healthyDiagnostics(now))
	trimmed := strings.TrimSpace(strings.TrimSuffix(base, "}"))
	return trimmed + ",\"moderation\":" + moderation + "}"
}

// doctorReportOf 解析 --json 输出为 doctorReport。
func doctorReportOf(t *testing.T, out string) doctorReport {
	t.Helper()
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("JSON 输出不可解析: %v\n%s", err, out)
	}
	return report
}

// 未启用 ⇒ ok（配置层面的正常选择），退出码 0。
func TestDoctor_ClamAVDisabledIsOK(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithModeration(now, `{"enabled":false,"clamav":{"enabled":false}}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("未启用不得改变退出码（应 0）: %v\n%s", err, out)
	}
	r := doctorReportOf(t, out)
	c := findCheck(t, r.Checks, "病毒扫描")
	if c.Status != "ok" {
		t.Errorf("未启用时状态应为 ok，实际 %q（detail=%q）", c.Status, c.Detail)
	}
}

// 全好 ⇒ ok，退出码 0。
func TestDoctor_ClamAVHealthyIsOK(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithModeration(now, `{
		"enabled":true,"clamav":{"enabled":true,"addr":"localhost:3310","degraded":false,
		"degraded_count":0,"window_size":100,"scans_in_window":5,
		"last_ping":{"ok":true,"at":"`+now.Format(time.RFC3339)+`","duration_ms":2}}
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("扫描器存活应退出码 0: %v\n%s", err, out)
	}
	r := doctorReportOf(t, out)
	c := findCheck(t, r.Checks, "病毒扫描")
	if c.Status != "ok" {
		t.Errorf("存活时状态应为 ok，实际 %q（detail=%q）", c.Status, c.Detail)
	}
}

// clamd 不可达 ⇒ **WARN 行可见 + 退出码 0**（两条判据缺一不可）。
func TestDoctor_ClamAVDegradedWarnsButExitsZero(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithModeration(now, `{
		"enabled":true,"clamav":{"enabled":true,"addr":"localhost:3310","degraded":true,
		"degraded_count":2,"window_size":100,"scans_in_window":3,
		"last_ping":{"ok":false,"at":"`+now.Format(time.RFC3339)+`","duration_ms":15,"error":"dial tcp 127.0.0.1:3310: connect: connection refused"}}
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	// 核心判据（负对照锚点：Advisory 改 false ⇒ warn ⇒ 此处转红）
	if err != nil {
		t.Fatalf("扫描器不可用是 fail-open 提示级，**不得**改变退出码（应 0）: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "[WARN") {
		t.Errorf("静默失效必须可见：输出应含 [WARN] 行：\n%s", out)
	}
	if !strings.Contains(out, "放行") {
		t.Errorf("应写明当前是 fail-open 放行：\n%s", out)
	}

	// JSON 消费面（巡检）：状态 warn + advisory 标记，且不计入 warn 汇总
	r := doctorReportOf(t, mustJSONOutput(t))
	c := findCheck(t, r.Checks, "病毒扫描")
	if c.Status != "warn" {
		t.Errorf("degraded 时 JSON 状态应为 warn，实际 %q", c.Status)
	}
	if !c.Advisory {
		t.Errorf("必须带 advisory 标记（WARN 级但不参与退出码）")
	}
	if r.Summary.Warn != 0 || r.Summary.Note == 0 {
		t.Errorf("advisory 应计入 note 而非 warn：summary=%+v", r.Summary)
	}
}

// 探活正常但近 N 次扫描有 fail-open 放行 ⇒ warn + advisory（期间文件没被扫）。
func TestDoctor_ClamAVFailOpenWindowWarns(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithModeration(now, `{
		"enabled":true,"clamav":{"enabled":true,"addr":"localhost:3310","degraded":false,
		"degraded_count":3,"window_size":100,"scans_in_window":10,
		"last_ping":{"ok":true,"at":"`+now.Format(time.RFC3339)+`","duration_ms":2}}
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("fail-open 窗口计数是提示级，退出码应 0: %v\n%s", err, out)
	}
	r := doctorReportOf(t, out)
	c := findCheck(t, r.Checks, "病毒扫描")
	if c.Status != "warn" || !c.Advisory {
		t.Errorf("应为 warn + advisory，实际 status=%q advisory=%v（detail=%q）", c.Status, c.Advisory, c.Detail)
	}
}

// 服务端未上报这段（旧版本）⇒ skip（未取到 ≠ 未启用），退出码 0。
func TestDoctor_ClamAVMissingSectionIsSkip(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, healthyDiagnostics(now), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("旧服务端无 moderation 段不得改变退出码: %v\n%s", err, out)
	}
	r := doctorReportOf(t, out)
	c := findCheck(t, r.Checks, "病毒扫描")
	if c.Status != "skip" {
		t.Errorf("未取到时状态应为 skip（未取到 ≠ 未启用），实际 %q（detail=%q）", c.Status, c.Detail)
	}
}

// mustJSONOutput 再跑一次 doctor --json（人读那次已消费）。
func mustJSONOutput(t *testing.T) string {
	t.Helper()
	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("--json 不得改变退出码: %v\n%s", err, out)
	}
	return out
}
