package main

// pb storage migrate 的行为断言（2026-10-30）。
//
// 守卫点：
//  1. 请求打到管理面端点 POST /admin/storage/migrate，**body 形状**要对
//     （target_type / target_path / dry_run）；
//  2. 缺 --to / 未知参数 ⇒ 用法错误（退出码 2），**不得发请求**；
//  3. 服务端返回失败摘要时，命令要把结论打出来（WARN 打在 stdout 上）。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// migrateServer 捕获迁移请求并回放指定响应。
func migrateServer(t *testing.T, resp string) (*httptest.Server, *[]byte, *string) {
	t.Helper()
	var gotBody []byte
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv, &gotBody, &gotPath
}

// captureStorage 跑一次 storage 命令，捕获 stdout。
func captureStorage(t *testing.T, args []string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &buf, &buf
	defer func() { stdout, stderr = oldOut, oldErr }()
	err := storageCmd(args)
	return buf.String(), err
}

func TestStorageMigrate_PostsToAdminEndpoint(t *testing.T) {
	srv, gotBody, gotPath := migrateServer(t,
		`{"code":0,"message":"ok","data":{"switched":true,"copied":3,"skipped":1,"failed":0,"bytes":2048,"objects":4,"from_type":"local","to_type":"s3","message":"已迁移 3 个对象"}}`)
	oldServer, oldKey := serverURL, apiKey
	serverURL = srv.URL
	apiKey = "pb_sk_admin"
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	out, err := captureStorage(t, []string{"migrate", "--to", "s3"})
	if err != nil {
		t.Fatalf("迁移命令不应报错: %v", err)
	}
	if *gotPath != "/admin/storage/migrate" {
		t.Fatalf("端点不对: %s", *gotPath)
	}
	var payload map[string]any
	if err := json.Unmarshal(*gotBody, &payload); err != nil {
		t.Fatalf("body 不是 JSON: %v (%s)", err, string(*gotBody))
	}
	if payload["target_type"] != "s3" {
		t.Errorf("target_type 不对: %v", payload["target_type"])
	}
	if _, ok := payload["dry_run"]; ok {
		t.Errorf("非 dry-run 不该带 dry_run 字段: %v", payload)
	}
	if _, ok := payload["target_path"]; ok {
		t.Errorf("未指定 --path 不该带 target_path: %v", payload)
	}
	if !bytes.Contains([]byte(out), []byte("复制 3")) || !bytes.Contains([]byte(out), []byte("后端已切换: true")) {
		t.Errorf("输出应含迁移结论: %s", out)
	}
}

func TestStorageMigrate_DryRunAndPath(t *testing.T) {
	srv, gotBody, _ := migrateServer(t,
		`{"code":0,"message":"ok","data":{"switched":false,"copied":0,"skipped":0,"failed":0,"bytes":0,"objects":7,"from_type":"local","to_type":"local"}}`)
	oldServer, oldKey := serverURL, apiKey
	serverURL = srv.URL
	apiKey = "pb_sk_admin"
	t.Cleanup(func() { serverURL, apiKey = oldServer, oldKey })

	out, err := captureStorage(t, []string{"migrate", "--to", "local", "--path", "/data/new", "--dry-run"})
	if err != nil {
		t.Fatalf("dry-run 不应报错: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(*gotBody, &payload); err != nil {
		t.Fatalf("body 不是 JSON: %v", err)
	}
	if payload["target_path"] != "/data/new" {
		t.Errorf("target_path 不对: %v", payload["target_path"])
	}
	if payload["dry_run"] != true {
		t.Errorf("dry_run 不对: %v", payload["dry_run"])
	}
	if !bytes.Contains([]byte(out), []byte("计划")) {
		t.Errorf("dry-run 输出应是「计划」: %s", out)
	}
}

func TestStorageMigrate_UsageErrors(t *testing.T) {
	cases := [][]string{
		{"migrate"},                          // 缺 --to
		{"migrate", "--to"},                  // --to 缺取值
		{"migrate", "--to", "s3", "--bogus"}, // 未知参数
		{"migrate", "--to", "s3", "--path"},  // --path 缺取值
	}
	for _, args := range cases {
		_, err := captureStorage(t, args)
		var ue *usageErr
		if err == nil {
			t.Errorf("参数 %v 应报用法错误", args)
			continue
		}
		if !asUsageErr(err, &ue) {
			t.Errorf("参数 %v 报的不是用法错误: %v", args, err)
		}
	}
}

func TestStorageCmd_UnknownSubcommand(t *testing.T) {
	_, err := captureStorage(t, []string{"nope"})
	var ue *usageErr
	if err == nil || !asUsageErr(err, &ue) {
		t.Fatalf("未知子命令应是用法错误: %v", err)
	}
}

// asUsageErr 判定是否为用法错误（errors.As 的小包装，测试里更好读）。
func asUsageErr(err error, target **usageErr) bool {
	for err != nil {
		if ue, ok := err.(*usageErr); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
