// pb —— PigeonBox 命令行客户端（P3）。
//
// 面向脚本化/命令行场景的官方 CLI：文本/文件分享、本地文件导入（NAS 场景）、
// 我的分享管理与直链获取。认证复用用户级 API Key（X-API-Key），零额外依赖（stdlib only）。
//
// 用法：
//
//	export PB_SERVER=http://10.0.0.2:12345
//	export PB_API_KEY=pb_sk_xxx            # 用户中心 → API 令牌 创建
//
//	pb put report.pdf                        # 上传文件 → 输出取件码/直链
//	pb put -e 7d -p secret a.mp4 b.mp4      # 多文件合并为一个分享，7 天，密码保护
//	pb put -t "hello"                        # 文本分享
//	pb import /nas/photos/img.jpg            # 服务器本地文件导入（需启用 local_import）
//	pb ls                                    # 我的分享列表
//	pb url <code>                            # 取分享下载直链
//	pb rm <code>                             # 删除分享
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oekit/pkg/cli"
)

var (
	serverURL string
	apiKey    string
)

// stdout/stderr 走可注入 writer：命令分发层（oekit/pkg/cli）要求输出流由宿主
// 给出，命令实现才能被单元测试直接调用（不必起子进程、不必捕获 os.Stdout）。
// 进程入口把它们绑到真实标准流。
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

func main() {
	serverURL = strings.TrimRight(envOr("PB_SERVER", "http://localhost:12345"), "/")
	apiKey = os.Getenv("PB_API_KEY")
	os.Exit(app().Run(context.Background(), os.Args[1:], stdout, stderr))
}

// app 组装命令表。退出码语义由 oekit/pkg/cli 统一（0 成功 / 1 运行失败 /
// 2 用法错误），与旧的手写 switch 一致。
func app() *cli.App {
	run := func(fn func([]string) error) func(context.Context, []string, io.Writer, io.Writer) int {
		return func(_ context.Context, args []string, out, errOut io.Writer) int {
			// 命令实现内的输出走包级 stdout/stderr，绑定到本次调用的流——
			// 单测传入 buffer 即可断言输出，不必捕获进程级 stdout。
			oldOut, oldErr := stdout, stderr
			stdout, stderr = out, errOut
			defer func() { stdout, stderr = oldOut, oldErr }()

			err := fn(args)
			if err == nil {
				return cli.ExitOK
			}
			fmt.Fprintf(errOut, "错误: %v\n", err)
			// 用法错误(2)与运行失败(1)分级：脚本可据此区分「命令写错了」与「服务端拒绝了」
			var ue *usageErr
			if errors.As(err, &ue) {
				return cli.ExitUsage
			}
			return cli.ExitError
		}
	}
	return &cli.App{
		Name: "pb",
		Commands: []cli.Command{
			{Name: "put", Summary: "上传文件（多文件合并为一个分享）", Run: run(cmdPut)},
			{Name: "import", Summary: "服务器本地文件导入（免上传，NAS 场景）", Run: run(cmdImport)},
			{Name: "ls", Summary: "我的分享列表", Run: run(func([]string) error { return cmdList() })},
			{Name: "url", Summary: "取分享链接与下载直链", Run: run(cmdURL)},
			{Name: "rm", Summary: "删除分享", Run: run(cmdDelete)},
			{Name: "p2p", Summary: "联邦（设备名录/待收邀请/推送）", Run: run(p2pCmd)},
			{Name: "doctor", Summary: "部署自检（DB/存储/Redis/版本/首启状态）", Run: run(cmdDoctor)},
			{Name: "help", Summary: "显示用法", Run: func(_ context.Context, _ []string, out, _ io.Writer) int {
				usage(out)
				return cli.ExitOK
			}},
		},
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `pb —— PigeonBox CLI

环境变量:
  PB_SERVER   服务地址（默认 http://localhost:12345）
  PB_API_KEY  用户级 API Key（用户中心 → API 令牌 创建；必需）

命令:
  put [flags] <file...>   上传文件（多文件合并为一个分享）
    -t <text>             分享文本（与文件互斥）
    -e <expire>           有效期: 30m/1h/7d/2w/3M/1y/forever（默认 1d）
    -p <password>         取件密码
    -c <code>             自定义取件码（登录用户）
  import [flags] <path>   服务器本地文件导入（免上传，NAS 场景）
    -e / -p / -c 同 put
  ls                      我的分享列表
  url [-p <password>] <code>   取分享链接与下载直链（直链含短时效令牌，随取随用勿缓存）
                           -p 取件密码（经 X-Share-Password 头传，不入 URL）
  rm <code>               删除分享
  doctor [--json]         部署自检：DB/存储/Redis/版本/站点地址/首启状态
                           --json 输出机器可读结论(CI/巡检用);
                           退出码 0=健康 1=有待处置或故障 2=用法错误
                           (深度检查需 admin 用户的 API Key;否则降级为公开面检查)

联邦(admin API Key;2026-10-14):
  p2p peers               在线设备名录(node_id 用于定向邀请)
  p2p invites             待收邀请列表
  p2p accept <invite_id>  认领邀请并接收
  p2p push <目标地址> [附言]   推送口令到另一实例(免口令交接)
                           口令取自 PB_P2P_CODE 环境变量(推荐);
                           也可写成 <目标地址> <口令> [附言],但会打提示
                           (命令行参数会进 ps 与 shell 历史)

设备直传已独立为 PigeonBeam(信鸽直传)应用——本 CLI 不再提供直传子命令。
`)
}

