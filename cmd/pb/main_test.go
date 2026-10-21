package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oekit/pkg/cli"
)

// 分发层退出码契约（oekit/pkg/cli 语义：0 成功 / 1 运行失败 / 2 用法错误）。
// 这层此前是手写 switch + os.Exit，无法被单测覆盖——命令表拼错、退出码
// 分级错位都只能靠人肉发现。
func TestAppDispatchExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"无参数", nil, cli.ExitUsage},
		{"未知子命令", []string{"nope"}, cli.ExitUsage},
		{"help", []string{"help"}, cli.ExitOK},
		{"p2p 缺子命令", []string{"p2p"}, cli.ExitUsage},
		{"p2p 未知子命令", []string{"p2p", "nope"}, cli.ExitUsage},
		{"url 缺 code", []string{"url"}, cli.ExitUsage},
		{"url 多余参数", []string{"url", "a", "b"}, cli.ExitUsage},
		{"rm 缺 code", []string{"rm"}, cli.ExitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			got := app().Run(context.Background(), c.args, &out, &errOut)
			if got != c.want {
				t.Errorf("退出码 = %d, 期望 %d (stderr: %s)", got, c.want, errOut.String())
			}
		})
	}
}

// 取件密码必须走 X-Share-Password 头，不进 URL——query 形态有访问日志/
// 浏览器历史/Referer 泄漏面（2026-10-16 拍板，与 core v0.24.0 同批收敛）。
func TestURLPasswordViaHeader(t *testing.T) {
	var gotPath, gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotHeader = r.Header.Get("X-Share-Password")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"download_url":"/share/download?code=abc&token=t","text":""}}`))
	}))
	defer srv.Close()

	oldServer, oldKey := serverURL, apiKey
	serverURL, apiKey = srv.URL, "pb_sk_test"
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	var out, errOut bytes.Buffer
	if code := app().Run(context.Background(), []string{"url", "-p", "s3cret", "abc123"}, &out, &errOut); code != cli.ExitOK {
		t.Fatalf("退出码 = %d, 期望 0 (stderr: %s)", code, errOut.String())
	}
	if gotHeader != "s3cret" {
		t.Errorf("X-Share-Password 头 = %q, 期望 %q", gotHeader, "s3cret")
	}
	if strings.Contains(gotPath, "password") {
		t.Errorf("URL 里出现了 password: %s", gotPath)
	}
	if !strings.Contains(gotPath, "code=abc123") {
		t.Errorf("URL 缺 code: %s", gotPath)
	}
	if !strings.Contains(out.String(), "token=t") {
		t.Errorf("输出未含下载直链: %s", out.String())
	}
}

// 未给密码时不得带空头（服务端按"无密码"判定，多余的空头会被当成一次口令尝试）。
func TestURLWithoutPasswordNoHeader(t *testing.T) {
	var hadHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hadHeader = r.Header["X-Share-Password"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"download_url":"/share/download?code=abc&token=t"}}`))
	}))
	defer srv.Close()

	oldServer, oldKey := serverURL, apiKey
	serverURL, apiKey = srv.URL, "pb_sk_test"
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	var out, errOut bytes.Buffer
	if code := app().Run(context.Background(), []string{"url", "abc123"}, &out, &errOut); code != cli.ExitOK {
		t.Fatalf("退出码 = %d, 期望 0 (stderr: %s)", code, errOut.String())
	}
	if hadHeader {
		t.Error("未给密码时不应发送 X-Share-Password 头")
	}
}

// 缺 PB_API_KEY 属运行失败（1），不是用法错误（2）——脚本可据此区分。
func TestURLMissingAPIKeyIsRuntimeError(t *testing.T) {
	oldKey := apiKey
	apiKey = ""
	t.Cleanup(func() { apiKey = oldKey })

	var out, errOut bytes.Buffer
	if code := app().Run(context.Background(), []string{"url", "abc123"}, &out, &errOut); code != cli.ExitError {
		t.Errorf("退出码 = %d, 期望 %d", code, cli.ExitError)
	}
}
