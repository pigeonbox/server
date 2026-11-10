// doctor.go —— pb doctor 部署自检命令（2026-10-30，第一趟 4.2.0）。
//
// # 为什么有它
//
// 9 种部署面（docker / charts / fnos / openwrt / qnap / synology / ugreen /
// terramaster / desktop）差异极大：端口、存储后端、Redis 有无、随包 Beam 与否
// 都不同。用户「装完不ら」时缺的不是日志，而是**一条能给出结论的命令**：
//
//	pb doctor        # 人读的结论（每行 = 判定 + 证据 + 下一步）
//	pb doctor --json # 机器读（CI / 巡检）
//
// # 数据来源
//
// 深度检查走管理面端点 GET /admin/diagnostics（core 侧；鉴权 = admin JWT 或
// **admin 用户的 API Key**）——诊断逻辑集中在 core 一处，CLI 只做编排与呈现。
//
// 无 admin Key 时**降级**为公开面检查（/api/config + /version）：仍然给出结论，
// 但明确标注"深度检查未执行（需要 admin API Key）"，不静默假装检查过。
//
// # 端点与凭据（issue #117）
//
// `pb doctor` 的端点**不再只认环境变量**：
//
//   - `--server <url>`：显式指定端点（优先级最高）；
//   - 未给定时按 环境变量 PB_SERVER → 部署目录 .env（PB_SERVER / PB_API_PORT）
//     → 内置默认地址 的顺序解析（见 endpoint.go）；
//   - API Key 走 `--api-key-file <路径>` / 环境变量 PB_API_KEY / .env 的 PB_API_KEY。
//     ⚠️ **不接受明文 --api-key**：命令行参数会进 `ps` 与 shell 历史。
//
// **读到的来源会标注在输出里**（哪个文件、哪个键）——否则运维无法判断
// doctor 到底读了什么。
//
// # 三态结论（issue #117）
//
// 「地址没配」与「端点不可达」**必须分开**，否则运维看到 [FAIL] 会误判成
// 「服务坏了」（真机 10.10.10.3：后端 12345 端口不发布，裸跑 doctor 打的
// 是没人监听的默认地址 ⇒ 恒假红）。
//
//   - ok    端点已配置且探到了
//   - na    **未配置端点**（没有 --server / PB_SERVER / .env）⇒ 不是故障，
//           不探测、不报 fail，只提示怎么配
//   - fail  端点**已配置但不可达** ⇒ 真问题（配错了/服务没起）
//
// # 退出码
//
// 沿用 oekit/pkg/cli 的全局语义（0 成功 / 1 运行失败 / 2 用法错误）——
// **不新造语义**：2 是「用法错误」，挪作「故障」会让脚本把「服务端有故障」
// 误读成「命令写错了」。
//
//	- 0 健康：全部检查通过；
//	- 1 有问题：有 WARN（配置缺失）或 FAIL（真故障），或深度检查未执行；
//	- 2 用法错误：未知参数。
//
// ⚠️ WARN 与 FAIL **不靠退出码区分**（靠输出：结论行 / JSON 的 summary）——
// 「检查不完整」也归到 1：没检查 ≠ 检查通过（静默降级比报错危险）。

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// doctorCheck 一条检查结论。
type doctorCheck struct {
	// Name 检查项名（人读的短名）。
	Name string `json:"name"`
	// Status ok | warn | fail | skip | na
	//
	// ⚠️ na = **not applicable**：该项**无法判定**（当前是「端点未配置」）
	// ⇒ 与 fail 严格分开：na 不是故障，不该让人以为服务坏了（issue #117）。
	Status string `json:"status"`
	// Detail 证据/取值。
	Detail string `json:"detail"`
	// Advice 下一步（可为空）。
	Advice string `json:"advice,omitempty"`
	// Advisory 提示级（core#121）：显示 WARN 但**不参与退出码**。
	//
	// 只用于「必须可见、但部署本身没坏」的项（如 ClamAV 扫描器不可用：
	// fail-open 放行中、业务照常）——若并入 warn，巡检/CI 会恒红，
	// 与「新版本」项的告警疲劳同一条理由。退出码口径见 doctorSummary.worst。
	Advisory bool `json:"advisory,omitempty"`
}

// doctorReport 一次自检的完整结论。
type doctorReport struct {
	Server  string        `json:"server"`
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
	// Deep true = 执行了深度检查（拿得到 /admin/diagnostics）。
	Deep bool `json:"deep"`
	// ServerSource 端点来源（哪个文件、哪个键）；空 = 未标注来源。
	// ⚠️ 只放来源标签，**不含密钥明文**。
	ServerSource string `json:"server_source,omitempty"`
	// APIKeySource 凭据来源；空 = 未提供凭据。
	// ⚠️ 只放来源标签，**不含密钥明文**。
	APIKeySource string `json:"api_key_source,omitempty"`
	// ProbeError 候选端点探测失败的原因（用于给出可操作的下一步）。
	// 空 = 没探过（已配置且探到了）。
	ProbeError string `json:"probe_error,omitempty"`
}

// doctorSummary 结论统计。
type doctorSummary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
	// NA = **无法判定**的项（当前只有「端点未配置」这一种）。
	// ⚠️ 与 Fail 严格分开：na 不是故障（issue #117）。
	NA int `json:"na"`
	// Note = advisory 提示级条数（WARN 显示但不改退出码，core#121）。
	Note  int `json:"note"`
	Total int `json:"total"`
}

// status 严重度排序：fail > warn > ok（skip 不参与）。
func (s doctorSummary) worst() string {
	switch {
	case s.Fail > 0:
		return "fail"
	case s.Warn > 0:
		return "warn"
	case s.NA > 0:
		// ⚠️ na **不是** fail（端点没配 ≠ 服务坏了），但同样意味着
		// 「没检查成」⇒ 退出码仍为 1（没检查 ≠ 检查通过）。
		return "warn"
	default:
		return "ok"
	}
}

