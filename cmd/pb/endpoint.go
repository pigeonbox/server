// endpoint.go —— 端点与凭据的解析（issue #117）。
//
// # 为什么需要它
//
// 真机实录（2026-11-10，10.10.10.3）：`pb doctor` 的目标端点**只能来自环境变量**——
//
//	serverURL = strings.TrimRight(envOr("PB_SERVER", "http://localhost:12345"), "/")
//
// 既没有命令行参数，也不读部署目录的 .env ⇒ 在**非默认端口**的真实部署上
// 裸跑必然打到一个没人监听的地址 ⇒ 结论恒为「服务不可达」⇒ **假红**。
//
// 该机的后端 12345 端口**不发布**（对外 TLS 56789 / 回环明文 56790，
// TLS 由 pigeonbox-tls 终结并透传 X-Forwarded-Proto），而 .env 里明明
// 写着端口 ⇒ 「没告诉 doctor 地址」被说成了「服务坏了」。
//
// # 端点解析：两类来源，规则不同
//
// **① 显式覆盖**（用户明确说了「就是这个地址」）⇒ **独占**，不回落：
//
//   - 命令行 --server <url>（doctor 专属）
//   - 环境变量 PB_SERVER
//
// ⚠️ 显式指定时不回落到别的候选：用户已经指了地址，探别的端口等于
// 「不听指挥」（还会把请求打到别的主机/端口上）。
//
// **② 部署描述符**（.env 里写的部署事实）⇒ **有序候选列表**，逐个探测：
//
//   1. .env 的 PB_SERVER         （显式地址，最优先）
//   2. .env 的 PB_FRONTEND_PORT   （明文回环前端口）
//   3. .env 的 PB_API_PORT        （对外发布端口，**可能是 TLS 端口**）
//
// 都没有 ⇒ 内置默认地址 http://localhost:12345 ⇒ **视为「未配置」**（doctor 报 na）。
//
// # 为什么要「候选 + 探测」而不是「读一个键就完事」
//
// 真机 10.10.10.3 的 .env 里 **PB_API_PORT=56789 指的是 TLS 端口**：
//
//	$ curl http://127.0.0.1:56789/api/config
//	400 The plain HTTP request was sent to HTTPS port   ← nginx 的明文/TLS 提示
//
// 真正能答话的是明文回环前端口（PB_FRONTEND_PORT=56790）：
//
//	$ curl http://127.0.0.1:56790/api/config   → 200（JSON）
//
// ⇒ 只读一个键**仍然会假红**（400 会被当成「不可达」）。
// ⇒ 故：解析出**有序候选列表**，doctor 逐个探测，取第一个「确实是 PigeonBox
// API」的端点（见 probeEndpoint 的判据），并把**选中的那个**标注进输出。
//
// # 解析优先级（API Key）
//
//	1. 命令行 --api-key-file <path> （**只给路径**，明文不进 argv）
//	2. 环境变量 PB_API_KEY
//	3. .env 的 PB_API_KEY
//
// ⚠️ **刻意不提供 --api-key 明文形式**：命令行参数会进 `ps` 与 shell 历史
// （与 p2p push 的 PB_P2P_CODE 同一条理由）⇒ 给了 --api-key 直接当用法错误拒绝。
//
// # .env 候选位置
//
//	$PIGEONBOX_DIR/.env → ./.env → /opt/pigeonbox/.env → /etc/pigeonbox/.env
//
// 逐键取**第一个命中的文件**（不同键可以来自不同文件），并在输出里标注
// 「哪个文件、哪个键」——否则运维无法判断 doctor 到底读了什么。

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultServerURL 内置默认端点。
//
// ⚠️ 它**不是**一个「已配置的端点」：doctor 遇到它时按「未配置」处理
// （报 na 而不是报 fail）——见 endpointConfigured 的口径。
//
// 做成 var（不是 const）：单测需要一个「确定连不上的默认地址」来验证
// 「未配置端点 ⇒ na 而非 fail」这条三态边界。
var defaultServerURL = "http://localhost:12345"

// endpointCandidate 一个候选端点及其来源（人读，用于输出标注）。
type endpointCandidate struct {
	// URL 服务端地址（末尾无 "/"）。
	URL string
	// Source 来源说明：哪个文件、哪个键。
	// ⚠️ 只放来源标签，**绝不放密钥明文**。
	Source string
}

