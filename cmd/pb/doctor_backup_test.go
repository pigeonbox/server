package main

// 「定时自动备份」检查项的断言（2026-11-06，5.1.0）。
//
// 这一项的判据不是「能不能列出文件」，而是**该红的红、不该红的不红**：
//   · 配好了却一份都没有 / 最近一份隔了一整天 ⇒ 必须 warn（退出码 1），
//     因为「没备份」在平时完全无症状，是最容易被忽略的真故障；
//   · 未启用 ⇒ 只 ok + 建议（默认关闭是产品决定，不该让人每次巡检见红）。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// diagWithBackup 在健康快照上挂一段 backup。
func diagWithBackup(now time.Time, backup string) string {
	base := strings.TrimSpace(healthyDiagnostics(now))
	trimmed := strings.TrimSpace(strings.TrimSuffix(base, "}"))
	return trimmed + ",\"backup\":" + backup + "}"
}

// TestDoctor_BackupDisabledIsNotAnError 未启用 ⇒ ok（退出码 0）。
func TestDoctor_BackupDisabledIsNotAnError(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithBackup(now, `{"enabled":false,"at":"03:00","keep":7,"dir":"/data/backups"}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("未启用备份不是故障（应退出码 0）: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "未启用") {
		t.Errorf("应写明未启用：\n%s", out)
	}
	if !strings.Contains(out, "PB_BACKUP_ENABLED") {
		t.Errorf("应给出开启方式（建议而不是空结论）：\n%s", out)
	}
}

// TestDoctor_BackupEnabledButNone 启用但零份 ⇒ warn（这是最危险的静默状态）。
func TestDoctor_BackupEnabledButNone(t *testing.T) {
	now := time.Now()
	srv := doctorServer(t, diagWithBackup(now,
		`{"enabled":true,"at":"03:00","keep":7,"dir":"/data/backups","count":0,"last_backup_at":""}`), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("启用备份却一份都没有，必须报 warn（退出码 1）:\n%s", out)
	}
	if !strings.Contains(out, "还没有任何备份") {
		t.Errorf("应写明「一份都没有」：\n%s", out)
	}
}

// TestDoctor_BackupFresh 最近备份很新 ⇒ ok。
func TestDoctor_BackupFresh(t *testing.T) {
	now := time.Now()
	recent := now.Add(-2 * time.Hour).Format(time.RFC3339)
	payload := `{"enabled":true,"at":"03:00","keep":7,"dir":"/data/backups",` +
		`"count":3,"total_bytes":3145728,"last_backup_at":"` + recent + `"}`
	srv := doctorServer(t, diagWithBackup(now, payload), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err != nil {
		t.Fatalf("备份新鲜时不得报红: %v\n输出:\n%s", err, out)
	}
	if !strings.Contains(out, "3.0 MB") {
		t.Errorf("应给出总占用（人类可读）：\n%s", out)
	}
}

// TestDoctor_BackupStale 最近一份超过 48h ⇒ warn（定时任务没在跑）。
func TestDoctor_BackupStale(t *testing.T) {
	now := time.Now()
	stale := now.Add(-72 * time.Hour).Format(time.RFC3339)
	payload := `{"enabled":true,"at":"03:00","keep":7,"dir":"/data/backups",` +
		`"count":1,"total_bytes":1048576,"last_backup_at":"` + stale + `"}`
	srv := doctorServer(t, diagWithBackup(now, payload), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("最近一份 3 天前的备份必须报 warn:\n%s", out)
	}
	if !strings.Contains(out, "定时任务没有在跑") {
		t.Errorf("应指出是定时任务没跑，而不是笼统的「备份过期」：\n%s", out)
	}
}

// TestDoctor_BackupFailure 上次失败 ⇒ warn，并把原因原样带出来。
func TestDoctor_BackupFailure(t *testing.T) {
	now := time.Now()
	payload := `{"enabled":true,"at":"03:00","keep":7,"dir":"/data/backups",` +
		`"count":2,"last_error":"导出备份失败: no space left on device"}`
	srv := doctorServer(t, diagWithBackup(now, payload), `{}`)
	useTestServer(t, srv)

	out, err := captureDoctor(t, []string{})
	if err == nil {
		t.Fatalf("上次备份失败必须报 warn:\n%s", out)
	}
	if !strings.Contains(out, "no space left on device") {
		t.Errorf("必须带出真实原因（否则无法排查）：\n%s", out)
	}
}

// TestDoctor_BackupPayloadShape backup 段必须能从真实 JSON 里解出（形状回归锁）。
func TestDoctor_BackupPayloadShape(t *testing.T) {
	raw := `{"enabled":true,"dir":"/data/backups","keep":7,"at":"03:00","count":2,` +
		`"total_bytes":100,"last_backup_at":"2026-11-06T03:00:05+08:00",` +
		`"last_file":"pigeonbox-backup-20261106-030005.tar.gz","last_size":100,` +
		`"running":false,"last_error":""}`
	var p doctorPayload
	if err := json.Unmarshal([]byte(raw), &p.Backup); err != nil {
		t.Fatalf("backup 段形状不符: %v", err)
	}
	if !p.Backup.Enabled || p.Backup.Count != 2 || p.Backup.Keep != 7 {
		t.Fatalf("字段没解出来: %+v", p.Backup)
	}
	if p.Backup.LastFile != "pigeonbox-backup-20261106-030005.tar.gz" {
		t.Fatalf("last_file 没解出来: %+v", p.Backup)
	}
}
