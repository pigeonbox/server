package main

// 端点与凭据解析的守卫（issue #117）。
//
// # 缺陷（真机实录，不是推理）
//
// 2026-11-10，10.10.10.3：后端 12345 端口**不发布**（对外 TLS 56789 /
// 回环明文 56790，TLS 由 pigeonbox-tls 终结并透传 X-Forwarded-Proto）。
// 而 `pb doctor` 的端点**只能来自环境变量**：
//
//	serverURL = strings.TrimRight(envOr("PB_SERVER", "http://localhost:12345"), "/")
//
// ⇒ 裸跑 doctor 必然打到一个没人监听的地址 ⇒ 结论恒为「服务不可达」
// ⇒ **假红** ⇒ 运维误判成「服务坏了」（实际只是「地址/密钥没告诉 doctor」）。
//
// # 本守卫覆盖
//
//   ① 给定 .env ⇒ doctor 走该端点（且输出标注来源：哪个文件、哪个键）；
//   ② 无任何配置 ⇒ 报「未配置端点」而不是「端点不可达」（三态里的 na）；
//   ③ 显式参数优先于 .env；
//   ④ 敏感值不出现在 argv 与输出里（明文密钥不进命令行）。
//
// # 三态口径（issue #117 的核心）
//
//   - 端点已配置且探到了 ⇒ ok
//   - 端点已配置但连不上 ⇒ **fail**（配错了/服务没起 ⇒ 真问题）
//   - 端点**未配置**且默认地址也连不上 ⇒ **na**（无法判定，不是故障）
//
// ⇒ 「没配地址」与「端点不可达」必须是两件事，否则每次裸跑都假红。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeEnv 写一份 .env 到临时目录，返回文件路径。
func writeEnv(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// useEnvCandidates 把 .env 候选集钉到给定的文件（单测不能依赖真机上
// /opt/pigeonbox/.env 的存在与否 ⇒ 必须能替换候选集）。
func useEnvCandidates(t *testing.T, paths ...string) {
	t.Helper()
	old := dotenvCandidatesFn
	dotenvCandidatesFn = func() []string { return paths }
	t.Cleanup(func() { dotenvCandidatesFn = old })
}

// clearEndpointEnv 清掉环境变量里的端点/凭据（单测的起点必须是「干净」的）。
func clearEndpointEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PB_SERVER", "")
	t.Setenv("PB_API_KEY", "")
	t.Setenv("PIGEONBOX_DIR", "")
}

// TestDotEnvParsing —— .env 解析的基本口径。
func TestDotEnvParsing(t *testing.T) {
	m := parseDotEnv("# 注释行\n" +
		"\n" +
		"PB_API_PORT=56789\n" +
		"export PB_API_KEY=\"pb_sk_quoted\"\n" +
		"PB_SERVER='http://box.example.com:56789'\n" +
		"EMPTY=\n" +
		"WITH_EQUALS=a=b\n")
	if m["PB_API_PORT"] != "56789" {
		t.Fatalf("PB_API_PORT 解析错: %q", m["PB_API_PORT"])
	}
	if m["PB_API_KEY"] != "pb_sk_quoted" {
		t.Fatalf("引号应被去掉: %q", m["PB_API_KEY"])
	}
	if m["PB_SERVER"] != "http://box.example.com:56789" {
		t.Fatalf("单引号应被去掉: %q", m["PB_SERVER"])
	}
	if v, ok := m["EMPTY"]; !ok || v != "" {
		t.Fatalf("空值键应存在且为空: %q ok=%v", v, ok)
	}
	if m["WITH_EQUALS"] != "a=b" {
		t.Fatalf("值里含 '=' 时应只切第一个: %q", m["WITH_EQUALS"])
	}
}

