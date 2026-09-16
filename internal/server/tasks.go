// tasks.go 任务中心 HTTP 接口：扫描 / 单账号一键完成 / 执行队列 / 队列状态 / 单账号任务列表。
package server

import (
	"encoding/json"
	"net/http"

	"workbuddy2api/internal/panel"
)

// tasksScan 扫描全部账号任务（只读）。
func (h *Handler) tasksScan(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "task runner not enabled"})
		return
	}
	items := h.cfg.Tasks.ScanAll()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// tasksRun 单账号一键完成。body {uid} 或 {uid, task_code}。
// 只给 uid → 一键完成全部；给 task_code → 单项。
func (h *Handler) tasksRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "task runner not enabled"})
		return
	}
	var body struct {
		UID      string `json:"uid"`
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "uid required"})
		return
	}
	if body.TaskCode != "" {
		resp, engaged := h.cfg.Tasks.RunAutoOne(body.UID, body.TaskCode)
		if engaged {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "该账号有任务动作正在执行中，请等本轮结束后再试"})
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	results, engaged := h.cfg.Tasks.RunAuto(body.UID)
	if engaged {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "该账号有任务动作正在执行中，请等本轮结束后再试"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
}

// tasksQueueStart 启动执行队列。body {concurrency, growth}。
//
// concurrency 缺省（0/负）用配置值（默认 10）；上限同样来自配置（默认 100）。
// 响应回传**实际生效**的并发度，前端据此如实显示，避免"填了 50 实际跑 X"的静默偏差。
func (h *Handler) tasksQueueStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "task runner not enabled"})
		return
	}
	var body struct {
		Concurrency int  `json:"concurrency"`
		Growth      bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	conc := body.Concurrency
	if conc <= 0 {
		conc = h.taskConcurrency() // 未指定 → 用配置默认
	}
	total, effective, alreadyRunning := h.cfg.Tasks.RunQueue(conc, h.taskConcurrencyMax())
	if alreadyRunning {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "队列正在执行中"})
		return
	}
	if total == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": "全部账号没有待办任务"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"started":     true,
		"total":       total,
		"concurrency": effective,
		"conc_max":    h.taskConcurrencyMax(),
	})
}

// taskConcurrency 返回配置的任务队列并发度（未配置回落 panel 默认）。
func (h *Handler) taskConcurrency() int {
	if h.cfg.TaskConcurrency > 0 {
		return h.cfg.TaskConcurrency
	}
	return panel.DefaultConcurrency
}

// taskConcurrencyMax 返回并发度上限（未配置回落 panel 默认上限）。
func (h *Handler) taskConcurrencyMax() int {
	if h.cfg.TaskConcurrencyMax > 0 {
		return h.cfg.TaskConcurrencyMax
	}
	return panel.DefaultConcurrencyMax
}

// tasksQueueStatus 队列状态（轮询用）。
func (h *Handler) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "task runner not enabled"})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Tasks.QueueStatus())
}

// tasksList 单账号任务列表（可选）。?uid=xxx
func (h *Handler) tasksList(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Tasks == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "task runner not enabled"})
		return
	}
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "uid required"})
		return
	}
	a := h.cfg.Tasks.AccountByUID(uid)
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "account not found"})
		return
	}
	tasks, err := h.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "list tasks: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks})
}

// keepaliveRun 立即对全部账号刷新 token（看板「保活」按钮）。
//
// POST /keepalive
// 响应：{"ok":true,"total":N,"ok_count":A,"fail_count":B,"skip_count":C,"results":[...]}
//
// 与定时 22:00 的保活共用 scheduler.KeepaliveAll 的同一份实现与互斥锁：
// 撞车时返回 409（而非静默排队），让用户知道"已经有一次在跑"。
//
// 为什么值得做成按钮：accessToken 寿命取决于签发方（实测腾讯站 60 天、
// www.codebuddy.cn 站仅 3 天），临近过期时手工刷一次即可续期，
// 不必等定时到点或重启进程。
func (h *Handler) keepaliveRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Keepalive == nil {
		writeJSON(w, http.StatusNotImplemented, map[string]any{"ok": false, "error": "keepalive not enabled"})
		return
	}
	results, err := h.cfg.Keepalive()
	if err != nil {
		// 仅"已有一次在跑"是预期分支：回 409 让前端提示，不当作错误。
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	okN, failN, skipN := 0, 0, 0
	for _, it := range results {
		switch it.Status {
		case "ok":
			okN++
		case "fail":
			failN++
		default:
			skipN++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"total":      len(results),
		"ok_count":   okN,
		"fail_count": failN,
		"skip_count": skipN,
		"results":    results,
	})
}
