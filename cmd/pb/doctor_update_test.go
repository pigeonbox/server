package main

// 「新版本可用」检查项的断言（2026-11-06，5.0.0）。
//
// 这一项最容易做错的地方不是「能不能比对版本」，而是**会不会把
// 「上游有新版本」报成故障**：一旦报 FAIL/WARN，pb doctor 就返回退出码 1，
// CI 与巡检会因为「GitHub 上发了个新 tag」恒红，运维很快学会忽略整条报告。
//
// 所以这里的核心判据是：**任何更新状态都不得改变退出码**。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// diagWithUpdate 在健康快照上挂一段 update。
func diagWithUpdate(now time.Time, update string) string {
	base := strings.TrimSpace(healthyDiagnostics(now))
	trimmed := strings.TrimSpace(strings.TrimSuffix(base, "}"))
	return trimmed + ",\"update\":" + update + "}"
}

func TestDoctor_NewVersionReportsButExitsZero(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithUpdate(now, `{
		"current":"5.0.0","latest":"v5.1.0","available":true,"source":"github",
		"checked_at":"`+now.Format(time.RFC3339)+`","url":"https://example.test/releases/v5.1.0"
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("有新版本**不得**改变退出码（应 0）: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "v5.1.0") {
		t.Errorf("应写明最新版本：\n%s", out)
	}
	if !strings.Contains(out, "5.0.0") {
		t.Errorf("应写明当前版本：\n%s", out)
	}
	if !strings.Contains(out, "[OK") {
		t.Errorf("更新检查项状态应为 OK：\n%s", out)
	}
	if strings.Contains(out, "[FAIL") || strings.Contains(out, "[WARN") {
		t.Errorf("有更新不是故障，不得出现 FAIL/WARN：\n%s", out)
	}
}

func TestDoctor_UpToDate(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithUpdate(now, `{
		"current":"5.0.0","latest":"v5.0.0","available":false,"source":"github"
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("已是最新应退出码 0: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "已是最新") {
		t.Errorf("应显示「已是最新」：\n%s", out)
	}
}

// 离线 / 上游不可达 ⇒ unknown。这**不是故障**，与 server#88 同属
// 「把『不知道』报成『坏了』」的错误类型。
func TestDoctor_UpdateUnknownIsNotFailure(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithUpdate(now, `{
		"current":"5.0.0","available":false,"source":"unknown",
		"error":"请求上游失败: dial tcp: connection refused"
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("离线不是故障，退出码必须 0: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "未检查到") {
		t.Errorf("应明确说明未检查到：\n%s", out)
	}
	if strings.Contains(out, "[FAIL") {
		t.Errorf("unknown 不得报 FAIL：\n%s", out)
	}
}

func TestDoctor_UpdateDisabled(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithUpdate(now, `{"current":"5.0.0","source":"disabled"}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("关闭检查应退出码 0: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "未启用") {
		t.Errorf("应说明检查已关闭：\n%s", out)
	}
}

// 过期缓存：版本差可能不准 ⇒ 必须显式提示，但仍不是故障。
func TestDoctor_StaleUpdateIsFlagged(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithUpdate(now, `{
		"current":"5.0.0","latest":"v5.1.0","available":true,"source":"github",
		"stale":true,"error":"请求上游失败: timeout"
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("过期缓存不得改变退出码: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "过期缓存") {
		t.Errorf("应提示数据来自过期缓存：\n%s", out)
	}
}

// JSON 消费方要能直接读到 update 段（CI 巡检按字段判定，不解析人读文本）。
func TestDoctor_UpdateInJSON(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithUpdate(now, `{
		"current":"5.0.0","latest":"v5.1.0","available":true,"source":"github"
	}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("--json 有更新应退出码 0: %v\n输出:\n%s", err, out)
	}
	var report struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("JSON 输出不可解析: %v\n%s", err, out)
	}
	found := false
	for _, c := range report.Checks {
		if c.Name == "新版本" {
			found = true
			if c.Status != "ok" {
				t.Errorf("新版本检查项 JSON 状态应为 ok，实际 %q", c.Status)
			}
			if !strings.Contains(c.Detail, "v5.1.0") {
				t.Errorf("JSON detail 应含最新版本，实际 %q", c.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("JSON 输出缺少「新版本」检查项：\n%s", out)
	}
}