// TestResolveEndpointFromEnvFile —— ① 给定 .env ⇒ 走该端点。
//
// 判据：解析出的端点 = .env 里的端口，且**来源标注**里带文件名与键名。
func TestResolveEndpointFromEnvFile(t *testing.T) {
	clearEndpointEnv(t)
	path := writeEnv(t, "PB_API_PORT=56789\nPB_API_KEY=pb_sk_from_env\n")
	useEnvCandidates(t, path)

	ep := resolveEndpoint()
	if ep.URL != "http://127.0.0.1:56789" {
		t.Fatalf("端点应取自 .env 的 PB_API_PORT，实际 %q", ep.URL)
	}
	if !strings.Contains(ep.URLSource, path) || !strings.Contains(ep.URLSource, "PB_API_PORT") {
		t.Fatalf("来源标注必须含文件名与键名，实际 %q", ep.URLSource)
	}
	if ep.APIKey != "pb_sk_from_env" {
		t.Fatalf("API Key 应取自 .env，实际 %q", ep.APIKey)
	}
	if !strings.Contains(ep.APIKeySource, "PB_API_KEY") {
		t.Fatalf("凭据来源标注必须含键名，实际 %q", ep.APIKeySource)
	}
	if !endpointConfigured(ep) {
		t.Fatalf("读到 .env ⇒ 端点应算已配置")
	}
}

// TestResolveEndpointEnvFileServerWins —— .env 里的 PB_SERVER 优先于 PB_API_PORT。
func TestResolveEndpointEnvFileServerWins(t *testing.T) {
	clearEndpointEnv(t)
	path := writeEnv(t, "PB_API_PORT=56789\nPB_SERVER=http://box.example.com:12345\n")
	useEnvCandidates(t, path)

	ep := resolveEndpoint()
	if ep.URL != "http://box.example.com:12345" {
		t.Fatalf("PB_SERVER 应优先于 PB_API_PORT，实际 %q", ep.URL)
	}
}

// TestResolveEndpointEnvFileBindAddress —— PB_API_BIND=0.0.0.0 时改用 127.0.0.1
// （0.0.0.0 不是可连接的地址）。
func TestResolveEndpointEnvFileBindAddress(t *testing.T) {
	clearEndpointEnv(t)
	path := writeEnv(t, "PB_API_PORT=56789\nPB_API_BIND=0.0.0.0\n")
	useEnvCandidates(t, path)

	ep := resolveEndpoint()
	if ep.URL != "http://127.0.0.1:56789" {
		t.Fatalf("0.0.0.0 应换成 127.0.0.1，实际 %q", ep.URL)
	}
}

// TestResolveEndpointNoConfigIsNotConfigured —— ② 无任何配置 ⇒ 「未配置」，
// 不是「已配置但不可达」。
func TestResolveEndpointNoConfigIsNotConfigured(t *testing.T) {
	clearEndpointEnv(t)
	useEnvCandidates(t)

	ep := resolveEndpoint()
	if endpointConfigured(ep) {
		t.Fatalf("无任何配置时端点不应算已配置（source=%q url=%q）", ep.URLSource, ep.URL)
	}
	if ep.URLSource != "" {
		t.Fatalf("未配置时来源应为空，实际 %q", ep.URLSource)
	}
}

// TestReachabilityStatusThreeStates —— 三态判据本身（na ≠ fail）。
func TestReachabilityStatusThreeStates(t *testing.T) {
	cases := []struct {
		name                  string
		configured, reachable bool
		want                  string
	}{
		{"已配置且可达", true, true, "ok"},
		{"未配置但默认地址可达", false, true, "ok"},
		{"已配置但不可达", true, false, "fail"},
		{"未配置且不可达", false, false, "na"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reachabilityStatus(tc.configured, tc.reachable); got != tc.want {
				t.Fatalf("三态判定错: got %q want %q", got, tc.want)
			}
		})
	}
}

// TestDoctorNoConfigReportsNotConfiguredInsteadOfUnreachable —— ② 的端到端口径。
//
// 判据（打在真正会被读到的量上）：
//   - 输出**不得**出现 [FAIL] 服务可达；
//   - 必须出现「未配置端点」；
//   - 结论里不能出现「项故障」。
func TestDoctorNoConfigReportsNotConfiguredInsteadOfUnreachable(t *testing.T) {
	clearEndpointEnv(t)
	useEnvCandidates(t)

	// 默认地址指向一个确定没人监听的端口 ⇒ 探不通。
	oldDefault := defaultServerURL
	defaultServerURL = "http://127.0.0.1:1"
	t.Cleanup(func() { defaultServerURL = oldDefault })

	oldURL, oldSrc := serverURL, serverURLSource
	oldKey, oldKeySrc := apiKey, apiKeySource
	t.Cleanup(func() {
		serverURL, serverURLSource = oldURL, oldSrc
		apiKey, apiKeySource = oldKey, oldKeySrc
	})
	serverURL, serverURLSource = defaultServerURL, ""
	apiKey, apiKeySource = "", ""

	out, _ := captureDoctor(t, []string{})

	if strings.Contains(out, "[FAIL] 服务可达") {
		t.Fatalf("未配置端点不得判 FAIL（没配地址 ≠ 服务坏了）：\n%s", out)
	}
	if !strings.Contains(out, "未配置端点") {
		t.Fatalf("应报「未配置端点」：\n%s", out)
	}
	if !strings.Contains(out, "[NA") {
		t.Fatalf("应出现独立的 NA 状态标签（三态）：\n%s", out)
	}
	if strings.Contains(out, "项故障") {
		t.Fatalf("未配置端点不应计为故障：\n%s", out)
	}
}