// doctorCapabilities 服务端算出的能力位（**生效值**，不是配置回显）。
//
// ⚠️ 这段是「部署模式与被降级的能力」这条检查项的**唯一数据来源**：
// 多副本拓扑下 core 的 deployment.go 会把联邦强制关闭（节点身份是进程级的
// Ed25519，多副本语义未定义），存储远端后端构造失败也会降级到 local
// ⇒ 这两件事用户从日志里看不出来，doctor 必须替他说出来。
type doctorCapabilities struct {
	DeploymentMode  string `json:"deployment_mode"`
	PublicURLSet    bool   `json:"public_url_set"`
	SetupTokenReq   bool   `json:"setup_token_required"`
	Federation      bool   `json:"federation"`
	RedisMode       string `json:"redis_mode"`
	StorageBackend  string `json:"storage_backend"`
	StorageDegraded bool   `json:"storage_degraded"`
}

// doctorPayload 服务端诊断端点的响应形状（只取用到的字段）。
type doctorPayload struct {
	Version    string `json:"version"`
	Deployment string `json:"deployment_mode"`
	ServerTime string `json:"server_time"`
	Database   struct {
		Driver    string `json:"driver"`
		Reachable bool   `json:"reachable"`
		Error     string `json:"error"`
	} `json:"database"`
	Storage struct {
		Backend    string `json:"backend"`
		Configured string `json:"configured"`
		Degraded   bool   `json:"degraded"`
		DataPath   string `json:"data_path"`
		InitError  string `json:"init_error"`
	} `json:"storage"`
	Redis struct {
		Mode    string `json:"mode"`
		Address string `json:"address"`
	} `json:"redis"`
	Site struct {
		PublicURL    string `json:"public_url"`
		PublicURLSet bool   `json:"public_url_set"`
	} `json:"site"`
	Setup struct {
		Initialized   bool   `json:"initialized"`
		TokenRequired bool   `json:"token_required"`
		TokenSource   string `json:"token_source"`
	} `json:"setup"`
	// Capabilities 服务端算出的能力位（**生效值**，不是配置回显）。
	//
	// ⚠️ 这段是「部署模式与被降级的能力」这条检查项的**唯一数据来源**：
	// 多副本拓扑下 core 的 deployment.go 会把联邦强制关闭（节点身份是进程级的
	// Ed25519，多副本语义未定义），存储远端后端构造失败也会降级到 local
	// ⇒ 这两件事用户从日志里看不出来，doctor 必须替他说出来。
	Capabilities doctorCapabilities `json:"capabilities"`
	// Update 更新状态（2026-11-06，5.0.0）。服务端默认 24h 缓存；
	// source=unknown 表示查不到（离线 / 超时 / 已关闭），**不是故障**。
	Update struct {
		Current   string `json:"current"`
		Latest    string `json:"latest"`
		Available bool   `json:"available"`
		Source    string `json:"source"`
		Stale     bool   `json:"stale"`
		URL       string `json:"url"`
		Error     string `json:"error"`
	} `json:"update"`
	// Moderation 内容审核/病毒扫描状态（core#121）。
	//
	// ⚠️ 指针：旧服务端没有这段 ⇒ 「未取到」≠「未启用」（与「不知道」和
	// 「坏了」分开报的同一条原则）。
	Moderation *doctorModeration `json:"moderation"`
	// Backup 定时自动备份状态（2026-11-06，5.1.0）。**纯本地**，不出站。
	Backup struct {
		Enabled      bool   `json:"enabled"`
		Dir          string `json:"dir"`
		Keep         int    `json:"keep"`
		At           string `json:"at"`
		Count        int    `json:"count"`
		TotalBytes   int64  `json:"total_bytes"`
		LastBackupAt string `json:"last_backup_at"`
		LastFile     string `json:"last_file"`
		LastSize     int64  `json:"last_size"`
		Running      bool   `json:"running"`
		LastError    string `json:"last_error"`
	} `json:"backup"`
}

// doctorModeration 内容审核段（core#121；只取用到的字段）。
type doctorModeration struct {
	Enabled bool         `json:"enabled"`
	ClamAV  doctorClamAV `json:"clamav"`
}

// doctorClamAV 病毒扫描可观测段。
type doctorClamAV struct {
	Enabled       bool              `json:"enabled"`
	Addr          string            `json:"addr"`
	Degraded      bool              `json:"degraded"`
	DegradedCount int               `json:"degraded_count"`
	WindowSize    int               `json:"window_size"`
	ScansInWindow int               `json:"scans_in_window"`
	LastPing      *doctorClamAVPing `json:"last_ping"`
}

// doctorClamAVPing 一次探活结果。
type doctorClamAVPing struct {
	OK         bool   `json:"ok"`
	At         string `json:"at"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error"`
}

// doctorPublicPayload /api/config 的响应形状（降级检查用到的字段）。
type doctorPublicPayload struct {
	Version      string `json:"version"`
	PublicURL    string `json:"publicUrl"`
	Capabilities struct {
		DeploymentMode     string `json:"deployment_mode"`
		PublicURLSet       bool   `json:"public_url_set"`
		SetupTokenRequired bool   `json:"setup_token_required"`
	} `json:"capabilities"`
}

// doctorFlags pb doctor 的旗标（自研解析，不用标准库 flag —— 见 main.go 的说明）。
//
// ⚠️ **刻意没有 --api-key 明文形式**：命令行参数会进 `ps` 与 shell 历史
// （与 p2p push 的 PB_P2P_CODE 同一条理由）⇒ 给了 --api-key 直接当用法错误拒绝，
// 并指向 --api-key-file / 环境变量 PB_API_KEY。
type doctorFlags struct {
	jsonOut    bool
	server     string
	apiKeyFile string
}