// endpoint 一次调用用的端点与凭据，以及它们的**来源**（人读，用于输出标注）。
type endpoint struct {
	// URL 当前生效的服务端地址（末尾无 "/"）。
	URL string
	// URLSource URL 的来源说明；空 ⇒ **未配置**（用了内置默认地址）。
	URLSource string
	// APIKey 用户级 API Key；空 ⇒ 没有凭据（只能做公开面检查）。
	APIKey string
	// APIKeySource 凭据来源说明；空 ⇒ 未配置。
	// ⚠️ **只放来源标签，绝不放密钥本身**。
	APIKeySource string
	// Candidates 有序候选端点（doctor 逐个探测，取第一个真答话的）。
	Candidates []endpointCandidate
}

// dotenvCandidatesFn 取 .env 候选路径（按优先级）。抽成变量是为了让单测
// 能替换候选集（真机上 /opt/pigeonbox/.env 可能真实存在 ⇒ 不能只靠 chdir）。
var dotenvCandidatesFn = dotenvCandidates

// dotenvCandidates .env 候选路径（按优先级）。
func dotenvCandidates() []string {
	var out []string
	if d := strings.TrimSpace(os.Getenv("PIGEONBOX_DIR")); d != "" {
		out = append(out, filepath.Join(d, ".env"))
	}
	out = append(out,
		".env",
		"/opt/pigeonbox/.env",
		"/etc/pigeonbox/.env",
	)
	return out
}

// parseDotEnv 解析 .env 文本 ⇒ 键值表。
//
// 极简实现（compose 风格）：
//   - 空行与以 '#' 开头的行忽略；
//   - 允许 `export KEY=VALUE` 前缀；
//   - 值两侧的成对单/双引号去掉；
//   - 第一个 '=' 之后全部算值（值里可以含 '='）。
//
// ⚠️ **不解析行尾注释**：compose 的行尾注释规则本身有歧义（值里含 '#'
// 时到底算不算注释），这里的取舍是「宁可把 '#' 当普通字符」——
// 端口/URL/Key 这三个键的值里出现 '#' 的概率极低，而误吞字符会得到
// 一个更离谱的端点。
func parseDotEnv(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "" {
			continue
		}
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}
		out[key] = val
	}
	return out
}

// lookupDotEnv 在候选文件里找第一个定义了 key 的文件。
//
// 返回 (值, 文件路径, 该文件的键值表, 是否命中)。第三个返回值用于取同文件里的
// 配套键（如 PB_API_PORT 与 PB_API_BIND 必须来自同一份编排）。
func lookupDotEnv(key string) (value string, file string, all map[string]string, found bool) {
	for _, cand := range dotenvCandidatesFn() {
		raw, err := os.ReadFile(cand)
		if err != nil {
			continue
		}
		m := parseDotEnv(string(raw))
		v, ok := m[key]
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		return v, absPath(cand), m, true
	}
	return "", "", nil, false
}

// absPath 把候选路径解析成**绝对路径**（来源标注用）。
//
// ⚠️ 相对路径（「.env」）在输出里看不出读的是哪个文件（运维会以为读的是
// 当前目录的，而实际是相对 CWD 解析的）⇒ 统一打成绝对路径。
func absPath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// normalizeServerURL 规整端点：去尾斜杠；没写 scheme 时补 http://。
func normalizeServerURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	return strings.TrimRight(s, "/")
}

// urlFromPort 由端口 + 监听地址拼出端点。
//
// ⚠️ 监听地址为 0.0.0.0 / :: 时改用 127.0.0.1：0.0.0.0 不是可连接的地址。
func urlFromPort(port string, all map[string]string) string {
	host := strings.TrimSpace(all["PB_API_BIND"])
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return normalizeServerURL("http://" + host + ":" + strings.TrimSpace(port))
}