// TestDoctorConfiguredButUnreachableIsFail —— 三态的另一半：配了却连不上 ⇒ fail
// （这是真问题，必须报故障）。
func TestDoctorConfiguredButUnreachableIsFail(t *testing.T) {
	clearEndpointEnv(t)
	useEnvCandidates(t)

	oldURL, oldSrc := serverURL, serverURLSource
	oldKey, oldKeySrc := apiKey, apiKeySource
	t.Cleanup(func() {
		serverURL, serverURLSource = oldURL, oldSrc
		apiKey, apiKeySource = oldKey, oldKeySrc
	})
	// 端点**已配置**（来源非空）但地址没人监听。
	serverURL, serverURLSource = "http://127.0.0.1:1", "命令行 --server"
	apiKey, apiKeySource = "", ""

	out, _ := captureDoctor(t, []string{})

	if !strings.Contains(out, "[FAIL] 服务可达") {
		t.Fatalf("已配置但连不上 ⇒ 应判 FAIL：\n%s", out)
	}
	if strings.Contains(out, "未配置端点") {
		t.Fatalf("已配置时不得说「未配置端点」：\n%s", out)
	}
}

// TestDoctorServerFlagOverridesEnvFile —— ③ 显式参数优先于 .env。
//
// 判据：请求必须打到 --server 指的地址，**不是** .env 里的那个。
func TestDoctorServerFlagOverridesEnvFile(t *testing.T) {
	clearEndpointEnv(t)

	var envHits, flagHits int
	envSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	t.Cleanup(envSrv.Close)
	flagSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flagHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	t.Cleanup(flagSrv.Close)

	path := writeEnv(t, "PB_SERVER="+envSrv.URL+"\n")
	useEnvCandidates(t, path)

	oldURL, oldSrc := serverURL, serverURLSource
	oldKey, oldKeySrc := apiKey, apiKeySource
	t.Cleanup(func() {
		serverURL, serverURLSource = oldURL, oldSrc
		apiKey, apiKeySource = oldKey, oldKeySrc
	})
	// 先按 .env 解析（模拟 main() 的启动解析）。
	applyEndpoint(resolveEndpoint())

	out, _ := captureDoctor(t, []string{"--server", flagSrv.URL})

	if flagHits == 0 {
		t.Fatalf("--server 指定的端点必须被使用：\n%s", out)
	}
	if envHits != 0 {
		t.Fatalf("--server 优先于 .env：.env 的端点仍被打到 %d 次", envHits)
	}
	if !strings.Contains(out, "命令行 --server") {
		t.Fatalf("输出应标注端点来源为命令行 --server：\n%s", out)
	}
}

