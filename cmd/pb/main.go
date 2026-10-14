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
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	serverURL string
	apiKey    string
)

func main() {
	serverURL = strings.TrimRight(envOr("PB_SERVER", "http://localhost:12345"), "/")
	apiKey = os.Getenv("PB_API_KEY")

	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "put":
		err = cmdPut(rest)
	case "import":
		err = cmdImport(rest)
	case "ls":
		err = cmdList()
	case "url":
		err = cmdURL(rest)
	case "p2p":
		p2pCmd(rest)
	case "rm":
		err = cmdDelete(rest)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`pb —— PigeonBox CLI

环境变量:
  PB_SERVER   服务地址（默认 http://localhost:12345）
  PB_API_KEY  用户级 API Key（用户中心 → API 令牌 创建；必需）

命令:
  put [flags] <file...>   上传文件（多文件合并为一个分享）
    -t <text>             分享文本（与文件互斥）
    -e <expire>           有效期: 30m/1h/7d/2w/3M/1y/forever（默认 1d）
    -p <password>         取件密码
    -c <code>             自定义取件码（登录用户）
    --encrypt             端到端加密（实验性，仅文本）
  import [flags] <path>   服务器本地文件导入（免上传，NAS 场景）
    -e / -p / -c 同 put
  ls                      我的分享列表
  url <code>              获取分享下载直链
  rm <code>               删除分享

设备直传(admin API Key;2026-10-14):
  p2p status              直传状态(启用/p2pc/注册中心/收件区)
  p2p tasks               直传任务列表
  p2p send <path...>      发起发送(服务器路径,须在白名单内)→ 输出口令
  p2p recv <口令>          凭口令接收(落收件暂存区)
  p2p cancel <task_id>    取消任务
  p2p invites             待收邀请列表
  p2p accept <invite_id>  认领邀请并接收
  p2p push <目标地址> <口令> [附言]   推送口令到另一实例(免口令交接)
`)
}

// ===== p2p 设备直传(2026-10-14;admin 用户的 API Key,服务端 AdminOrAdminKey 复核角色) =====

func p2pCmd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "✗ 缺少 p2p 子命令(status/tasks/send/recv/cancel/invites/accept/push)")
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	var err error
	switch sub {
	case "status":
		err = p2pGet("/admin/p2p/status")
	case "tasks":
		err = p2pGet("/admin/p2p/tasks")
	case "invites":
		err = p2pGet("/admin/p2p/invites")
	case "send":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "✗ p2p send 需要至少一个服务器路径")
			os.Exit(2)
		}
		err = p2pPost("/admin/p2p/send", map[string]any{"paths": rest})
	case "recv":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "✗ p2p recv 需要口令")
			os.Exit(2)
		}
		err = p2pPost("/admin/p2p/recv", map[string]any{"code": rest[0]})
	case "cancel":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "✗ p2p cancel 需要 task_id")
			os.Exit(2)
		}
		err = p2pPost("/admin/p2p/cancel", map[string]any{"task_id": rest[0]})
	case "accept":
		if len(rest) == 0 {
			fmt.Fprintln(os.Stderr, "✗ p2p accept 需要 invite_id")
			os.Exit(2)
		}
		err = p2pPost("/admin/p2p/invites/accept", map[string]any{"invite_id": rest[0]})
	case "push":
		if len(rest) < 2 {
			fmt.Fprintln(os.Stderr, "✗ p2p push 需要 <目标实例地址> <口令> [附言]")
			os.Exit(2)
		}
		err = p2pPost("/admin/p2p/invite-push", map[string]any{
			"target_url": rest[0], "code": rest[1],
			"note": strings.Join(rest[2:], " "), "from_name": "",
		})
	default:
		fmt.Fprintf(os.Stderr, "✗ 未知 p2p 子命令: %s\n", sub)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", err)
		os.Exit(1)
	}
}

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
		fmt.Println("ok")
		return nil
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		fmt.Println(string(raw))
		return nil
	}
	fmt.Println(buf.String())
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
		return &ar, fmt.Errorf("%s", ar.Message)
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

func parsePutFlags(args []string) (*putFlags, []string) {
	f := &putFlags{expire: "1d"}
	var files []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-t":
			if i+1 < len(args) {
				i++
				f.text = args[i]
			}
		case "-e":
			if i+1 < len(args) {
				i++
				f.expire = args[i]
			}
		case "-p":
			if i+1 < len(args) {
				i++
				f.password = args[i]
			}
		case "-c":
			if i+1 < len(args) {
				i++
				f.code = args[i]
			}
		default:
			files = append(files, args[i])
		}
	}
	return f, files
}

func cmdPut(args []string) error {
	f, files := parsePutFlags(args)
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
		if f.password != "" {
			_ = writer.WriteField("require_auth", "true")
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
		return fmt.Errorf("请指定文件或用 -t 分享文本")
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
	if f.password != "" {
		_ = writer.WriteField("require_auth", "true")
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
	if f.password != "" {
		_ = writer.WriteField("require_auth", "true")
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
	f, files := parsePutFlags(args)
	if len(files) != 1 {
		return fmt.Errorf("import 需要恰好一个服务器本地路径")
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
	if f.password != "" {
		payload["require_auth"] = true
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
	fmt.Printf("共 %d 条分享：\n", data.Total)
	for _, it := range data.Items {
		name := it.FileName
		if name == "" {
			name = it.Text
		}
		fmt.Printf("  %-10s %8d 次取件  %10d B  %s\n", it.Code, it.Used, it.Size, truncate(name, 48))
	}
	return nil
}

func cmdURL(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: pb url <code>")
	}
	code := args[0]
	fmt.Printf("%s/#/share/%s\n", serverURL, code)
	fmt.Printf("直链（下载令牌需重新取件获取）: %s/share/download?code=%s\n", serverURL, code)
	return nil
}

func cmdDelete(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: pb rm <code>")
	}
	_, err := doReq("DELETE", "/api/v1/user/shares/"+args[0]+"/hard", nil, "")
	if err != nil {
		return err
	}
	fmt.Println("已删除")
	return nil
}

// ===== 输出 =====

func printShare(data json.RawMessage) error {
	var d struct {
		Code string `json:"code"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}
	fmt.Printf("取件码: %s\n分享链接: %s/#/share/%s\n", d.Code, serverURL, d.Code)
	if d.URL != "" {
		fmt.Printf("服务端链接: %s\n", d.URL)
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
