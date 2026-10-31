package main

// doctor 的**公开面降级检查**（2026-10-31 真机实录：fnOS 10.10.30.253）。
//
// # 缺陷（真机发现，不是推理）
//
// `pb doctor` 在**未设置 PB_API_KEY** 时给出：
//
//	[WARN] 深度检查  未执行（未设置 PB_API_KEY）
//	[FAIL] 服务可达  GET /api/config 失败：未设置 PB_API_KEY（用户中心 → API 令牌 创建）
//	结论：1 项故障、1 项需处置、0 项正常
//
// ⇒ 「服务可达」被判 **FAIL** ⇒ 结论是「**1 项故障**」⇒ 用户会以为**应用坏了**
//   （在 NAS 这类「刚装好、还没配 Key」的场景里极具误导性 ⇒ 用户会去重装应用，
//   而不是去建一个 API Key）。
//
// # 根因
//
// `doReq` 在 `apiKey == ""` 时**直接返回错误、根本不发请求**
// ⇒ doctor 的公开面检查（`/api/config` 这个**无需鉴权**的端点）
// 在无 Key 场景下**永远拿不到数据** ⇒ 「服务可达」恒 FAIL
// ⇒ **降级路径形同虚设**。
//
// # 修法
//
// ① 新增 `doPublicReq`：**匿名**通路（不带 X-API-Key，也不因缺 Key 而拒）
//    ⇒ 公开端点照常返回数据；
// ② 公开面检查改用它；
// ③ 「服务可达」在**端点返回未认证**时判 **warn**（服务其实是活的）
//    ⇒ 不再把「缺 Key」说成「服务故障」。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDoctor_PublicCheckWorksWithoutAPIKey —— 无 Key 时公开面检查**必须真的发出请求**。
//
// 判据打在「真正会被读到的量」上：
//   - 「服务可达」必须是 **ok**（而不是 fail）；
//   - 服务端**确实收到了** /api/config 请求（mock 计数）——
//     这条是本缺陷的判据核心：修复前该请求**根本没发出去**。
func TestDoctor_PublicCheckWorksWithoutAPIKey(t *testing.T) {
	var gotPublic int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/diagnostics":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"admin access required"}`))
		case "/api/config":
			gotPublic++
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"capabilities":{"deployment_mode":"standalone","public_url_set":true}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)

	oldServer, oldKey := serverURL, apiKey
	serverURL = srv.URL
	apiKey = "" // 关键：**不设** API Key
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	out, _ := captureDoctor(t, []string{})

	if gotPublic == 0 {
		t.Fatalf("公开面检查**没有发出请求**（修复前就是这个症状：doReq 因缺 Key 直接拒）\n输出:\n%s", out)
	}
	if strings.Contains(out, "[FAIL] 服务可达") {
		t.Errorf("无 API Key 时「服务可达」不得判 FAIL（缺 Key ≠ 服务故障）：\n%s", out)
	}
	if !strings.Contains(out, "[OK  ] 服务可达") {
		t.Errorf("无 API Key 时「服务可达」应判 OK（公开端点可达即服务活着）：\n%s", out)
	}
	// 深度检查仍然必须明确标注「未执行」（没检查 ≠ 检查通过）。
	if !strings.Contains(out, "深度检查") || !strings.Contains(out, "未执行") {
		t.Errorf("仍须明确标注深度检查未执行：\n%s", out)
	}
}

// TestDoctor_PublicCheckAuthErrorIsWarnNotFail —— 端点返回未认证时判 **warn**。
//
// 场景：某些部署下 /api/config 被网关/鉴权层拦住（返回 401 业务码）
// ⇒ 服务其实是**活的**（有响应）⇒ 判 fail 会再次误导。
func TestDoctor_PublicCheckAuthErrorIsWarnNotFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/diagnostics":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"admin access required"}`))
		case "/api/config":
			// 业务码 10002 = 未认证（core 的错误码）
			_, _ = w.Write([]byte(`{"code":10002,"message":"未认证（缺少 Bearer 头或会话 Cookie）"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)

	oldServer, oldKey := serverURL, apiKey
	serverURL = srv.URL
	apiKey = ""
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	out, _ := captureDoctor(t, []string{})

	if strings.Contains(out, "[FAIL] 服务可达") {
		t.Errorf("端点返回未认证时「服务可达」不得判 FAIL（服务有响应=活的）：\n%s", out)
	}
	if !strings.Contains(out, "[WARN] 服务可达") {
		t.Errorf("端点返回未认证时「服务可达」应判 WARN：\n%s", out)
	}
}