// parseDoctorFlags 解析 pb doctor 的旗标。
//
// 旗标缺值 ⇒ **用法错误**（退出码 2），与 parsePutFlags 同一口径：
// 静默吞掉旗标会让「没配端点」变成「配了但没人知道」。
func parseDoctorFlags(args []string) (doctorFlags, error) {
	var f doctorFlags
	needValue := func(name string, i int) (string, int, error) {
		if i+1 >= len(args) {
			return "", i, errUsage("旗标 " + name + " 缺少取值")
		}
		return args[i+1], i + 1, nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			f.jsonOut = true
		case a == "--server":
			v, ni, err := needValue("--server", i)
			if err != nil {
				return f, err
			}
			// ⚠️ 空端点必须报错：静默回落成默认地址 ⇒ 又变成「打错地方」。
			if strings.TrimSpace(v) == "" {
				return f, errUsage("--server 不能为空（要给一个 http(s):// 地址）")
			}
			f.server = v
			i = ni
		case strings.HasPrefix(a, "--server="):
			f.server = strings.TrimPrefix(a, "--server=")
			if strings.TrimSpace(f.server) == "" {
				return f, errUsage("--server 不能为空（要给一个 http(s):// 地址）")
			}
		case a == "--api-key-file":
			v, ni, err := needValue("--api-key-file", i)
			if err != nil {
				return f, err
			}
			if strings.TrimSpace(v) == "" {
				return f, errUsage("--api-key-file 不能为空（要给一个文件路径）")
			}
			f.apiKeyFile = v
			i = ni
		case strings.HasPrefix(a, "--api-key-file="):
			f.apiKeyFile = strings.TrimPrefix(a, "--api-key-file=")
			if strings.TrimSpace(f.apiKeyFile) == "" {
				return f, errUsage("--api-key-file 不能为空（要给一个文件路径）")
			}
		case a == "--api-key" || strings.HasPrefix(a, "--api-key="):
			// ⚠️ 拒绝明文密钥：argv 会进 ps 与 shell 历史。
			return f, errUsage("--api-key 不接受明文密钥（会进 ps 与 shell 历史）；请用 --api-key-file <路径> 或环境变量 PB_API_KEY")
		default:
			return f, errUsage("未知参数: " + a + "（pb doctor 支持 --json / --server <url> / --api-key-file <路径>）")
		}
	}
	return f, nil
}

// readKeyFile 读 API Key 文件（只给路径，明文不进 argv）。
//
// ⚠️ 读到的**内容**只在内存里用，**绝不进输出与日志**；
// 输出里只出现「来源：命令行 --api-key-file <路径>」这样的标签。
func readKeyFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取 API Key 文件失败: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("API Key 文件为空: %s", path)
	}
	return key, nil
}

// reachabilityStatus 判定「服务可达」这条检查的三态（issue #117）。
//
// ⚠️ 这三态的边界就是 issue 的核心：**「没配端点」与「端点不可达」
// 必须分开**——把前者印成 [FAIL] 会让运维以为服务坏了（真机 10.10.10.3：
// 后端 12345 端口不发布 ⇒ 裸跑 doctor 打的默认地址没人监听 ⇒ 恒假红）。
//
//   - reachable && configured ⇒ ok（探到了）
//   - reachable && !configured ⇒ ok（默认地址探到了；来源未配置仍会单独标注）
//   - !reachable && configured ⇒ **fail**（配了却连不上 ⇒ 真问题）
//   - !reachable && !configured ⇒ **na**（不知道服务在哪 ⇒ 无法判定，
//     **不是**故障）
func reachabilityStatus(configured, reachable bool) string {
	switch {
	case reachable:
		return "ok"
	case configured:
		return "fail"
	default:
		return "na"
	}
}

// hasCandidate 判断候选列表里是否已有该地址（避免重复探测）。
func hasCandidate(cands []endpointCandidate, url string) bool {
	for _, c := range cands {
		if c.URL == url {
			return true
		}
	}
	return false
}

// selectEndpoint 逐个探测候选端点，返回**第一个确实是 PigeonBox API** 的那个。
//
// ⚠️ 判据不是「有响应就算活着」：TLS 端口用明文 HTTP 打过去会回 nginx 的
// HTML 提示页（HTTP 400）——那是响应，但**不是 PigeonBox 的响应**
// ⇒ 据此判「可达」会得到「端点可达、但后续全 400」的假绿。
//
// 返回 (选中的候选, 错误说明)。全部候选都探不到时返回零值候选 + 首个失败原因。
func selectEndpoint(cands []endpointCandidate) (endpointCandidate, error) {
	var firstErr error
	for _, c := range cands {
		alive, err := probeEndpoint(c.URL)
		if alive {
			return c, nil
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s：%w", c.URL, err)
		}
	}
	return endpointCandidate{}, firstErr
}

// describeEndpoint 产出端点来源的**人读**说明。
//
// ⚠️ 只描述「哪来的」，**绝不包含任何密钥明文**。
func describeEndpoint(ep endpoint) string {
	if ep.URLSource != "" {
		return ep.URLSource
	}
	if ep.URL != "" && ep.URL != defaultServerURL {
		return "直接指定（未标注来源）"
	}
	return "未配置（未找到 --server / 环境变量 PB_SERVER / .env 的 PB_SERVER 或 PB_API_PORT）"
}

