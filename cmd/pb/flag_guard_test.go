package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLI 旗标缺值守卫（2026-10-26 审计 P1）。
//
// ## 实测事故
//
//	pb put -t "机密文本" -p
//
// 原实现把每个取值分支写成 `if i+1 < len(args) { ... }` ⇒ **旗标缺值时
// 整个旗标被静默吞掉** ⇒ 密码丢失 ⇒ 服务端按 require_auth=false 建出
// **公开**分享。用户以为自己加了密码，实际任何人都能取件。
//
// 为什么这条最危险：它不是"命令写错了"，而是**命令成功返回**、服务端
// 也成功建了分享 —— 唯一的差别是安全语义被静默**放宽**（共享范围变大）。
// 静默降级比报错危险：用户没有任何信号。
//
// 本守卫覆盖：
//   ① 四个取值旗标（-t/-e/-p/-c）缺值 ⇒ 必须是**用法错误**（退出码 2），
//      且**不得发出任何请求**（不能先建出分享再报缺值）；
//   ② 未知旗标 ⇒ 用法错误；
//   ③ 正常路径（旗标有值、旗标可写在位置参数之后）不受影响。

// TestFlagMissingValueIsUsageError —— 缺值必须报错，且不得发请求。
func TestFlagMissingValueIsUsageError(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"文本分享缺密码", []string{"-t", "机密文本", "-p"}},
		{"文本分享缺文本", []string{"-t"}},
		{"文件分享缺有效期", []string{"-p", "pw", "-e"}},
		{"文件分享缺自定义码", []string{"-c"}},
		{"仅 -p", []string{"-p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 请求计数：缺值时**一个请求都不能发**（不能先建分享再报错）
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
			}))
			defer srv.Close()
			useTestServer(t, srv)

			_, _, err := parsePutFlags(tc.args)
			if err == nil {
				t.Fatalf("旗标缺值必须报错，实际 err=nil（静默吞掉 ⇒ 密码等语义被悄悄放宽）")
			}
			if !isUsageErr(err) {
				t.Fatalf("缺值应是用法错误（退出码 2），实际 %T: %v", err, err)
			}
			if calls != 0 {
				t.Fatalf("缺值时不得发出请求，实际发了 %d 次（先建分享再报错 ⇒ 用户仍拿到一个宽松权限的分享）", calls)
			}
		})
	}
}

// TestFlagMissingValueDoesNotDowngradeToPublic —— 核心防线：
// `pb put -t "机密" -p` 绝不能建出**公开**分享。
func TestFlagMissingValueDoesNotDowngradeToPublic(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	if err := cmdPut([]string{"-t", "机密文本", "-p"}); err == nil {
		t.Fatalf("cmdPut 应因 -p 缺值而失败，实际成功")
	}
	if _, ok := fields["require_auth"]; ok {
		t.Fatalf("缺值时不得发出任何上传请求（已捕获表单 %v）", fields)
	}
}

// TestUnknownFlagIsUsageError —— 未知旗标是用法错误（退出码 2），
// 且不得被当成文件名吞掉。
func TestUnknownFlagIsUsageError(t *testing.T) {
	_, _, err := parsePutFlags([]string{"--encrypt"})
	if err == nil {
		t.Fatalf("未知旗标必须报错")
	}
	if !isUsageErr(err) {
		t.Fatalf("未知旗标应是用法错误（退出码 2），实际 %T: %v", err, err)
	}
}

// TestFlagsAfterPositionalStillWork —— 防止"修缺值"时把既有用法改坏：
// 旗标写在位置参数**之后**仍要生效（标准库 flag 在位置参数处会停止解析，
// 故本项目保留手写解析器）。
func TestFlagsAfterPositionalStillWork(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, files, err := parsePutFlags([]string{p, "-e", "7d", "-p", "pw123"})
	if err != nil {
		t.Fatalf("旗标在位置参数之后应仍可解析: %v", err)
	}
	if len(files) != 1 || files[0] != p {
		t.Fatalf("位置参数解析错误: %v", files)
	}
	if f.expire != "7d" {
		t.Fatalf("旗标在位置参数之后应生效，expire=%q", f.expire)
	}
	if f.password != "pw123" {
		t.Fatalf("旗标在位置参数之后应生效，password=%q", f.password)
	}
}

// TestP2PPushCodeFromEnv —— 联邦推送口令改走环境变量 PB_P2P_CODE
// （命令行参数会进 ps 与 shell 历史 ⇒ 明文暴露）。
func TestP2PPushCodeFromEnv(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	t.Setenv("PB_P2P_CODE", "s3cr3t-code")

	// 口令取自 env ⇒ argv 里只给目标地址与附言
	if err := p2pCmd([]string{"push", "http://peer.example", "来自 A 站"}); err != nil {
		t.Fatalf("p2p push 应成功: %v", err)
	}
	if got, _ := payload["code"].(string); got != "s3cr3t-code" {
		t.Fatalf("口令应取自 PB_P2P_CODE，实际 %q（body=%v）", got, payload)
	}
	if got, _ := payload["target_url"].(string); got != "http://peer.example" {
		t.Fatalf("target_url 不对: %q", got)
	}
	if got, _ := payload["note"].(string); got != "来自 A 站" {
		t.Fatalf("附言不对: %q", got)
	}
}

// TestP2PPushCodeFromArgvStillWorks —— argv 形态保留兼容，但必须打提示
// （不静默接受明文口令）。
func TestP2PPushCodeFromArgvStillWorks(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":{}}`))
	}))
	defer srv.Close()
	useTestServer(t, srv)

	t.Setenv("PB_P2P_CODE", "")

	var buf strings.Builder
	oldErr := stderr
	stderr = &buf
	t.Cleanup(func() { stderr = oldErr })

	if err := p2pCmd([]string{"push", "http://peer.example", "argv-code", "附言"}); err != nil {
		t.Fatalf("p2p push 应成功: %v", err)
	}
	if got, _ := payload["code"].(string); got != "argv-code" {
		t.Fatalf("argv 形态应仍可用，实际 code=%q", got)
	}
	if got, _ := payload["note"].(string); got != "附言" {
		t.Fatalf("附言不对: %q", got)
	}
	if !strings.Contains(buf.String(), "PB_P2P_CODE") {
		t.Fatalf("argv 明文口令必须打提示（stderr=%q）", buf.String())
	}
}

// isUsageErr 判定是否用法错误（退出码 2）。
func isUsageErr(err error) bool {
	var ue *usageErr
	return err != nil && errors.As(err, &ue)
}
