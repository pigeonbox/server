package main

// pb doctor 的「部署模式与被降级的能力」检查项（pigeonbox/server#69 验收 ⑧）。
//
// # 验收判据（issue 原文）
//
//	「⑧ 部署模式与被降级的能力（直传 / 联邦是否因多副本被关）」。
//
// # 为什么这条值得单独测
//
// 被降级的能力是**用户从日志里看不出来**的一类事实：
//
//   - 多副本拓扑（public×N + admin×1）下，core 的 deployment.go 会把联邦
//     **强制关闭**（节点身份是进程级的 Ed25519，多副本语义未定义）⇒
//     用户以为联邦能用，实际一直是关的；
//   - 存储远端后端构造失败时，会静默降级到 local ⇒ 用户以为在用对象存储，
//     实际文件全落在本地盘上。
//
// ⇒ doctor 必须替用户把这两件事说出来，而且**分级要不同**：
// 存储降级是**真故障**（fail），多副本关联邦是**架构事实**（warn）。
//
// # 数据来源
//
// /admin/diagnostics 的 capabilities 段（服务端按运行时真实状态算出的生效值）。

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// capabilitiesDiagnostics 一份带 capabilities 段的诊断快照。
func capabilitiesDiagnostics(now time.Time, caps string) string {
	return `{
      "version":"v4.2.0",
      "deployment_mode":"standalone",
      "server_time":"` + now.Format(time.RFC3339) + `",
      "database":{"driver":"sqlite","reachable":true},
      "storage":{"backend":"local","configured":"local","degraded":false,"data_path":"/data"},
      "redis":{"mode":"memory"},
      "site":{"public_url":"https://box.example.com","public_url_set":true},
      "setup":{"initialized":true,"token_required":false,"token_source":"none"},
      "capabilities":` + caps + `
    }`
}

// findCheck 按名字找一条检查结论。
func findCheck(t *testing.T, checks []doctorCheck, name string) doctorCheck {
	t.Helper()
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("未找到检查项 %q，实际有：%v", name, names(checks))
	return doctorCheck{}
}

func names(checks []doctorCheck) []string {
	out := make([]string, 0, len(checks))
	for _, c := range checks {
		out = append(out, c.Name)
	}
	return out
}

// TestDoctor_DegradedCapability_MultiReplicaDisablesFederation 多副本 ⇒ 联邦被强制关闭。
func TestDoctor_DegradedCapability_MultiReplicaDisablesFederation(t *testing.T) {
	payload := capabilitiesDiagnostics(time.Now(),
		`{"deployment_mode":"admin","public_url_set":true,"federation":false,"storage_backend":"local","storage_degraded":false}`)
	srv := doctorServer(t, payload, `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("多副本下联邦被关 ⇒ 退出码不得为 0：\n%s", out)
	}
	if !strings.Contains(out, "被降级的能力") {
		t.Fatalf("输出应含「被降级的能力」检查项：\n%s", out)
	}
	if !strings.Contains(out, "多副本") {
		t.Errorf("输出应说明是多副本导致：\n%s", out)
	}
	if !strings.Contains(out, "[WARN]") {
		t.Errorf("多副本关联邦应是 WARN（架构事实，不是故障）：\n%s", out)
	}
	if strings.Contains(out, "[FAIL]") {
		t.Errorf("多副本关联邦不应报 FAIL（那是存储降级的事）：\n%s", out)
	}
}

// TestDoctor_DegradedCapability_StorageDegradedIsFailure 存储降级 ⇒ fail（真故障）。
func TestDoctor_DegradedCapability_StorageDegradedIsFailure(t *testing.T) {
	payload := `{
      "version":"v4.2.0",
      "deployment_mode":"standalone",
      "server_time":"` + time.Now().Format(time.RFC3339) + `",
      "database":{"driver":"sqlite","reachable":true},
      "storage":{"backend":"local","configured":"s3","degraded":true,"init_error":"s3 unreachable"},
      "redis":{"mode":"memory"},
      "site":{"public_url":"https://box.example.com","public_url_set":true},
      "setup":{"initialized":true},
      "capabilities":{"deployment_mode":"standalone","federation":true,"storage_backend":"local","storage_degraded":true}
    }`
	srv := doctorServer(t, payload, `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("存储降级必须报错（退出码非 0）：\n%s", out)
	}
	var de *doctorExitError
	if !errors.As(err, &de) {
		t.Fatalf("错误应为 doctorExitError：%v", err)
	}
	if de.report.Summary.Fail == 0 {
		t.Errorf("存储降级必须计入 FAIL 计数：\n%s", out)
	}
	if !strings.Contains(out, "存储已降级") {
		t.Errorf("输出应说明存储已降级：\n%s", out)
	}
}