// cmdDoctor 执行部署自检。
//
// 用法：
//
//	pb doctor                      人读结论
//	pb doctor --json               机器读（JSON）
//	pb doctor --server <url>       显式指定端点（issue #117）
//	pb doctor --api-key-file <路径> 从文件读 API Key（明文不进 argv）
func cmdDoctor(args []string) error {
	f, err := parseDoctorFlags(args)
	if err != nil {
		return err
	}

	// 候选端点：默认取启动时解析出的有序候选；--server **独占**（不回落到其它候选）。
	//
	// ⚠️ 显式 --server 时不回落：用户明确指定了地址，探别的端口等于
	// 「不听指挥」（而且会把请求打到别的主机/端口上）。
	// 候选端点**每次现取**（不缓存）：进程启动时解析过一次，但 doctor 可能
	// 在别的上下文里被调用；现取保证「读到什么」永远与当下环境一致。
	cur := currentEndpoint()
	cands := resolveEndpoint().Candidates
	if f.server != "" {
		// 显式指定 ⇒ **独占**：用户指了地址，探别的端口等于「不听指挥」。
		cands = []endpointCandidate{{
			URL:    normalizeServerURL(f.server),
			Source: "命令行 --server",
		}}
	} else if len(cands) == 0 && cur.URL != "" {
		// 环境与 .env 都没给地址 ⇒ 回落：若程序内已直接设置过端点（单测夹具 /
		// 编程调用），拿它当候选；否则就是内置默认地址（= 未配置）。
		cands = []endpointCandidate{{URL: cur.URL, Source: cur.URLSource}}
	}

	ep := endpoint{Candidates: cands}
	if f.apiKeyFile != "" {
		key, kerr := readKeyFile(f.apiKeyFile)
		if kerr != nil {
			return kerr
		}
		ep.APIKey = key
		ep.APIKeySource = "命令行 --api-key-file " + f.apiKeyFile
	} else {
		// 未显式给 Key ⇒ 用启动时解析出的（env / .env）。
		ep.APIKey = apiKey
		ep.APIKeySource = apiKeySource
	}

	// 生效到全局（请求层读的就是这两个全局）。
	// ⚠️ 结束时还原：doctor 不该给后续逻辑留下副作用（单测连续调用靠它隔离）。
	oldURL, oldURLSrc := serverURL, serverURLSource
	oldKey, oldKeySrc := apiKey, apiKeySource
	defer func() {
		serverURL, serverURLSource = oldURL, oldURLSrc
		apiKey, apiKeySource = oldKey, oldKeySrc
	}()

	// 逐个探测候选，取**第一个确实是 PigeonBox API** 的端点。
	//
	// ⚠️ 为什么必须探：真机 10.10.10.3 的 .env 里 PB_API_PORT 指的是 **TLS 端口**，
	// 明文打过去是 nginx 的 400 提示页 ⇒ 只读一个键**仍然会假红**（issue #117）。
	chosen, probeErr := selectEndpoint(cands)
	if chosen.URL != "" {
		ep.URL = chosen.URL
		ep.URLSource = chosen.Source
	} else {
		// 一个都没探到 ⇒ 用第一个候选（供输出展示「试的是哪个地址」）。
		if len(cands) > 0 {
			ep.URL = cands[0].URL
			ep.URLSource = cands[0].Source
		} else {
			ep.URL = defaultServerURL
			ep.URLSource = ""
		}
	}
	serverURL, serverURLSource = ep.URL, ep.URLSource
	apiKey, apiKeySource = ep.APIKey, ep.APIKeySource

	configured := endpointConfigured(ep)
	report := doctorReport{
		Server:       ep.URL,
		ServerSource: describeEndpoint(ep),
		APIKeySource: ep.APIKeySource,
	}
	if probeErr != nil {
		report.ProbeError = probeErr.Error()
	}

	// 深度检查：管理面诊断端点（需要 admin API Key）。
	deep, derr := fetchDiagnostics()
	if derr == nil && deep != nil {
		report.Deep = true
		report.Checks = append(report.Checks, checksFromDiagnostics(deep)...)
	} else {
		// 拿不到深度数据 ⇒ 降级为公开面检查，并**明确**说明未做深度检查。
		// 深度检查没跑成的原因分两类，**严重度不同**（issue #117）：
		//   - 端点没配 ⇒ **na**（无从谈起，不是故障；先配端点）；
		//   - 端点配了但拿不到管理面数据 ⇒ warn（Key 缺失/权限不足/服务端拒绝）。
		deepStatus := "warn"
		reason := "未设置 PB_API_KEY"
		if apiKey != "" {
			reason = "需要 admin 用户的 API Key（当前 Key 无管理面权限或服务端不可达）"
			if apiErr, ok := derr.(*doctorAuthError); ok {
				reason = apiErr.Reason
			}
		}
		if !configured {
			// ⚠️ 端点没配 ⇒ 这次失败**多半是打错地址**，不是 Key 的问题。
			// 报「Key 无权限」会把人引到错误的方向（issue #117）。
			deepStatus = "na"
			reason = "未配置端点（没有 --server / PB_SERVER / .env，请求打到默认地址 " + ep.URL + "）"
		}
		report.Checks = append(report.Checks, doctorCheck{
			Name:   "深度检查",
			Status: deepStatus,
			Detail: "未执行（" + reason + "）",
			Advice: "用 --server <url> 指定端点，并在用户中心用管理员账号创建 API Key（PB_API_KEY 或 --api-key-file）",
		})
		report.Checks = append(report.Checks, checksFromPublic(configured, probeErr)...)
	}

	report.Summary = summarize(report.Checks)
	return doctorFinish(report, f.jsonOut)
}

// doctorFinish 输出结论并映射退出码（0 健康 / 1 有待处置或无法判定 / 2 用法错误）。
func doctorFinish(report doctorReport, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("JSON 输出失败: %w", err)
		}
	} else {
		printDoctorReport(report)
	}

	switch report.Summary.worst() {
	case "fail", "warn":
		return &doctorExitError{report: report}
	default:
		return nil
	}
}

// doctorAuthError 取不到管理面数据（鉴权不足 / 端点不存在 / 网络不通）。
type doctorAuthError struct {
	Reason string
}

func (e *doctorAuthError) Error() string { return e.Reason }

// doctorExitError 自检发现待处置项（WARN）或故障（FAIL）。
//
// 退出码统一为 1（运行失败），严重度由输出/JSON 表达——见文件头「退出码」节。
type doctorExitError struct {
	report doctorReport
}

func (e *doctorExitError) Error() string {
	switch {
	case e.report.Summary.Fail > 0:
		return fmt.Sprintf("自检发现 %d 项故障", e.report.Summary.Fail)
	case e.report.Summary.Warn > 0:
		return fmt.Sprintf("自检发现 %d 项需要处置", e.report.Summary.Warn)
	case e.report.Summary.NA > 0:
		// ⚠️ na = 无法判定（未配置端点）⇒ 不是故障，但也不能算通过。
		return fmt.Sprintf("自检未完成：%d 项无法判定（未配置端点）", e.report.Summary.NA)
	default:
		return "自检未完成"
	}
}

// fetchDiagnostics 取管理面诊断快照。
func fetchDiagnostics() (*doctorPayload, error) {
	if apiKey == "" {
		return nil, &doctorAuthError{Reason: "未设置 PB_API_KEY"}
	}
	ar, err := doReq("GET", "/admin/diagnostics", nil, "")
	if err != nil {
		return nil, &doctorAuthError{Reason: "取不到 /admin/diagnostics：" + err.Error()}
	}
	var payload doctorPayload
	if err := json.Unmarshal(ar.Data, &payload); err != nil {
		return nil, fmt.Errorf("诊断响应解析失败: %w", err)
	}
	return &payload, nil
}

