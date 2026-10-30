package main

// pb doctor 部署自检命令的断言（2026-10-30）。
//
// 守卫点：
//  1. **全绿 ⇒ 退出码 0**（健康）；
//  2. **存储降级 / DB 不可达 ⇒ 报 FAIL**（真故障，不能被当成"检查通过"）；
//  3. **拿不到管理面数据 ⇒ 降级为公开面检查 + 明确标注未做深度检查**，
//     且退出码为 1（没检查 ≠ 检查通过）；
//  4. `--json` 输出机器可读结论（CI/巡检消费）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// doctorServer 起一个假的 PigeonBox 服务端。
// diagnostics 为 nil 时 /admin/diagnostics 返回 401（模拟无 admin 权限）。
func doctorServer(t *testing.T, diagnostics string, publicCfg string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/diagnostics":
			if diagnostics == "" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":401,"message":"admin access required"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":` + diagnostics + `}`))
		case "/api/config":
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":` + publicCfg + `}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// healthyDiagnostics 一份"一切正常"的诊断快照。
func healthyDiagnostics(now time.Time) string {
	return `{
      "version":"v4.2.0",
      "deployment_mode":"standalone",
      "server_time":"` + now.Format(time.RFC3339) + `",
      "database":{"driver":"sqlite","reachable":true},
      "storage":{"backend":"local","configured":"local","degraded":false,"data_path":"/data"},
      "redis":{"mode":"memory"},
      "site":{"public_url":"https://box.example.com","public_url_set":true},
      "setup":{"initialized":true,"token_required":false,"token_source":"none"}
    }`
}

// captureDoctor 跑一次 doctor，捕获 stdout，返回 (输出, 错误)。
func captureDoctor(t *testing.T, args []string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &buf, &buf
	defer func() { stdout, stderr = oldOut, oldErr }()
	err := cmdDoctor(args)
	return buf.String(), err
}

func TestDoctor_HealthyServerExitsZero(t *testing.T) {
	srv := doctorServer(t, healthyDiagnostics(time.Now()), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("全绿时不得报错（退出码应 0）: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "[OK") {
		t.Errorf("输出应含 OK 行：\n%s", out)
	}
	if strings.Contains(out, "[FAIL") {
		t.Errorf("全绿时不得出现 FAIL：\n%s", out)
	}
	if !strings.Contains(out, "全部正常") {
		t.Errorf("结论行应为「全部正常」：\n%s", out)
	}
}

func TestDoctor_StorageDegradedIsFailure(t *testing.T) {
	// 配的是 s3，实际生效 local ⇒ 用户以为在用对象存储、实际全落本地盘
	payload := `{
      "version":"v4.2.0",
      "deployment_mode":"standalone",
      "server_time":"` + time.Now().Format(time.RFC3339) + `",
      "database":{"driver":"sqlite","reachable":true},
      "storage":{"backend":"local","configured":"s3","degraded":true,"init_error":"s3 endpoint unreachable"},
      "redis":{"mode":"memory"},
      "site":{"public_url":"https://box.example.com","public_url_set":true},
      "setup":{"initialized":true}
    }`
	srv := doctorServer(t, payload, `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("存储降级必须报错（退出码非 0）：\n%s", out)
	}
	var de *doctorExitError
	if !errors.As(err, &de) {
		t.Fatalf("应返回 doctorExitError，实际 %T: %v", err, err)
	}
	if de.report.Summary.Fail == 0 {
		t.Errorf("存储降级必须计为 FAIL：\n%s", out)
	}
	if !strings.Contains(out, "[FAIL") || !strings.Contains(out, "已降级") {
		t.Errorf("输出应明确标出存储降级：\n%s", out)
	}
}

func TestDoctor_DatabaseUnreachableIsFailure(t *testing.T) {
	payload := `{
      "version":"v4.2.0",
      "deployment_mode":"standalone",
      "server_time":"` + time.Now().Format(time.RFC3339) + `",
      "database":{"driver":"sqlite","reachable":false,"error":"disk I/O error"},
      "storage":{"backend":"local","configured":"local","degraded":false},
      "redis":{"mode":"memory"},
      "site":{"public_url":"https://box.example.com","public_url_set":true},
      "setup":{"initialized":true}
    }`
	srv := doctorServer(t, payload, `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("DB 不可达必须报错：\n%s", out)
	}
	if !strings.Contains(out, "[FAIL") || !strings.Contains(out, "disk I/O error") {
		t.Errorf("输出应含 DB 故障与原因：\n%s", out)
	}
}

func TestDoctor_NoAdminKeyFallsBackAndStillFails(t *testing.T) {
	// /admin/diagnostics 返回 401 ⇒ 降级为公开面检查
	srv := doctorServer(t, "", `{"capabilities":{"deployment_mode":"standalone","public_url_set":true}}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("深度检查未执行时退出码不得为 0（没检查 ≠ 检查通过）：\n%s", out)
	}
	if !strings.Contains(out, "深度检查") {
		t.Errorf("输出应说明深度检查未执行：\n%s", out)
	}
	if !strings.Contains(out, "[WARN") {
		t.Errorf("深度检查缺失应标为 WARN：\n%s", out)
	}
}

func TestDoctor_NoAPIKeyAtAllStillReports(t *testing.T) {
	srv := doctorServer(t, "", `{"capabilities":{"deployment_mode":"standalone","public_url_set":false}}`)
	oldServer, oldKey := serverURL, apiKey
	serverURL = srv.URL
	apiKey = ""
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("未设置 API Key 时退出码不得为 0：\n%s", out)
	}
	if !strings.Contains(out, "未设置 PB_API_KEY") {
		t.Errorf("输出应说明未设置 API Key：\n%s", out)
	}
}

func TestDoctor_JSONOutputIsMachineReadable(t *testing.T) {
	srv := doctorServer(t, healthyDiagnostics(time.Now()), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{"--json"})
	if err != nil {
		t.Fatalf("全绿时不得报错: %v\n%s", err, out)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("--json 输出必须可解析: %v\n%s", err, out)
	}
	if !report.Deep {
		t.Errorf("拿到诊断数据时 deep 应为 true")
	}
	if report.Summary.Total == 0 || report.Summary.OK == 0 {
		t.Errorf("summary 统计不对: %+v", report.Summary)
	}
}

func TestDoctor_UnknownFlagIsUsageError(t *testing.T) {
	_, err := captureDoctor(t, []string{"--bogus"})
	var ue *usageErr
	if !errors.As(err, &ue) {
		t.Fatalf("未知参数必须是用法错误，实际 %T: %v", err, err)
	}
}