// TestDoctor_DegradedCapability_FederationEnabledIsOK 单站 + 联邦启用 ⇒ 不告警。
func TestDoctor_DegradedCapability_FederationEnabledIsOK(t *testing.T) {
	payload := capabilitiesDiagnostics(time.Now(),
		`{"deployment_mode":"standalone","public_url_set":true,"federation":true,"storage_backend":"local","storage_degraded":false}`)
	srv := doctorServer(t, payload, `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("单站 + 联邦启用 ⇒ 不得报错：%v\n%s", err, out)
	}
	if strings.Contains(out, "[WARN]") || strings.Contains(out, "[FAIL]") {
		t.Errorf("单站 + 联邦启用不应有 WARN/FAIL：\n%s", out)
	}
}

// TestDoctor_DegradedCapability_MissingCapabilitiesIsUnknown 老后端不下发 capabilities 段
// ⇒ 属于「未知」，**不得误报**（否则每次跑 doctor 都出一条假告警）。
func TestDoctor_DegradedCapability_MissingCapabilitiesIsUnknown(t *testing.T) {
	// healthyDiagnostics 刻意不含 capabilities 段
	srv := doctorServer(t, healthyDiagnostics(time.Now()), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("缺 capabilities 段（未知）不得报错：%v\n%s", err, out)
	}
	if strings.Contains(out, "[WARN]") || strings.Contains(out, "[FAIL]") {
		t.Errorf("缺 capabilities 段属未知，不应误报 WARN/FAIL：\n%s", out)
	}
}

// TestDoctor_DegradedCapability_UnitCheck 单元级：检查项的分级口径。
func TestDoctor_DegradedCapability_UnitCheck(t *testing.T) {
	cases := []struct {
		name   string
		p      *doctorPayload
		status string
	}{
		{
			name: "单站 + 联邦启用 ⇒ ok",
			p: &doctorPayload{
				Capabilities: doctorCapabilities{DeploymentMode: "standalone", Federation: true},
			},
			status: "ok",
		},
		{
			name: "单站 + 联邦未启用 ⇒ ok（配置层面的正常选择，不是问题）",
			p: &doctorPayload{
				Capabilities: doctorCapabilities{DeploymentMode: "standalone", Federation: false},
			},
			status: "ok",
		},
		{
			name: "多副本 ⇒ warn（架构事实）",
			p: &doctorPayload{
				Deployment: "admin",
				Capabilities: doctorCapabilities{
					DeploymentMode: "admin", Federation: false,
				},
			},
			status: "warn",
		},
		{
			name: "存储降级 ⇒ fail（真故障，优先于多副本判断）",
			p: &doctorPayload{
				Deployment: "admin",
				Storage: struct {
					Backend    string `json:"backend"`
					Configured string `json:"configured"`
					Degraded   bool   `json:"degraded"`
					DataPath   string `json:"data_path"`
					InitError  string `json:"init_error"`
				}{Configured: "s3", Backend: "local", Degraded: true},
				Capabilities: doctorCapabilities{
					DeploymentMode: "admin", StorageDegraded: true, StorageBackend: "local",
				},
			},
			status: "fail",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := degradedCapabilityCheck(tc.p)
			if c.Status != tc.status {
				t.Errorf("状态应为 %q，实际 %q（detail=%s）", tc.status, c.Status, c.Detail)
			}
			if c.Name != "被降级的能力" {
				t.Errorf("检查项名应为「被降级的能力」，实际 %q", c.Name)
			}
		})
	}
}