// checksFromDiagnostics 把诊断快照翻译成检查结论。
func checksFromDiagnostics(p *doctorPayload) []doctorCheck {
	checks := []doctorCheck{
		{
			Name:   "服务版本",
			Status: "ok",
			Detail: p.Version,
		},
		{
			Name:   "部署模式",
			Status: "ok",
			Detail: p.Deployment,
		},
	}

	// 数据库
	db := doctorCheck{Name: "数据库"}
	switch {
	case p.Database.Reachable:
		db.Status = "ok"
		db.Detail = p.Database.Driver + " 可达"
	default:
		db.Status = "fail"
		db.Detail = "不可达" + detailSuffix(p.Database.Error)
		db.Advice = "检查数据目录权限与数据库配置；容器场景先看日志里的迁移报错"
	}
	checks = append(checks, db)

	// 存储
	st := doctorCheck{Name: "存储后端"}
	switch {
	case p.Storage.Degraded:
		st.Status = "fail"
		st.Detail = fmt.Sprintf("配置 %s，实际生效 %s（已降级）%s",
			p.Storage.Configured, p.Storage.Backend, detailSuffix(p.Storage.InitError))
		st.Advice = "远端存储构造失败 ⇒ 文件会全部落在本地盘；修好凭据/网络后重启"
	case p.Storage.Backend == "":
		st.Status = "warn"
		st.Detail = "未取到存储后端信息"
	default:
		st.Status = "ok"
		st.Detail = p.Storage.Backend
		if p.Storage.DataPath != "" {
			st.Detail += "（本地根目录 " + p.Storage.DataPath + "）"
		}
	}
	checks = append(checks, st)

	// Redis
	rd := doctorCheck{Name: "Redis"}
	if p.Redis.Mode == "redis" {
		rd.Status = "ok"
		rd.Detail = p.Redis.Address
	} else {
		rd.Status = "ok"
		rd.Detail = "单机内存模式（redis.host 未配置）"
	}
	checks = append(checks, rd)

	// 站点公网地址
	site := doctorCheck{Name: "站点公网地址"}
	if p.Site.PublicURLSet {
		site.Status = "ok"
		site.Detail = p.Site.PublicURL
	} else {
		site.Status = "warn"
		site.Detail = "未配置"
		site.Advice = "分享链接会按请求来源拼：内网部署会把内网 IP 发给外部用户 ⇒ 在管理台「站点配置」填公网地址"
	}
	checks = append(checks, site)

	// 首启状态
	su := doctorCheck{Name: "首启"}
	if p.Setup.Initialized {
		su.Status = "ok"
		su.Detail = "已初始化"
		if p.Setup.TokenRequired {
			su.Detail += "（首启令牌仍生效）"
		}
	} else {
		su.Status = "warn"
		su.Detail = "未初始化"
		if p.Setup.TokenRequired {
			su.Detail += fmt.Sprintf("（首启令牌生效，来源 %s；在启动日志里取）", p.Setup.TokenSource)
		} else {
			su.Detail += "（未启用首启令牌 ⇒ 任何能访问到的人都能抢先建号）"
		}
	}
	checks = append(checks, su)

	// 部署模式与**被降级的能力**（issue ⑧）
	checks = append(checks, degradedCapabilityCheck(p))

	// 时钟偏差（客户端时钟 vs 服务端时钟）
	checks = append(checks, clockCheck(p.ServerTime))

	// 新版本可用（2026-11-06，5.0.0）
	checks = append(checks, updateCheck(p))

	// 病毒扫描（core#121）
	checks = append(checks, clamavCheck(p))

	// 定时自动备份（2026-11-06，5.1.0）
	checks = append(checks, backupCheck(p, time.Now()))

	return checks
}

// updateCheck 「新版本可用」检查项。
//
// # 为什么状态是 ok 而不是 warn
//
// 「上游发了新版本」**不是本机的故障**。若用 warn，`pb doctor` 会返回退出码 1
// ⇒ CI / 巡检会因为「GitHub 上有个新 tag」而恒红，运维很快学会忽略这个信号
// （告警疲劳比没有告警更糟）。所以：有更新 = ok + detail 写明版本差 + advice
// 给升级入口；查不到 = ok + 说明「未检查」，绝不报失败。
//
// ⚠️ 这条与 §「离线不是故障」同一条原则：**「不知道」和「坏了」必须分开报**。
func updateCheck(p *doctorPayload) doctorCheck {
	c := doctorCheck{Name: "新版本"}

	if p.Update.Source == "disabled" {
		c.Status = "ok"
		c.Detail = "未启用（update.check_enabled=false）"
		return c
	}

	if p.Update.Source == "" || p.Update.Source == "unknown" || p.Update.Latest == "" {
		c.Status = "ok"
		c.Detail = "未检查到（离线 / 上游不可达 / 未启用）"
		if p.Update.Error != "" {
			c.Detail += "：" + p.Update.Error
		}
		return c
	}

	switch {
	case p.Update.Available && p.Update.Stale:
		// 有更新但数据来自过期缓存：版本差可信度降低 ⇒ 但仍不是故障。
		c.Status = "ok"
		c.Detail = fmt.Sprintf("可能有新版本 %s（当前 %s；数据取自过期缓存，未必最新）",
			p.Update.Latest, p.Update.Current)
	case p.Update.Available:
		c.Status = "ok"
		c.Detail = fmt.Sprintf("有新版本 %s（当前 %s）", p.Update.Latest, p.Update.Current)
		if p.Update.URL != "" {
			c.Advice = "升级说明 " + p.Update.URL
		} else {
			c.Advice = "见各平台升级说明（docs/UPGRADE.md）"
		}
	default:
		c.Status = "ok"
		c.Detail = fmt.Sprintf("已是最新（%s）", p.Update.Current)
	}
	return c
}