// resolveEndpoint 解析端点候选与凭据（**不含命令行参数**——那是 doctor 层的职责）。
//
// 来源优先级见文件头。返回值里的 URLSource/APIKeySource 用于输出标注，
// **不含任何密钥明文**。
func resolveEndpoint() endpoint {
	var ep endpoint

	// 端点候选（有序）。
	//
	// ⚠️ 环境变量 PB_SERVER 是**显式覆盖** ⇒ 独占（不产 .env 里的候选）：
	// 用户在 shell 里明确指了地址，再去探 .env 的端口等于「不听指挥」。
	if v := strings.TrimSpace(os.Getenv("PB_SERVER")); v != "" {
		ep.Candidates = append(ep.Candidates, endpointCandidate{
			URL:    normalizeServerURL(v),
			Source: "环境变量 PB_SERVER",
		})
	} else {
		// .env = 部署描述符 ⇒ 有序候选列表（逐个探测，取第一个真答话的）。
		//
		// ⚠️ .env 里若写了 PB_SERVER（显式地址）⇒ **独占**：
		// 部署方已经指明了地址，再拿端口去探属于越权猜测。
		if v, file, _, ok := lookupDotEnv("PB_SERVER"); ok {
			ep.Candidates = append(ep.Candidates, endpointCandidate{
				URL:    normalizeServerURL(v),
				Source: fmt.Sprintf(".env %s 的 PB_SERVER", file),
			})
		} else {
			// PB_FRONTEND_PORT：明文回环前端口（TLS 终结层部署下唯一能明文答话的）。
			if port, file, all, ok := lookupDotEnv("PB_FRONTEND_PORT"); ok {
				ep.Candidates = append(ep.Candidates, endpointCandidate{
					URL:    urlFromPort(port, all),
					Source: fmt.Sprintf(".env %s 的 PB_FRONTEND_PORT", file),
				})
			}
			// PB_API_PORT：对外发布端口（**可能是 TLS 端口** ⇒ 明文打过去是 400）。
			if port, file, all, ok := lookupDotEnv("PB_API_PORT"); ok {
				ep.Candidates = append(ep.Candidates, endpointCandidate{
					URL:    urlFromPort(port, all),
					Source: fmt.Sprintf(".env %s 的 PB_API_PORT", file),
				})
			}
		}
	}

	// 凭据。
	switch {
	case strings.TrimSpace(os.Getenv("PB_API_KEY")) != "":
		ep.APIKey = os.Getenv("PB_API_KEY")
		ep.APIKeySource = "环境变量 PB_API_KEY"
	default:
		if v, file, _, ok := lookupDotEnv("PB_API_KEY"); ok {
			ep.APIKey = v
			ep.APIKeySource = fmt.Sprintf(".env %s 的 PB_API_KEY", file)
		}
	}

	applyCandidates(&ep)
	return ep
}

// applyCandidates 把候选列表落到 ep 的「当前端点」字段上。
//
// 候选为空 ⇒ 落到内置默认地址且**来源为空**（= 未配置，doctor 报 na）。
func applyCandidates(ep *endpoint) {
	if len(ep.Candidates) > 0 {
		ep.URL = ep.Candidates[0].URL
		ep.URLSource = ep.Candidates[0].Source
		return
	}
	ep.URL = defaultServerURL
	ep.URLSource = ""
}

// applyEndpoint 把解析结果写进全局（进程入口与单测共用）。
func applyEndpoint(ep endpoint) {
	serverURL = ep.URL
	serverURLSource = ep.URLSource
	apiKey = ep.APIKey
	apiKeySource = ep.APIKeySource
}

// currentEndpoint 取当前生效的端点（从全局还原）。
func currentEndpoint() endpoint {
	return endpoint{
		URL:          serverURL,
		URLSource:    serverURLSource,
		APIKey:       apiKey,
		APIKeySource: apiKeySource,
	}
}

// endpointConfigured 判断当前端点是否算「已配置的部署端点」。
//
// 口径（doctor 的三态结论靠它区分「未配置」与「不可达」）：
//
//   - 来源非空（命令行 / 环境变量 / .env）⇒ **已配置**；
//   - 来源为空但 URL 不等于内置默认地址 ⇒ **已配置**
//     （程序内直接设置：单测夹具、其它命令的编程用法）；
//   - URL 等于内置默认地址且无来源 ⇒ **未配置** ⇒ doctor 报 na，
//     绝不报「服务不可达」（没配置地址 ≠ 服务坏了）。
func endpointConfigured(ep endpoint) bool {
	if ep.URLSource != "" {
		return true
	}
	return ep.URL != "" && ep.URL != defaultServerURL
}

// looksLikeAPI 判断响应体是不是 PigeonBox 的 API 信封（JSON 且含 code 字段）。
//
// ⚠️ 判据**不能**是「有 HTTP 响应就算活着」：真机 10.10.10.3 的 TLS 端口
// 用明文 HTTP 打过去会回 nginx 的 HTML 提示页：
//
//	400 The plain HTTP request was sent to HTTPS port
//
// 那是个**响应**，但**不是 PigeonBox 的响应** ⇒ 据此判「可达」会得到
// 「端点可达、但后续全 400」的假绿。
func looksLikeAPI(body []byte) bool {
	var probe struct {
		Code *int `json:"code"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return probe.Code != nil
}

// probeEndpoint 探一次候选端点：GET {url}/api/config。
//
// 返回 (alive bool, err error)：alive = 拿到的响应**确实是 PigeonBox API**
// （见 looksLikeAPI）。alive=false 时 err 带上原因（供 advice 用）。
func probeEndpoint(url string) (bool, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(url, "/") + "/api/config")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, fmt.Errorf("读取响应失败: %w", err)
	}
	if !looksLikeAPI(body) {
		return false, fmt.Errorf("响应不是 PigeonBox API（HTTP %d）", resp.StatusCode)
	}
	return true, nil
}
