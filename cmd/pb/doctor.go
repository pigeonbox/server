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
	"strings"
	"time"
)

// doctorCheck 一条检查结论。
type doctorCheck struct {
	// Name 检查项名（人读的短名）。
	Name string `json:"name"`
	// Status ok | warn | fail | skip
	Status string `json:"status"`
	// Detail 证据/取值。
	Detail string `json:"detail"`
	// Advice 下一步（可为空）。
	Advice string `json:"advice,omitempty"`
}

// doctorReport 一次自检的完整结论。
type doctorReport struct {
	Server  string        `json:"server"`
	Checks  []doctorCheck `json:"checks"`
	Summary doctorSummary `json:"summary"`
	// Deep true = 执行了深度检查（拿得到 /admin/diagnostics）。
	Deep bool `json:"deep"`
}

// doctorSummary 结论统计。
type doctorSummary struct {
	OK    int `json:"ok"`
	Warn  int `json:"warn"`
	Fail  int `json:"fail"`
	Skip  int `json:"skip"`
	Total int `json:"total"`
}

// status 严重度排序：fail > warn > ok（skip 不参与）。
func (s doctorSummary) worst() string {
	switch {
	case s.Fail > 0:
		return "fail"
	case s.Warn > 0:
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

// cmdDoctor 执行部署自检。
//
// 用法：
//
//	pb doctor            人读结论
//	pb doctor --json     机器读（JSON）
func cmdDoctor(args []string) error {
	jsonOut := false
	for _, a := range args {
		switch a {
		case "--json":
			jsonOut = true
		default:
			return errUsage("未知参数: " + a + "（pb doctor 只支持 --json）")
		}
	}

	report := doctorReport{Server: serverURL}

	// 深度检查：管理面诊断端点（需要 admin API Key）。
	deep, err := fetchDiagnostics()
	if err == nil && deep != nil {
		report.Deep = true
		report.Checks = append(report.Checks, checksFromDiagnostics(deep)...)
	} else {
		// 拿不到深度数据 ⇒ 降级为公开面检查，并**明确**说明未做深度检查。
		reason := "未设置 PB_API_KEY"
		if apiKey != "" {
			reason = "需要 admin 用户的 API Key（当前 Key 无管理面权限或服务端不可达）"
			if apiErr, ok := err.(*doctorAuthError); ok {
				reason = apiErr.Reason
			}
		}
		report.Checks = append(report.Checks, doctorCheck{
			Name:   "深度检查",
			Status: "warn",
			Detail: "未执行（" + reason + "）",
			Advice: "在用户中心用管理员账号创建 API Key，设置 PB_API_KEY 后重跑",
		})
		report.Checks = append(report.Checks, checksFromPublic()...)
	}

	report.Summary = summarize(report.Checks)

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

	return checks
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
func checksFromPublic() []doctorCheck {
	checks := []doctorCheck{}
	ar, err := doReq("GET", "/api/config", nil, "")
	if err != nil {
		checks = append(checks, doctorCheck{
			Name:   "服务可达",
			Status: "fail",
			Detail: "GET /api/config 失败：" + err.Error(),
			Advice: "确认 PB_SERVER 地址与服务是否在跑",
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
	checks = append(checks, doctorCheck{Name: "服务可达", Status: "ok", Detail: serverURL})
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
			s.Warn++
		case "fail":
			s.Fail++
		default:
			s.Skip++
		}
	}
	return s
}

// printDoctorReport 人读结论输出。
func printDoctorReport(r doctorReport) {
	fmt.Fprintf(stdout, "PigeonBox 部署自检 → %s\n", r.Server)
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
	switch {
	case r.Summary.Fail > 0:
		fmt.Fprintf(stdout, "结论：%d 项故障、%d 项需处置、%d 项正常\n",
			r.Summary.Fail, r.Summary.Warn, r.Summary.OK)
	case r.Summary.Warn > 0:
		fmt.Fprintf(stdout, "结论：%d 项需处置、%d 项正常\n", r.Summary.Warn, r.Summary.OK)
	default:
		fmt.Fprintf(stdout, "结论：全部正常（%d 项）\n", r.Summary.OK)
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