// clamavCheck 「病毒扫描」检查项（core#121）。
//
// # 为什么是 advisory（WARN 级、但不改退出码）
//
// 两个口径要同时成立：
//  1. 「静默失效」必须可见（issue 核心：fail-open 是对的，但没信号不对）
//     ⇒ 真 WARN 行（status=warn）；
//  2. 扫描器挂掉时业务照常放行（fail-open），**部署本身没有坏**
//     ⇒ 不该让 CI/巡检因「clamd 没起」恒红（与 updateCheck 的告警疲劳
//     同一条理由）⇒ Advisory=true ⇒ 退出码保持不变。
//
// 分级口径：
//   - 服务端未上报这段（旧版本）⇒ skip（未取到 ≠ 未启用）；
//   - 未启用 ⇒ ok（配置层面的正常选择）；
//   - 已启用但探活失败（degraded，fail-open 放行中）⇒ warn + advisory；
//   - 已启用、探活正常，但近 N 次扫描有 fail-open 放行 ⇒ warn + advisory
//     （扫描器已恢复，但期间有文件没被扫过）；
//   - 全好 ⇒ ok。
func clamavCheck(p *doctorPayload) doctorCheck {
	c := doctorCheck{Name: "病毒扫描"}
	m := p.Moderation
	if m == nil {
		c.Status = "skip"
		c.Detail = "服务端未上报（版本较旧）"
		return c
	}
	if !m.ClamAV.Enabled {
		c.Status = "ok"
		c.Detail = "未启用（moderation.clamav.enabled=false）"
		return c
	}
	if m.ClamAV.Degraded {
		c.Status = "warn"
		c.Advisory = true
		c.Detail = "扫描器不可达（当前放行）"
		if m.ClamAV.LastPing != nil {
			pingState := "fail"
			if m.ClamAV.LastPing.OK {
				pingState = "ok"
			}
			c.Detail += fmt.Sprintf("，最近探活 %s（%dms）", pingState, m.ClamAV.LastPing.DurationMs)
			if m.ClamAV.LastPing.Error != "" {
				c.Detail += "：" + m.ClamAV.LastPing.Error
			}
		}
		c.Advice = "检查 clamd 是否在跑、moderation.clamav.addr（" + m.ClamAV.Addr + "）是否可达；修复前文件不被扫描（fail-open 放行）"
		return c
	}
	if m.ClamAV.DegradedCount > 0 {
		c.Status = "warn"
		c.Advisory = true
		c.Detail = fmt.Sprintf("扫描器已恢复，但近 %d 次扫描有 %d 次因故障放行（fail-open）",
			m.ClamAV.ScansInWindow, m.ClamAV.DegradedCount)
		c.Advice = "这段时间的文件未被扫描；如需追溯，按该时间窗排查上传记录"
		return c
	}
	c.Status = "ok"
	c.Detail = "扫描器存活（clamd " + m.ClamAV.Addr + "）"
	return c
}

// degradedCapabilityCheck 「部署模式与被降级的能力」检查项（issue ⑧）。
//
// 数据来源：/admin/diagnostics 的 capabilities 段（服务端按**运行时真实状态**
// 算出的生效值，不是配置回显）。
//
// 分级口径：
//   - 存储降级（配置 s3、实际 local）⇒ **fail**：用户以为在用对象存储，
//     实际文件全落在本地盘上——这是真故障信号，不是配置缺失；
//   - 多副本 ⇒ 联邦**架构上不可用**（被 core 强制降级）⇒ warn，并说清原因；
//     直传（PigeonBeam）在多副本拓扑下同样不适用。
//
// ⚠️ 「联邦未启用」本身**不是**问题（配置层面的正常选择）⇒ ok，不告警。
func degradedCapabilityCheck(p *doctorPayload) doctorCheck {
	c := doctorCheck{Name: "被降级的能力"}
	caps := p.Capabilities

	mode := caps.DeploymentMode
	if mode == "" {
		mode = p.Deployment
	}
	multiReplica := mode == "public" || mode == "admin"

	// 存储降级是真故障信号（配置的后端 ≠ 生效后端）
	if caps.StorageDegraded {
		c.Status = "fail"
		c.Detail = fmt.Sprintf("存储已降级：配置 %s，实际生效 %s",
			p.Storage.Configured, effectiveStorageBackend(caps, p))
		c.Advice = "修好远端存储的凭据/网络后重启；在此之前文件会全部落在本地盘"
		return c
	}

	if multiReplica {
		c.Status = "warn"
		c.Detail = fmt.Sprintf("多副本部署（%s）：联邦已被服务端强制关闭（节点身份是进程级的，多副本语义未定义）", mode)
		c.Advice = "跨站投递不可用；直传（PigeonBeam）同样不适用于多副本拓扑"
		return c
	}

	if !caps.Federation {
		c.Status = "ok"
		c.Detail = "联邦未启用（配置层面）"
		return c
	}

	c.Status = "ok"
	c.Detail = "联邦已启用"
	return c
}

// effectiveStorageBackend 取生效的存储后端（能力位优先，回落到诊断段的存储信息）。
func effectiveStorageBackend(caps doctorCapabilities, p *doctorPayload) string {
	if caps.StorageBackend != "" {
		return caps.StorageBackend
	}
	return p.Storage.Backend
}

