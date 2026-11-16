package main

// 诊断包导出（hub#385）——本地优先的一半：
//
//	pb doctor --export <路径|->   写出 {schema, exported_at, report, diagnostics}
//
// # 为什么是「信封」而不是只倒 --json 的 report
//
// report 是 doctor 的**结论**（检查项/建议），而维护者复盘时需要服务端诊断的
// **原始事实**（版本/平台/架构/schema 版本/DB/存储/Redis/更新状态）。
// 两者放在同一个 JSON 里，离线（无外网）也能被脚本解析：
//
//	reports = [json.loads(open(p).read())["diagnostics"] for p in files]
//
// # 隐私边界
//
// 导出包**由用户主动生成并主动交给维护者**，因此包含站点对外地址等诊断事实
// （与「默认关闭、只发七个字段」的匿名上报不同）。字段清单见
// docs/DIAGNOSTICS.md。

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

// doctorExportSchema 诊断包格式版本（信封结构变化时 +1）。
const doctorExportSchema = "1"

// doctorExport 诊断包信封：结论 + 原始事实。
type doctorExport struct {
	Schema      string         `json:"schema"`
	ExportedAt  time.Time      `json:"exported_at"`
	Report      doctorReport   `json:"report"`
	Diagnostics *doctorPayload `json:"diagnostics,omitempty"`
}

// writeDoctorExport 写诊断包；path 为 "-" 时写 stdout。
func writeDoctorExport(path string, report doctorReport, payload *doctorPayload) error {
	env := doctorExport{
		Schema:      doctorExportSchema,
		ExportedAt:  time.Now().UTC(),
		Report:      report,
		Diagnostics: payload,
	}
	var w io.Writer
	var f *os.File
	if path == "-" {
		w = stdout
	} else {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("创建诊断包失败: %w", err)
		}
		f = file
		w = file
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		if f != nil {
			_ = f.Close()
		}
		return fmt.Errorf("写诊断包失败: %w", err)
	}
	if f != nil {
		return f.Close()
	}
	return nil
}