// ===== p2p 联邦(2026-10-14;admin 用户的 API Key,服务端 AdminOrAdminKey 复核角色) =====
//
// 设备直传已拆为独立应用 PigeonBeam(信鸽直传):本 CLI 只保留联邦面
// (设备名录/待收邀请/认领/推送),直传子命令(status/tasks/send/recv/cancel)已移除。

func p2pCmd(args []string) error {
	if len(args) == 0 {
		return errUsage("缺少 p2p 子命令(peers/invites/accept/push)")
	}
	sub, rest := args[0], args[1:]
	var err error
	switch sub {
	case "peers":
		err = p2pGet("/admin/p2p/directory")
	case "invites":
		err = p2pGet("/admin/p2p/invites")
	case "accept":
		if len(rest) == 0 {
			return errUsage("p2p accept 需要 invite_id")
		}
		err = p2pPost("/admin/p2p/invites/accept", map[string]any{"invite_id": rest[0]})
	case "push":
		// 口令优先取环境变量 PB_P2P_CODE（2026-10-26 审计 P2）：
		// 写在命令行参数里的口令会进 `ps` 输出与 shell 历史 ⇒ 明文暴露。
		// p2pc 侧早已因同一原因改成 `recv -` + PB_P2P_CODE env。
		// argv 形态保留兼容，但会打提示（不静默接受明文 argv）。
		if code := strings.TrimSpace(os.Getenv("PB_P2P_CODE")); code != "" {
			if len(rest) < 1 {
				return errUsage("p2p push 需要 <目标实例地址> [附言]（口令取自 PB_P2P_CODE 环境变量）")
			}
			err = p2pPost("/admin/p2p/invite-push", map[string]any{
				"target_url": rest[0], "code": code,
				"note": strings.Join(rest[1:], " "), "from_name": "",
			})
			break
		}
		if len(rest) < 2 {
			return errUsage("p2p push 需要 <目标实例地址> <口令> [附言]（口令建议改用 PB_P2P_CODE 环境变量传入）")
		}
		fmt.Fprintln(stderr, "提示：口令写在命令行参数里会被 ps 与 shell 历史看到；建议改用 PB_P2P_CODE 环境变量")
		err = p2pPost("/admin/p2p/invite-push", map[string]any{
			"target_url": rest[0], "code": rest[1],
			"note": strings.Join(rest[2:], " "), "from_name": "",
		})
	default:
		return errUsage("未知 p2p 子命令: " + sub)
	}
	return err
}

// errUsage 用法错误：退出码 2（与运行失败 1 分级）。
type usageErr struct{ msg string }

func (e *usageErr) Error() string { return e.msg }

func errUsage(msg string) error { return &usageErr{msg: msg} }

func p2pGet(path string) error {
	ar, err := doReq("GET", path, nil, "")
	if err != nil {
		return err
	}
	return prettyPrint(ar.Data)
}