// checksFromPublic 降级检查：只做公开面能看出的事。
//
// configured = 端点是否**已配置**（--server / PB_SERVER / .env）。
// ⚠️ 它决定「连不上」时该判 fail 还是 na（issue #117）：
// 没配地址 ⇒ 连不上只是「不知道服务在哪」，**不是**故障。
func checksFromPublic(configured bool, probeErr error) []doctorCheck {
	checks := []doctorCheck{}
	// ⚠️ 用**匿名通路**（doPublicReq）而不是 doReq：
	//   doReq 在无 PB_API_KEY 时直接返回错误、根本不发请求
	//   ⇒ 这里永远拿不到数据 ⇒ 「服务可达」恒 FAIL
	//   ⇒ doctor 的结论变成「1 项故障」⇒ 用户以为应用坏了（真机实录：fnOS）。
	ar, err := doPublicReq("GET", "/api/config")
	if err != nil {
		// 区分两种失败：**服务不可达** vs **端点要鉴权**——
		// 后者说明服务其实是活的（回了 401/业务码），只是这个端点不再公开。
		// 三态（issue #117）：端点已配却连不上 ⇒ fail；没配 ⇒ na。
		// ⚠️ 「端点不可达」与「未配置端点」必须分开，否则运维会误判成「服务坏了」
		// （真机 10.10.10.3：后端端口不发布 ⇒ 默认地址没人监听 ⇒ 裸跑必假红）。
		status := reachabilityStatus(configured, false)
		detail := "端点不可达：GET /api/config 失败：" + err.Error()
		advice := "确认端点地址与服务是否在跑（--server / PB_SERVER / .env 的 PB_API_PORT）"
		if probeErr != nil {
			// 候选探测已经逐个试过了 ⇒ 用探测结论（比二次请求的报错更准：
			// 能区分「连不上」与「连上了但不是 PigeonBox API（TLS 端口明文打过去）
			// ⇒ 后者是**地址口径错**，不是服务坏了）。
			detail = "端点不可达：" + probeErr.Error()
			advice = "确认端点地址与协议（--server / PB_SERVER / .env 的 PB_FRONTEND_PORT、PB_API_PORT）；TLS 端口要用 https://"
		}
		if !configured {
			// 没配端点 ⇒ 这次「连不上」只说明**地址是猜的**（内置默认地址），
			// 不能据此说服务坏了 ⇒ 判 na（无法判定）。
			detail = "未配置端点：默认地址 " + serverURL + " 不可达（" + err.Error() + "）"
			advice = "用 --server <url> 指定真实端点；或在部署目录写 .env（含 PB_SERVER 或 PB_API_PORT）"
		}
		if strings.Contains(err.Error(), "code 10002") || strings.Contains(err.Error(), "未认证") {
			status = "warn"
			detail = "GET /api/config 返回未认证（HTTP 鉴权拦截）"
			advice = "服务可达；该端点需鉴权，请设置 admin 的 PB_API_KEY 后重跑以做深度检查"
		}
		checks = append(checks, doctorCheck{
			Name:   "服务可达",
			Status: status,
			Detail: detail,
			Advice: advice,
		})
		return checks
	}
	var p doctorPublicPayload
	if err := json.Unmarshal(ar.Data, &p); err != nil {
		checks = append(checks, doctorCheck{
			Name:   "服务可达",
			Status: "warn",
			Detail: "/api/config 响应解析失败：" + err.Error(),
		})
		return checks
	}
	// 探到了：端点可用。
	// ⚠️ 若端点**未配置**，那这只是「默认地址恰好能用」，不等于「部署端点已确认」
	// ⇒ 结论写清来源，别让人以为配置是好的（issue #117）。
	reachDetail := serverURL
	if !configured {
		reachDetail = serverURL + "（默认地址恰好可达；端点未显式配置）"
	}
	checks = append(checks, doctorCheck{Name: "服务可达", Status: "ok", Detail: reachDetail})
	if p.Capabilities.DeploymentMode != "" {
		checks = append(checks, doctorCheck{
			Name:   "部署模式",
			Status: "ok",
			Detail: p.Capabilities.DeploymentMode,
		})
	}
	if !p.Capabilities.PublicURLSet {
		checks = append(checks, doctorCheck{
			Name:   "站点公网地址",
			Status: "warn",
			Detail: "未配置",
			Advice: "分享链接会按请求来源拼：内网部署会把内网 IP 发给外部用户",
		})
	}
	if p.Capabilities.SetupTokenRequired {
		checks = append(checks, doctorCheck{
			Name:   "首启令牌",
			Status: "ok",
			Detail: "生效（未初始化实例的 /setup 需要令牌）",
		})
	}
	return checks
}

// clockCheck 时钟偏差检查（客户端时钟 vs 服务端上报时间）。
//
// 为什么值得查：令牌有效期、时间窗下载令牌、分享过期判定都吃本地时钟；
// 客户端与服务器差太多 ⇒ 「明明没过期却提示过期」这类玄学问题。
func clockCheck(serverTime string) doctorCheck {
	c := doctorCheck{Name: "时钟偏差"}
	t, err := time.Parse(time.RFC3339, serverTime)
	if err != nil {
		c.Status = "skip"
		c.Detail = "服务端未上报时间"
		return c
	}
	skew := time.Since(t)
	if skew < 0 {
		skew = -skew
	}
	switch {
	case skew > 5*time.Minute:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("与服务端相差 %s（超过 5 分钟）", skew.Round(time.Second))
		c.Advice = "校准本机时钟（NTP）；否则令牌有效期与分享过期判定会漂"
	case skew > 10*time.Second:
		c.Status = "warn"
		c.Detail = fmt.Sprintf("与服务端相差 %s", skew.Round(time.Second))
	default:
		c.Status = "ok"
		c.Detail = fmt.Sprintf("与服务端相差 %s", skew.Round(time.Millisecond))
	}
	return c
}

// summarize 统计各状态条数。
func summarize(checks []doctorCheck) doctorSummary {
	s := doctorSummary{Total: len(checks)}
	for _, c := range checks {
		switch c.Status {
		case "ok":
			s.OK++
		case "warn":
			if c.Advisory {
				// advisory：显示 WARN 但不参与退出码（core#121）。
				s.Note++
			} else {
				s.Warn++
			}
		case "fail":
			s.Fail++
		case "na":
			// ⚠️ na 单列，**不并进 Skip 也不并进 Fail**
			// （issue #117）：并进 Fail 会把「没配地址」说成「服务坏了」。
			s.NA++
		default:
			s.Skip++
		}
	}
	return s
}

