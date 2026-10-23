package main

import (
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI 上传请求体守卫（2026-10-24 真机实测）。
//
// 实测事故：`pb put -t "..."`（**不带 -p**）必失败——
//   错误: [code 10001] 'require_auth' field is a 'required' parameter,
//   but the request body does not have this parameter 'require_auth'
//
// 根因：`require_auth` 是**契约必填**（契约层无论 true/false 都要显式发送），
// 而 CLI 的四条上传路径都写成「有密码才发」⇒ 公开分享（无密码）一律被拒。
// 前端早已修过同一类（api/share.ts 注释：「契约必填: false 也必须显式发送
// (条件 append 曾致公开分享 400)」），CLI 漏了 ⇒ 官方 CLI 的主命令整个不可用，
// 而冒烟里没有 CLI 的任何断言 ⇒ 长期无人发现。
//
// 本守卫用 httptest 捕获真实请求体，断言 require_auth 恒在（无密码时为 "false"）。

// captureForm 起一个捕获 multipart 表单的测试服务器，返回 (表单键值, 关闭函数)。
func captureForm(t *testing.T) (map[string]string, func()) {
	t.Helper()
	fields := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
			return
		}
		for k, vs := range r.MultipartForm.Value {
			if len(vs) > 0 {
				fields[k] = vs[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"code":"AbCd1234","pickup_code":"X1X2X3","url":"http://x/#/share/AbCd1234"}}`))
	}))
	return fields, srv.Close
}

func useTestServer(t *testing.T, srv *httptest.Server) {
	t.Helper()
	oldServer, oldKey := serverURL, apiKey
	serverURL = srv.URL
	apiKey = "pb_sk_test"
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })
}

// TestPutTextRequiresRequireAuth —— 文本分享：无密码也必须显式发 require_auth=false。
func TestPutTextRequiresRequireAuth(t *testing.T) {
	fields, closeFn := captureForm(t)
	defer closeFn()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("解析 multipart 失败: %v", err)
			return
		}
		for k, vs := range r.MultipartForm.Value {
			if len(vs) > 0 {
				fields[k] = vs[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"code":"AbCd1234","url":"http://x/#/share/AbCd1234"}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	if err := cmdPut([]string{"-t", "无密码的文本分享"}); err != nil {
		t.Fatalf("cmdPut 应成功: %v", err)
	}
	v, ok := fields["require_auth"]
	if !ok {
		t.Fatalf("请求体缺少 require_auth（契约必填）⇒ 后端会拒成 10001；实际字段=%v", fields)
	}
	if v != "false" {
		t.Fatalf("无密码时 require_auth 应为 false，实际 %q", v)
	}
}

// TestPutTextWithPasswordRequireAuth —— 带密码：require_auth=true。
func TestPutTextWithPasswordRequireAuth(t *testing.T) {
	fields := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		for k, vs := range r.MultipartForm.Value {
			if len(vs) > 0 {
				fields[k] = vs[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"code":"AbCd1234","url":"http://x/#/share/AbCd1234"}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	if err := cmdPut([]string{"-t", "带密码的文本分享", "-p", "pw123"}); err != nil {
		t.Fatalf("cmdPut 应成功: %v", err)
	}
	if fields["require_auth"] != "true" {
		t.Fatalf("带密码时 require_auth 应为 true，实际 %q", fields["require_auth"])
	}
	if fields["password"] != "pw123" {
		t.Fatalf("password 字段应透传，实际 %q", fields["password"])
	}
}

// TestPutFileRequiresRequireAuth —— 单文件上传（multipart）：同样必填。
func TestPutFileRequiresRequireAuth(t *testing.T) {
	fields := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		for k, vs := range r.MultipartForm.Value {
			if len(vs) > 0 {
				fields[k] = vs[0]
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"code":"AbCd1234","url":"http://x/#/share/AbCd1234"}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdPut([]string{p}); err != nil {
		t.Fatalf("cmdPut 应成功: %v", err)
	}
	if fields["require_auth"] != "false" {
		t.Fatalf("无密码上传必须显式发 require_auth=false，实际 %q（缺字段 ⇒ 后端 10001）", fields["require_auth"])
	}
}

// TestImportRequiresRequireAuth —— 服务器本地文件导入（JSON 请求体）：同样必填。
func TestImportRequiresRequireAuth(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"code":"AbCd1234","url":"http://x/#/share/AbCd1234"}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	dir := t.TempDir()
	p := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cmdImport([]string{p}); err != nil {
		t.Fatalf("cmdImport 应成功: %v", err)
	}
	if payload == nil {
		t.Fatal("未捕获到请求体")
	}
	v, ok := payload["require_auth"]
	if !ok {
		t.Fatalf("导入请求体缺少 require_auth（契约必填）⇒ 实际=%v", payload)
	}
	if v != false {
		t.Fatalf("无密码导入时 require_auth 应为 false，实际 %v", v)
	}
}

// TestMultipartFormParsingGuard —— 守一下 helper 本身（别让守卫写错却绿）。
func TestMultipartFormParsingGuard(t *testing.T) {
	var buf strings.Builder
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("require_auth", "false")
	_ = w.Close()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", w.FormDataContentType())
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := r.FormValue("require_auth"); got != "false" {
		t.Fatalf("守卫解析异常: %q", got)
	}
}

// TestRemoveUsesSoftDelete —— `pb rm` 必须走**软删**（batch-delete），
// 不能直连硬删端点。
//
// 实测事故：CLI 原先打 DELETE /api/v1/user/shares/:code/hard，而该端点**只接受
// 已软删的分享** ⇒ 对一个正常分享永远报 10001「分享不在回收站中」⇒ 删除命令
// 形同虚设（用户只能改用网页端）。2026-10-24 实测修复。
func TestRemoveUsesSoftDelete(t *testing.T) {
	var gotPath, gotMethod string
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"count":1}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	if err := cmdDelete([]string{"AbCd1234"}); err != nil {
		t.Fatalf("cmdDelete 应成功: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/user/shares/batch-delete" {
		t.Fatalf("rm 应走软删端点 POST /api/v1/user/shares/batch-delete，实际 %s %s（直连硬删端点只接受已软删的分享 ⇒ 正常分享永远删不掉）", gotMethod, gotPath)
	}
	codes, _ := payload["codes"].([]any)
	if len(codes) != 1 || codes[0] != "AbCd1234" {
		t.Fatalf("软删请求体应带 codes=[AbCd1234]，实际 %v", payload["codes"])
	}
}