func p2pPost(path string, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	ar, err := doReq("POST", path, bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	return prettyPrint(ar.Data)
}

func prettyPrint(raw json.RawMessage) error {
	if len(raw) == 0 {
		fmt.Fprintln(stdout, "ok")
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		fmt.Fprintln(stdout, string(raw))
		return nil
	}
	fmt.Fprintln(stdout, buf.String())
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ===== 过期参数 =====

// parseExpire 简写 → (expire_value, expire_style)
func parseExpire(s string) (int, string, error) {
	if s == "forever" || s == "" {
		return 1, "forever", nil
	}
	unit := s[len(s)-1:]
	numStr := s[:len(s)-1]
	var num int
	if _, err := fmt.Sscanf(numStr, "%d", &num); err != nil || num <= 0 {
		return 0, "", fmt.Errorf("无效有效期 %q（示例: 30m/1h/7d/2w/3M/1y/forever）", s)
	}
	switch unit {
	case "m":
		return num, "minute", nil
	case "h":
		return num, "hour", nil
	case "d":
		return num, "day", nil
	case "w":
		return num, "week", nil
	case "M":
		return num, "month", nil
	case "y":
		return num, "year", nil
	default:
		return 0, "", fmt.Errorf("无效有效期单位 %q", unit)
	}
}

// ===== HTTP 基础 =====

type apiResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func doReq(method, path string, body io.Reader, contentType string) (*apiResp, error) {
	return doReqWithHeaders(method, path, body, contentType, nil)
}

// doReqWithHeaders 发请求（可带附加头）。取件密码经 X-Share-Password 头
// 传递（2026-10-16 拍板）：query 形态有访问日志/浏览器历史/Referer 泄漏面。
func doReqWithHeaders(method, path string, body io.Reader, contentType string, headers map[string]string) (*apiResp, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("未设置 PB_API_KEY（用户中心 → API 令牌 创建）")
	}
	req, err := http.NewRequest(method, serverURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", apiKey)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var ar apiResp
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, fmt.Errorf("响应解析失败 (HTTP %d): %w", resp.StatusCode, err)
	}
	ok := ar.Code == 200 || ar.Code == 0
	if !ok {
		// 带上业务码：集成者/用户按 code 排查（错误码对照表见文档站 error-codes 页）
		return &ar, fmt.Errorf("[code %d] %s", ar.Code, ar.Message)
	}
	return &ar, nil
}

// ===== put =====

type putFlags struct {
	text     string
	expire   string
	password string
	code     string
}

// parsePutFlags 解析 put/import 的旗标与位置参数。
//
// ## 旗标缺值必须报错（2026-10-26 审计 P1）
//
// 原实现每个取值分支写成 `if i+1 < len(args) { i++; f.x = args[i] }`
// ⇒ **旗标缺值时整个旗标被静默吞掉**：
//
//	pb put -t "机密文本" -p   ⇒ 密码被丢 ⇒ 服务端按 require_auth=false 建出
//	                          **公开**分享 ⇒ 用户以为自己加了密码，实际人人可取。
//
// 这类「静默降级」比报错危险得多：安全语义被悄悄**放宽**（共享范围变大），
// 而命令返回成功 ⇒ 用户毫无察觉。⇒ 缺值一律当**用法错误**（退出码 2）。
//
// ## 为什么不用标准库 flag / oekit cli.ParseFlags
//
// 标准库 flag 遇到第一个**位置参数就停止解析**（`pb put a.txt -e 7d` 里
// `-e 7d` 会被当成文件名）⇒ 会破坏「旗标可写在文件之后」的既有用法。
// 故保留手写解析器，但把「缺值/未知旗标」从静默吞掉改成硬错误。
func parsePutFlags(args []string) (*putFlags, []string, error) {
	f := &putFlags{expire: "1d"}
	var files []string
	// needValue 取旗标取值；缺值 ⇒ 用法错误（不静默丢）。
	needValue := func(name string, i int) (string, int, error) {
		if i+1 >= len(args) {
			return "", i, errUsage("旗标 " + name + " 缺少取值")
		}
		return args[i+1], i + 1, nil
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			v, ni, err := needValue("-t", i)
			if err != nil {
				return nil, nil, err
			}
			f.text = v
			i = ni
		case "-e":
			v, ni, err := needValue("-e", i)
			if err != nil {
				return nil, nil, err
			}
			f.expire = v
			i = ni
		case "-p":
			v, ni, err := needValue("-p", i)
			if err != nil {
				return nil, nil, err
			}
			f.password = v
			i = ni
		case "-c":
			v, ni, err := needValue("-c", i)
			if err != nil {
				return nil, nil, err
			}
			f.code = v
			i = ni
		default:
			// 未知旗标报错而非当文件名吞掉（曾致 --encrypt 报 open --encrypt: no such file）
			if len(args[i]) > 1 && strings.HasPrefix(args[i], "-") {
				return nil, nil, errUsage("未知参数: " + args[i] + "（见 pb help）")
			}
			files = append(files, args[i])
		}
	}
	return f, files, nil
}

