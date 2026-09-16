// taskswitch_api.go 定时任务开关的 HTTP 端点：查看 / 切换四类排程任务。
//
// 路由（看板 /api/* 下，走 Basic Auth，与账号管理、Key 管理同口径）：
//
//	GET  /schedule           列出四类任务的开关与时点
//	POST /schedule/{key}     切换某类任务，body {"enabled":true|false}
//
// 设计要点：
//   - **运行时立即生效**：切换直接改调度器的内存开关并唤醒其休眠（见
//     scheduler.SetTaskEnabled 的 wake 机制），不必重启进程。
//   - **同时写回 config.json**（可写时）：否则重启后回到文件里的旧值，
//     用户会以为"开关没生效/被重置"。写盘失败时如实回报，内存仍已生效。
//   - 只切"开关"，**不动时点小时**：与 config 的 *_enabled / *_hours 语义一致
//     （禁用保留 hours，重新启用即恢复原时点）。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// scheduleList 返回四类任务的开关与时点。
func (h *Handler) scheduleList(w http.ResponseWriter, r *http.Request) {
	if h.cfg.TaskSwitches == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "scheduler not enabled"})
		return
	}
	items := h.cfg.TaskSwitches()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"tasks":  items,
		"config": h.cfg.SetupConfigPath, // 前端可提示"开关会写回哪个文件"
	})
}

// scheduleSet 切换一类任务的启用状态。
//
// POST /schedule/{key}   body {"enabled":bool}
func (h *Handler) scheduleSet(w http.ResponseWriter, r *http.Request) {
	if h.cfg.SetTaskEnabled == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "scheduler not enabled"})
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_key", "缺少任务标识")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", `请求体需为 {"enabled":true|false}`)
		return
	}
	if !h.cfg.SetTaskEnabled(key, *body.Enabled) {
		writeOpenAIError(w, http.StatusBadRequest, "unknown_task",
			"未知任务标识（可选：checkin / travel / activity / keepalive）")
		return
	}

	// 写回 config.json：不写则重启后回到文件里的旧值，用户会以为开关没生效。
	// 内存已改完（上面那步），故写盘失败不回滚——但要如实告知。
	persisted, persistErr := true, ""
	if h.cfg.PersistScheduleSwitch == nil {
		persisted, persistErr = false, "未提供 config 路径，仅内存生效（重启后回到文件里的值）"
	} else if err := h.cfg.PersistScheduleSwitch(key, *body.Enabled); err != nil {
		persisted, persistErr = false, err.Error()
	}

	state := "禁用"
	if *body.Enabled {
		state = "启用"
	}
	log.Printf("[schedule] 已%s任务 %s（persisted=%v）", state, key, persisted)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"key":     key,
		"enabled": *body.Enabled,
		// 内存已立即生效，故 applied 恒 true；persisted 表示是否也落盘。
		"applied":       true,
		"persisted":     persisted,
		"persist_error": persistErr,
	})
}