// printDoctorReport 人读结论输出。
func printDoctorReport(r doctorReport) {
	fmt.Fprintf(stdout, "PigeonBox 部署自检 → %s\n", r.Server)
	// 来源标注（issue #117）：让运维知道 doctor 到底读了哪份配置。
	// ⚠️ 只打来源标签，**不打密钥明文**。
	if r.ServerSource != "" {
		fmt.Fprintf(stdout, "端点来源：%s\n", r.ServerSource)
	}
	if r.APIKeySource != "" {
		fmt.Fprintf(stdout, "凭据来源：%s\n", r.APIKeySource)
	} else {
		fmt.Fprintln(stdout, "凭据来源：未配置（未设置 PB_API_KEY / --api-key-file）")
	}
	if r.ProbeError != "" {
		// 候选端点全试过了都没通 ⇒ 把结论摆出来（issue #117：别让人猜）。
		fmt.Fprintf(stdout, "端点探测：%s\n", r.ProbeError)
	}
	if !r.Deep {
		fmt.Fprintln(stdout, "（未取到管理面数据：以下为公开面检查，深度项缺失）")
	}
	fmt.Fprintln(stdout)
	for _, c := range r.Checks {
		fmt.Fprintf(stdout, "[%-4s] %-12s %s\n", statusLabel(c.Status), c.Name, c.Detail)
		if c.Advice != "" {
			fmt.Fprintf(stdout, "       └─ %s\n", c.Advice)
		}
	}
	fmt.Fprintln(stdout)
	// 结论行：故障 / 需处置 / **未配置**（na）/ 正常。
	// ⚠️ na 单列一句——它是「无法判定」，不是「故障」也不是「正常」
	// （issue #117）。na==0 时**保持既有文案不变**（老脚本按「全部正常」判成功）。
	naPart := ""
	if r.Summary.NA > 0 {
		naPart = fmt.Sprintf("%d 项未配置", r.Summary.NA)
	}
	// 提示级（advisory，core#121）：显示但不改退出码 ⇒ 只作后缀，
	// 且 Note==0 时结论行文案与既有输出逐字一致（老脚本按它判成功）。
	noteSuffix := ""
	if r.Summary.Note > 0 {
		noteSuffix = fmt.Sprintf("（另有 %d 项提示）", r.Summary.Note)
	}
	switch {
	case r.Summary.Fail > 0:
		if naPart != "" {
			fmt.Fprintf(stdout, "结论：%d 项故障、%d 项需处置、%s、%d 项正常%s\n",
				r.Summary.Fail, r.Summary.Warn, naPart, r.Summary.OK, noteSuffix)
		} else {
			fmt.Fprintf(stdout, "结论：%d 项故障、%d 项需处置、%d 项正常%s\n",
				r.Summary.Fail, r.Summary.Warn, r.Summary.OK, noteSuffix)
		}
	case r.Summary.Warn > 0:
		if naPart != "" {
			fmt.Fprintf(stdout, "结论：%d 项需处置、%s、%d 项正常%s\n",
				r.Summary.Warn, naPart, r.Summary.OK, noteSuffix)
		} else {
			fmt.Fprintf(stdout, "结论：%d 项需处置、%d 项正常%s\n", r.Summary.Warn, r.Summary.OK, noteSuffix)
		}
	case r.Summary.NA > 0:
		fmt.Fprintf(stdout, "结论：%s、%d 项正常（%d 项无法判定）%s\n",
			naPart, r.Summary.OK, r.Summary.NA, noteSuffix)
	default:
		fmt.Fprintf(stdout, "结论：全部正常（%d 项）%s\n", r.Summary.OK, noteSuffix)
	}
}

// statusLabel 状态标签（等宽对齐用）。
func statusLabel(status string) string {
	switch status {
	case "ok":
		return "OK"
	case "warn":
		return "WARN"
	case "fail":
		return "FAIL"
	case "na":
		// ⚠️ 独立标签：**不是** FAIL（issue #117）——
		// 「没配端点」与「端点不可达」必须一眼能分开。
		return "NA"
	default:
		return "SKIP"
	}
}

// detailSuffix 拼错误详情后缀（空则不拼）。
func detailSuffix(msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return ""
	}
	return "：" + msg
}

// backupCheck 「定时自动备份」检查项（2026-11-06，5.1.0）。
//
// # 为什么这条要能报 warn
//
// 与「有新版本」不同：**备份配好了却没跑起来**是本机的真故障，而且它是最容易
// 被忽略的那一类——没备份的日子一切正常，直到需要恢复的那天。所以这里对
// 「启用了但一份都没有」与「最近一份超过 48 小时」都报 warn（退出码 1）。
//
// 反过来，**未启用**只报 ok + 建议：默认关闭是产品决定（写盘位置由部署形态
// 决定），不该让没开的人每次巡检都见到红色。
func backupCheck(p *doctorPayload, now time.Time) doctorCheck {
	c := doctorCheck{Name: "备份"}
	b := p.Backup

	if !b.Enabled {
		c.Status = "ok"
		c.Detail = "定时备份未启用"
		c.Advice = "建议开启（PB_BACKUP_ENABLED=true）：默认每天 " + b.At + " 落一份到 " + b.Dir + "，保留 " + itoa(b.Keep) + " 份"
		return c
	}

	if b.LastError != "" {
		c.Status = "warn"
		c.Detail = "上次备份失败：" + b.LastError
		c.Advice = "检查备份目录可写与磁盘余量：" + b.Dir
		return c
	}

	if b.Count == 0 || b.LastBackupAt == "" {
		c.Status = "warn"
		c.Detail = fmt.Sprintf("已启用但还没有任何备份（计划每天 %s，目录 %s）", b.At, b.Dir)
		c.Advice = "在管理台「维护」页点「立即备份」先跑一次；或等下一个计划时刻"
		return c
	}

	ts, err := time.Parse(time.RFC3339, b.LastBackupAt)
	if err != nil {
		c.Status = "ok"
		c.Detail = fmt.Sprintf("最近备份 %s（共 %d 份 / %s）", b.LastBackupAt, b.Count, humanBytes(b.TotalBytes))
		return c
	}

	age := now.Sub(ts)
	c.Status = "ok"
	c.Detail = fmt.Sprintf("最近备份 %s（%s前，共 %d 份 / %s）",
		ts.Format("2006-01-02 15:04"), humanAge(age), b.Count, humanBytes(b.TotalBytes))
	if b.Running {
		c.Detail += "；正在备份"
	}
	// 48h 而不是 24h：一天一份的节奏下，一次失败 + 一次重试会合法地跨过 24h，
	// 用 24h 会制造假红。48h 意味着「整整一天都没有新包」——那时候确实该看。
	if age > 48*time.Hour {
		c.Status = "warn"
		c.Detail = fmt.Sprintf("最近一份备份已是 %s前（%s）；计划每天 %s ⇒ 定时任务没有在跑",
			humanAge(age), ts.Format("2006-01-02 15:04"), b.At)
		c.Advice = "查启动日志的 auto backup enabled / auto backup failed；或在管理台点「立即备份」"
	}
	return c
}

// humanBytes 人类可读的字节数（诊断输出用）。
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	value := float64(n)
	i := 0
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d %s", n, units[i])
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// humanAge 人类可读的时长（只到天/小时，够判断备份新鲜度）。
func humanAge(d time.Duration) string {
	if d < 0 {
		return "0 分钟"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}

// itoa 小整数转字符串（避免为一个拼接额外引入 strconv 调用点）。
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