func cmdPut(args []string) error {
	f, files, err := parsePutFlags(args)
	if err != nil {
		return err
	}
	expVal, expStyle, err := parseExpire(f.expire)
	if err != nil {
		return err
	}

	// 文本分享
	if f.text != "" {
		form := &bytes.Buffer{}
		writer := multipart.NewWriter(form)
		_ = writer.WriteField("text", f.text)
		_ = writer.WriteField("expire_value", fmt.Sprint(expVal))
		_ = writer.WriteField("expire_style", expStyle)
		// require_auth 契约必填（false 也要显式发）——见下方同名注释。
		_ = writer.WriteField("require_auth", boolStr(f.password != ""))
		if f.password != "" {
			_ = writer.WriteField("password", f.password)
		}
		if f.code != "" {
			_ = writer.WriteField("custom_code", f.code)
		}
		_ = writer.Close()
		ar, err := doReq("POST", "/share/text/", form, writer.FormDataContentType())
		if err != nil {
			return err
		}
		return printShare(ar.Data)
	}

	if len(files) == 0 {
		return errUsage("请指定文件或用 -t 分享文本")
	}

	// 单文件直传；多文件合并（multi-direct）
	if len(files) == 1 {
		return putDirect(files[0], expVal, expStyle, f)
	}
	form := &bytes.Buffer{}
	writer := multipart.NewWriter(form)
	for _, p := range files {
		fh, err := os.Open(p)
		if err != nil {
			return err
		}
		part, err := writer.CreateFormFile("files", filepath.Base(p))
		if err != nil {
			_ = fh.Close()
			return err
		}
		if _, err := io.Copy(part, fh); err != nil {
			_ = fh.Close()
			return err
		}
		_ = fh.Close()
	}
	_ = writer.WriteField("expire_value", fmt.Sprint(expVal))
	_ = writer.WriteField("expire_style", expStyle)
	// require_auth 是**契约必填**（false 也必须显式发送）——条件省略 ⇒ 公开分享
	// 直接被后端拒成 10001（前端早已修过同一类，2026-10-24 CLI 实测补齐）。
	_ = writer.WriteField("require_auth", boolStr(f.password != ""))
	if f.password != "" {
		_ = writer.WriteField("password", f.password)
	}
	if f.code != "" {
		_ = writer.WriteField("custom_code", f.code)
	}
	_ = writer.Close()
	ar, err := doReq("POST", "/api/v1/share/multi-direct", form, writer.FormDataContentType())
	if err != nil {
		return err
	}
	return printShare(ar.Data)
}

func putDirect(path string, expVal int, expStyle string, f *putFlags) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()

	form := &bytes.Buffer{}
	writer := multipart.NewWriter(form)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, fh); err != nil {
		return err
	}
	_ = writer.WriteField("expire_value", fmt.Sprint(expVal))
	_ = writer.WriteField("expire_style", expStyle)
	// require_auth 是**契约必填**（false 也必须显式发送）——条件省略 ⇒ 公开分享
	// 直接被后端拒成 10001（前端早已修过同一类，2026-10-24 CLI 实测补齐）。
	_ = writer.WriteField("require_auth", boolStr(f.password != ""))
	if f.password != "" {
		_ = writer.WriteField("password", f.password)
	}
	if f.code != "" {
		_ = writer.WriteField("custom_code", f.code)
	}
	_ = writer.Close()

	ar, err := doReq("POST", "/share/file/", form, writer.FormDataContentType())
	if err != nil {
		return err
	}
	return printShare(ar.Data)
}

// ===== import（服务器本地文件导入）=====