// TestDoctorEnvFileEndpointIsUsed —— ① 的端到端口径：doctor 走 .env 里的端点。
func TestDoctorEnvFileEndpointIsUsed(t *testing.T) {
	clearEndpointEnv(t)

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/config":
			_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{"capabilities":{"deployment_mode":"standalone","public_url_set":true}}}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"admin access required"}`))
		}
	}))
	t.Cleanup(srv.Close)

	path := writeEnv(t, "PB_SERVER="+srv.URL+"\nPB_API_KEY=pb_sk_from_env\n")
	useEnvCandidates(t, path)

	oldURL, oldSrc := serverURL, serverURLSource
	oldKey, oldKeySrc := apiKey, apiKeySource
	t.Cleanup(func() {
		serverURL, serverURLSource = oldURL, oldSrc
		apiKey, apiKeySource = oldKey, oldKeySrc
	})
	applyEndpoint(resolveEndpoint())

	out, _ := captureDoctor(t, []string{})

	if hits == 0 {
		t.Fatalf("doctor 必须请求 .env 指定的端点：\n%s", out)
	}
	if !strings.Contains(out, path) {
		t.Fatalf("输出应标注端点来源文件（%s）：\n%s", path, out)
	}
	if !strings.Contains(out, "PB_SERVER") {
		t.Fatalf("输出应标注端点来源键名：\n%s", out)
	}
}

// TestDoctorAPIKeyFileNeverLeaksSecret —— ④ 敏感值不出现在 argv 与输出里。
//
// 判据：
//   - argv 里只出现**路径**，不出现密钥；
//   - 输出（stdout+stderr）里不出现密钥明文；
//   - 服务端确实收到了文件里的密钥（证明功能有效，不是靠不读文件蒙混）。
func TestDoctorAPIKeyFileNeverLeaksSecret(t *testing.T) {
	clearEndpointEnv(t)
	useEnvCandidates(t)

	const secret = "pb_sk_super_secret_value"
	keyPath := filepath.Join(t.TempDir(), "api_key")
	if err := os.WriteFile(keyPath, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	t.Cleanup(srv.Close)

	oldURL, oldSrc := serverURL, serverURLSource
	oldKey, oldKeySrc := apiKey, apiKeySource
	t.Cleanup(func() {
		serverURL, serverURLSource = oldURL, oldSrc
		apiKey, apiKeySource = oldKey, oldKeySrc
	})
	serverURL, serverURLSource = srv.URL, "命令行 --server"
	apiKey, apiKeySource = "", ""

	// argv 里只放路径。
	argv := []string{"--api-key-file", keyPath}
	for _, a := range argv {
		if strings.Contains(a, secret) {
			t.Fatalf("argv 里不得出现密钥明文: %q", a)
		}
	}

	out, _ := captureDoctor(t, argv)

	if strings.Contains(out, secret) {
		t.Fatalf("输出里不得出现密钥明文：\n%s", out)
	}
	if gotKey != secret {
		t.Fatalf("服务端应收到文件里的密钥，实际 %q", gotKey)
	}
	if !strings.Contains(out, keyPath) {
		t.Fatalf("输出应标注凭据来源（文件路径）：\n%s", out)
	}
}

// TestDoctorRejectsPlaintextAPIKeyFlag —— 明文密钥旗标必须被拒（用法错误）。
//
// 理由与 flag_guard_test.go 一致：命令行参数会进 ps 与 shell 历史。
func TestDoctorRejectsPlaintextAPIKeyFlag(t *testing.T) {
	for _, args := range [][]string{
		{"--api-key", "pb_sk_plaintext"},
		{"--api-key=pb_sk_plaintext"},
	} {
		_, err := captureDoctor(t, args)
		if err == nil {
			t.Fatalf("明文密钥旗标必须被拒: %v", args)
		}
		if !isUsageErr(err) {
			t.Fatalf("应为用法错误（退出码 2），实际 %T: %v", err, err)
		}
		if strings.Contains(err.Error(), "pb_sk_plaintext") {
			t.Fatalf("错误文案里不得回显密钥明文: %v", err)
		}
	}
}

// TestDoctorFlagsMissingValueIsUsageError —— 旗标缺值 ⇒ 用法错误（退出码 2）。
func TestDoctorFlagsMissingValueIsUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"--server"},
		{"--api-key-file"},
	} {
		_, err := captureDoctor(t, args)
		if err == nil {
			t.Fatalf("旗标缺值必须报错: %v", args)
		}
		if !isUsageErr(err) {
			t.Fatalf("缺值应是用法错误，实际 %T: %v", err, err)
		}
	}
}

// TestDoctorEmptyFlagValueIsUsageError —— 空取值必须报错（静默回落成默认地址
// ⇒ 又变成「打错地方」，正是 issue #117 要消灭的那种假红）。
func TestDoctorEmptyFlagValueIsUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"--server", ""},
		{"--server="},
		{"--api-key-file", ""},
		{"--api-key-file="},
	} {
		_, err := captureDoctor(t, args)
		if err == nil {
			t.Fatalf("空取值必须报错: %v", args)
		}
		if !isUsageErr(err) {
			t.Fatalf("应是用法错误，实际 %T: %v", err, err)
		}
	}
}

// TestSelectEndpointSkipsNonAPIPort —— 候选探测必须跳过「有响应但不是 PigeonBox
// API」的端点（真机 10.10.10.3 的 TLS 端口明文打过去是 nginx 的 400 HTML 提示页）。
//
// ⚠️ 若不跳过，会得到「端点可达、但后续全 400」的**假绿**（issue #117）。
func TestSelectEndpointSkipsNonAPIPort(t *testing.T) {
	// 假端点：回 HTML（模拟 nginx 的明文→HTTPS 提示页），不是 JSON 信封。
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("<html>400 The plain HTTP request was sent to HTTPS port</html>"))
	}))
	t.Cleanup(fake.Close)

	// 真端点：回 PigeonBox 的 JSON 信封。
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	t.Cleanup(real.Close)

	chosen, err := selectEndpoint([]endpointCandidate{
		{URL: fake.URL, Source: ".env 的 PB_API_PORT"},
		{URL: real.URL, Source: ".env 的 PB_FRONTEND_PORT"},
	})
	if err != nil {
		t.Fatalf("应探到可用端点: %v", err)
	}
	if chosen.URL != real.URL {
		t.Fatalf("应跳过非 API 响应端点，选中 %q（实际 %q）", real.URL, chosen.URL)
	}
}

// TestSelectEndpointAllFailReportsFirstError —— 全部候选都探不到时，返回首个失败原因
// （供输出给出可操作的下一步）。
func TestSelectEndpointAllFailReportsFirstError(t *testing.T) {
	chosen, err := selectEndpoint([]endpointCandidate{
		{URL: "http://127.0.0.1:1", Source: "候选一"},
		{URL: "http://127.0.0.1:2", Source: "候选二"},
	})
	if chosen.URL != "" {
		t.Fatalf("全失败时不得返回候选，实际 %q", chosen.URL)
	}
	if err == nil {
		t.Fatal("全失败时必须给出原因")
	}
	if !strings.Contains(err.Error(), "候选一") && !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("原因应指向首个候选，实际 %v", err)
	}
}

// TestResolveEndpointCandidateOrder —— 候选顺序：PB_SERVER > PB_FRONTEND_PORT > PB_API_PORT。
//
// ⚠️ 顺序有意义：真机上 PB_API_PORT 是 **TLS 端口**（明文打过去 400），
// 明文回环前端口（PB_FRONTEND_PORT）才是能答话的 ⇒ 必须排在前面。
func TestResolveEndpointCandidateOrder(t *testing.T) {
	clearEndpointEnv(t)
	path := writeEnv(t, "PB_API_PORT=56789\nPB_FRONTEND_PORT=56790\n")
	useEnvCandidates(t, path)

	ep := resolveEndpoint()
	if len(ep.Candidates) != 2 {
		t.Fatalf("应有 2 个候选，实际 %d: %+v", len(ep.Candidates), ep.Candidates)
	}
	if ep.Candidates[0].URL != "http://127.0.0.1:56790" {
		t.Fatalf("PB_FRONTEND_PORT 应优先于 PB_API_PORT，实际 %q", ep.Candidates[0].URL)
	}
	if ep.Candidates[1].URL != "http://127.0.0.1:56789" {
		t.Fatalf("PB_API_PORT 候选不对: %q", ep.Candidates[1].URL)
	}
	if !strings.Contains(ep.Candidates[0].Source, "PB_FRONTEND_PORT") {
		t.Fatalf("来源标注应含键名，实际 %q", ep.Candidates[0].Source)
	}
}

// TestResolveEndpointEnvFileServerIsOnlyCandidate —— .env 里的 PB_SERVER 存在时，
// 不再产出其它候选（用户已明确给了地址）。
func TestResolveEndpointEnvFileServerIsOnlyCandidate(t *testing.T) {
	clearEndpointEnv(t)
	path := writeEnv(t, "PB_SERVER=http://box.example.com:12345\nPB_API_PORT=56789\n")
	useEnvCandidates(t, path)

	ep := resolveEndpoint()
	if len(ep.Candidates) != 1 {
		t.Fatalf("PB_SERVER 存在时只应有 1 个候选，实际 %d: %+v", len(ep.Candidates), ep.Candidates)
	}
	if ep.Candidates[0].URL != "http://box.example.com:12345" {
		t.Fatalf("候选 URL 不对: %q", ep.Candidates[0].URL)
	}
}
