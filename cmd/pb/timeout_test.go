package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// issue #128 —— pb doctor 深度检查不得共用 30 分钟上传超时。
//
// 复现：`PB_SERVER` 指向被防火墙 DROP 的黑洞地址 ⇒ `pb doctor` 卡满 30 分钟
// 才报错（doReqWithHeaders 的 30 分钟 client 被深度检查 GET /admin/diagnostics
// 共用），巡检脚本 / CI / 发车前检查全部挂起；而同一命令的降级检查走的是
// 30s client ⇒ 同一次自检里两次检查超时差 60 倍。
//
// 判据：
//   - 默认值钉住：查询类 30s、上传类 30 分钟（30 分钟只留上传路径）；
//   - 查询类真走短超时（黑洞端点秒级失败，而不是挂到超时上限）；
//   - 上传类仍走长超时（大文件上传不被改坏）。

func TestHTTPTimeoutDefaults(t *testing.T) {
	if queryTimeout != 30*time.Second {
		t.Fatalf("查询类默认超时应为 30s（doctor/ls/url 等不得吃上传长超时），实际 %v", queryTimeout)
	}
	if uploadTimeout != 30*time.Minute {
		t.Fatalf("上传类超时应保持 30 分钟，实际 %v", uploadTimeout)
	}
}

// blockServer 起一个「永不回应」的端点（黑洞），返回释放函数。
func blockServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	return srv, func() { close(release); srv.Close() }
}

// TestDoctorDeepCheckUsesShortTimeout —— 深度检查走查询超时：黑洞端点秒级失败。
func TestDoctorDeepCheckUsesShortTimeout(t *testing.T) {
	old := queryTimeout
	queryTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queryTimeout = old })

	srv, closeFn := blockServer(t)
	defer closeFn()
	useTestServer(t, srv)

	start := time.Now()
	_, err := fetchDiagnostics()
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("黑洞端点必须超时报错")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("深度检查耗时 %v —— 仍共用 30 分钟上传超时（issue #128）", elapsed)
	}
}

// TestDoReqUsesShortTimeout —— 其余查询类请求（ls/url/rm/p2p）同样短超时。
func TestDoReqUsesShortTimeout(t *testing.T) {
	old := queryTimeout
	queryTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queryTimeout = old })

	srv, closeFn := blockServer(t)
	defer closeFn()
	useTestServer(t, srv)

	start := time.Now()
	_, err := doReq("GET", "/api/v1/user/shares?page=1", nil, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("黑洞端点必须超时报错")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("查询类请求耗时 %v —— 仍共用长超时", elapsed)
	}
}

// TestUploadPathKeepsLongTimeout —— put 直传/多文件/存储迁移仍走上传长超时
// （默认改短，但不能把大文件上传改坏）。
func TestUploadPathKeepsLongTimeout(t *testing.T) {
	old := uploadTimeout
	uploadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { uploadTimeout = old })

	srv, closeFn := blockServer(t)
	defer closeFn()
	useTestServer(t, srv)

	start := time.Now()
	_, err := doUploadReq("POST", "/share/file/", strings.NewReader("blob"), "application/json")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("黑洞端点必须超时报错")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("上传路径耗时 %v —— 没走 uploadTimeout", elapsed)
	}
}

// TestImportUsesUploadTimeout —— pb import（服务端整文件 SaveStream 拷贝 + 全量
// 哈希，core 侧同步完成）不得吃查询类短超时：NAS 大文件本地导入可达数分钟，
// 被 30s 掐死是 issue #128 修复自身的回归面。用「上传短(200ms)、查询长(5s)」
// 的反向配置验证走向：若走了查询超时，本判据 5s 才返回 ⇒ 红。
func TestImportUsesUploadTimeout(t *testing.T) {
	oldQ, oldU := queryTimeout, uploadTimeout
	queryTimeout = 5 * time.Second
	uploadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { queryTimeout, uploadTimeout = oldQ, oldU })

	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
	}))
	defer func() { close(release); srv.Close() }()
	useTestServer(t, srv)

	start := time.Now()
	err := cmdImport([]string{"/nas/big.iso"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("黑洞端点必须超时报错")
	}
	if hits.Load() != 1 {
		t.Fatalf("必须真正发出 1 次导入请求（否则本判据假绿），实际 %d", hits.Load())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("本地导入耗时 %v —— 走了查询类超时而非 uploadTimeout（大文件导入会被掐死）", elapsed)
	}
}