func cmdImport(args []string) error {
	f, files, err := parsePutFlags(args)
	if err != nil {
		return err
	}
	if len(files) != 1 {
		return errUsage("import 需要恰好一个服务器本地路径")
	}
	expVal, expStyle, err := parseExpire(f.expire)
	if err != nil {
		return err
	}
	payload := map[string]interface{}{
		"path":         files[0],
		"expire_value": expVal,
		"expire_style": expStyle,
	}
	// require_auth 契约必填（false 也要显式发）——同上。
	payload["require_auth"] = f.password != ""
	if f.password != "" {
		payload["password"] = f.password
	}
	if f.code != "" {
		payload["custom_code"] = f.code
	}
	body, _ := json.Marshal(payload)
	ar, err := doReq("POST", "/api/v1/user/shares/import-local", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	return printShare(ar.Data)
}

// ===== ls / url / rm =====

func cmdList() error {
	ar, err := doReq("GET", "/api/v1/user/shares?page=1&page_size=50", nil, "")
	if err != nil {
		return err
	}
	var data struct {
		Items []struct {
			Code     string `json:"code"`
			FileName string `json:"file_name"`
			Text     string `json:"text"`
			Size     int64  `json:"size"`
			Used     int    `json:"used_count"`
		} `json:"items"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(ar.Data, &data); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "共 %d 条分享：\n", data.Total)
	for _, it := range data.Items {
		name := it.FileName
		if name == "" {
			name = it.Text
		}
		fmt.Fprintf(stdout, "  %-10s %8d 次取件  %10d B  %s\n", it.Code, it.Used, it.Size, truncate(name, 48))
	}
	return nil
}

func cmdURL(args []string) error {
	// pb url [-p <password>] <code>
	// 密码保护分享的取件密码经 X-Share-Password 头传（与网页/前端同口径）。
	password, code, err := parseURLArgs(args)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s/#/share/%s\n", serverURL, code)
	// 直链经 /share/select/ 取签名 download_url（旧版直接拼 code 的 URL 缺下载令牌必 401）
	var headers map[string]string
	if password != "" {
		headers = map[string]string{"X-Share-Password": password}
	}
	ar, err := doReqWithHeaders("GET", "/share/select/?code="+url.QueryEscape(code), nil, "", headers)
	if err != nil {
		return err
	}
	var d struct {
		DownloadURL string `json:"download_url"`
		Text        string `json:"text"`
	}
	if err := json.Unmarshal(ar.Data, &d); err != nil {
		return fmt.Errorf("响应解析失败: %w", err)
	}
	if d.DownloadURL == "" {
		fmt.Fprintln(stdout, "(文本/受保护分享无匿名直链，取件见上方链接)")
		return nil
	}
	fmt.Fprintf(stdout, "直链（令牌短时效，随取随用勿缓存）: %s%s\n", serverURL, d.DownloadURL)
	return nil
}

// parseURLArgs 解析 pb url 的参数：[-p <password>] <code>。
func parseURLArgs(args []string) (password, code string, err error) {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case (args[i] == "-p" || args[i] == "--password") && i+1 < len(args):
			i++
			password = args[i]
		case strings.HasPrefix(args[i], "--password="):
			password = strings.SplitN(args[i], "=", 2)[1]
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) != 1 {
		return "", "", errUsage("用法: pb url [-p <password>] <code>")
	}
	return password, rest[0], nil
}

func cmdDelete(args []string) error {
	if len(args) != 1 {
		return errUsage("用法: pb rm <code>")
	}
	// 软删（进回收站，可在用户中心恢复）——**不能**直连硬删端点：
	// DELETE /api/v1/user/shares/:code/hard 只接受**已软删**的分享
	// （2026-10-24 实测：正常分享打硬删一律 10001「分享不在回收站中」⇒
	//  CLI 的删除命令形同虚设）。
	// 单条软删没有独立路由，走批量软删端点带一个 code（与前端同形状）。
	b, _ := json.Marshal(map[string]any{"codes": []string{args[0]}})
	ar, err := doReq("POST", "/api/v1/user/shares/batch-delete", bytes.NewReader(b), "application/json")
	if err != nil {
		return err
	}
	_ = ar
	fmt.Fprintln(stdout, "已移入回收站（可在用户中心恢复）")
	return nil
}

// ===== 输出 =====

func printShare(data json.RawMessage) error {
	var d struct {
		Code       string `json:"code"`
		PickupCode string `json:"pickup_code"`
		URL        string `json:"url"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	if d.PickupCode != "" {
		fmt.Fprintf(stdout, "取件码（6 位）: %s\n", d.PickupCode)
	}
	fmt.Fprintf(stdout, "分享码: %s\n分享链接: %s/#/share/%s\n", d.Code, serverURL, d.Code)
	if d.URL != "" {
		fmt.Fprintf(stdout, "服务端链接: %s\n", d.URL)
	}
	return nil
}

// boolStr 布尔 → 字符串（multipart 表单字段用；契约里 require_auth 是字符串型必填）。
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
