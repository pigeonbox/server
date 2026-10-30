// storage.go —— pb storage 子命令（2026-10-30：存储迁移）。
//
// # 为什么要有
//
// 换后端 / 换机器 /「NAS 小盘迁对象存储」此前只能**直接切后端**
// ⇒ 切完之前的历史对象留在旧后端 ⇒ 旧分享取不到文件。
// `pb storage migrate` 把存量对象先搬过去，**搬完才切换**，且：
//   · 取件码与分享链接**不变**（DB 记录不动）；
//   · 有任何对象失败 ⇒ **不切换**，源后端继续在用；
//   · 可中断可续（目标已存在的对象跳过）。
//
// # 用法
//
//	pb storage migrate --to s3                  # 迁移到 s3（用管理台已配置的凭据）
//	pb storage migrate --to local --path /data  # 搬本地目录（换存储根）
//	pb storage migrate --to s3 --dry-run        # 只看计划（对象数/字节数/源→目标）
//
// 需要 **admin 用户的 API Key**（管理面端点）。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// storageCmd 存储相关子命令。
func storageCmd(args []string) error {
	if len(args) == 0 {
		return errUsage("缺少 storage 子命令(migrate)")
	}
	switch args[0] {
	case "migrate":
		return storageMigrate(args[1:])
	default:
		return errUsage("未知 storage 子命令: " + args[0])
	}
}

// storageMigrate 执行存储迁移（搬存量对象 → 全成功后切换后端）。
func storageMigrate(args []string) error {
	var to, path string
	dryRun := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--to":
			if i+1 >= len(args) {
				return errUsage("--to 缺少取值")
			}
			to = args[i+1]
			i++
		case "--path":
			if i+1 >= len(args) {
				return errUsage("--path 缺少取值")
			}
			path = args[i+1]
			i++
		case "--dry-run":
			dryRun = true
		default:
			return errUsage("未知参数: " + args[i] + "（pb storage migrate 只支持 --to/--path/--dry-run）")
		}
	}
	if strings.TrimSpace(to) == "" {
		return errUsage("必须用 --to 指定目标后端类型（如 s3 / local / webdav）")
	}

	payload := map[string]any{"target_type": to}
	if path != "" {
		payload["target_path"] = path
	}
	if dryRun {
		payload["dry_run"] = true
	}
	body, _ := json.Marshal(payload)
	ar, err := doReq("POST", "/admin/storage/migrate", bytes.NewReader(body), "application/json")
	if err != nil {
		return err
	}
	var data struct {
		Switched bool   `json:"switched"`
		Copied   int64  `json:"copied"`
		Skipped  int64  `json:"skipped"`
		Failed   int64  `json:"failed"`
		Bytes    int64  `json:"bytes"`
		Objects  int64  `json:"objects"`
		FromType string `json:"from_type"`
		ToType   string `json:"to_type"`
		Message  string `json:"message"`
	}
	if err := json.Unmarshal(ar.Data, &data); err != nil {
		return fmt.Errorf("响应解析失败: %w", err)
	}
	if dryRun {
		fmt.Fprintf(stdout, "计划：%d 个对象，%s → %s\n", data.Objects, data.FromType, data.ToType)
	} else {
		fmt.Fprintf(stdout, "迁移完成：复制 %d、跳过 %d、失败 %d，%d 字节；%s → %s\n",
			data.Copied, data.Skipped, data.Failed, data.Bytes, data.FromType, data.ToType)
		fmt.Fprintf(stdout, "后端已切换: %v\n", data.Switched)
	}
	if data.Message != "" {
		fmt.Fprintln(stdout, data.Message)
	}
	return nil
}
