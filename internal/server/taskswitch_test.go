package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newSchedHandler 构造带可控调度器开关的 handler。
// 用内存态模拟 scheduler（不引入其依赖），只验证 HTTP 层与落盘逻辑。
func newSchedHandler(t *testing.T, cfgPath string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	state := map[string]bool{"checkin": true, "travel": true, "activity": true, "keepalive": true}
	names := map[string]string{"checkin": "签到", "travel": "猫猫旅行", "activity": "活跃上报", "keepalive": "Token 保活"}
	hours := map[string][]int{"checkin": {9, 21}, "travel": {9, 21}, "activity": {10}, "keepalive": {22}}

	h := NewHandler(Config{
		Pool: testPoolWith(), Upstream: up, APIKeyPath: filepath.Join(t.TempDir(), "k.json"),
		SetupConfigPath: cfgPath,
		TaskSwitches: func() []TaskSwitch {
			out := make([]TaskSwitch, 0, 4)
			for _, k := range []string{"checkin", "travel", "activity", "keepalive"} {
				out = append(out, TaskSwitch{
					Key: k, Name: names[k], Enabled: state[k],
					Hours: hours[k], ConfigKey: k + "_enabled",
				})
			}
			return out
		},
		SetTaskEnabled: func(key string, enabled bool) bool {
			if _, ok := state[key]; !ok {
				return false
			}
			state[key] = enabled
			return true
		},
	})
	if cfgPath != "" {
		h.cfg.PersistScheduleSwitch = func(key string, enabled bool) error {
			return WriteScheduleSwitch(cfgPath, key, enabled)
		}
	}
	return h
}

// schedGet 读开关列表。
func schedGet(t *testing.T, h *Handler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/schedule", nil)
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// schedSet 切换开关。
func schedSet(t *testing.T, h *Handler, key, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/schedule/"+key, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestScheduleListReturnsAllFour 列出四类任务的开关与时点。
func TestScheduleListReturnsAllFour(t *testing.T) {
	h := newSchedHandler(t, "")
	code, out := schedGet(t, h)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 4 {
		t.Fatalf("应有 4 类任务，得 %d", len(tasks))
	}
	seen := map[string]bool{}
	for _, it := range tasks {
		m := it.(map[string]any)
		seen[m["key"].(string)] = true
		if m["name"] == "" || m["config_key"] == "" {
			t.Errorf("任务项缺 name/config_key: %v", m)
		}
	}
	for _, k := range []string{"checkin", "travel", "activity", "keepalive"} {
		if !seen[k] {
			t.Errorf("缺少任务 %s", k)
		}
	}
}

// TestScheduleSetToggles 切换开关：applied=true，且列表随之变化。
func TestScheduleSetToggles(t *testing.T) {
	h := newSchedHandler(t, "")
	code, out := schedSet(t, h, "checkin", `{"enabled":false}`)
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["applied"] != true {
		t.Errorf("applied=%v want true（内存应已生效）", out["applied"])
	}
	// 列表应反映为新值。
	_, l := schedGet(t, h)
	for _, it := range l["tasks"].([]any) {
		m := it.(map[string]any)
		if m["key"] == "checkin" && m["enabled"] != false {
			t.Errorf("checkin 应已禁用，得 %v", m["enabled"])
		}
	}
}

// TestScheduleSetPersistsToConfig 切换写回 config.json 的 schedule.<key>_enabled，
// 并**保留文件里的其他字段**（不能整体重写把用户配置抹掉）。
func TestScheduleSetPersistsToConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// 预置带其他配置的文件。
	pre := `{"listen":":7863","server":{"max_body_mb":16},"schedule":{"checkin_hours":[9,21],"checkin_enabled":true,"travel_enabled":false}}`
	if err := os.WriteFile(cfgPath, []byte(pre), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newSchedHandler(t, cfgPath)

	code, out := schedSet(t, h, "checkin", `{"enabled":false}`)
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["persisted"] != true {
		t.Fatalf("persisted=%v err=%v", out["persisted"], out["persist_error"])
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("配置已损坏: %v\n%s", err, raw)
	}
	sched := doc["schedule"].(map[string]any)
	if sched["checkin_enabled"] != false {
		t.Errorf("checkin_enabled=%v want false", sched["checkin_enabled"])
	}
	// 关键：其他字段必须原样保留。
	if sched["travel_enabled"] != false {
		t.Errorf("travel_enabled 被改动: %v", sched["travel_enabled"])
	}
	if hr, _ := sched["checkin_hours"].([]any); len(hr) != 2 {
		t.Errorf("checkin_hours 丢失: %v", sched["checkin_hours"])
	}
	if srv, _ := doc["server"].(map[string]any); srv["max_body_mb"] != float64(16) {
		t.Errorf("server 配置丢失: %v", doc["server"])
	}
}

// TestScheduleSetPersistFailureReported 未提供 config 路径时如实回报"仅内存生效"，
// 而不是假装成功——否则用户重启后发现开关回退会以为有 bug。
func TestScheduleSetPersistFailureReported(t *testing.T) {
	h := newSchedHandler(t, "") // 无 cfgPath → 不注入 PersistScheduleSwitch
	code, out := schedSet(t, h, "travel", `{"enabled":false}`)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if out["applied"] != true {
		t.Error("内存仍应立即生效")
	}
	if out["persisted"] != false {
		t.Errorf("persisted=%v want false", out["persisted"])
	}
	if s, _ := out["persist_error"].(string); s == "" {
		t.Error("应给出 persist_error 说明原因")
	}
}

// TestScheduleSetUnknownKey 未知任务标识 → 400。
func TestScheduleSetUnknownKey(t *testing.T) {
	h := newSchedHandler(t, "")
	if code, _ := schedSet(t, h, "nope", `{"enabled":true}`); code != http.StatusBadRequest {
		t.Errorf("未知 key 应 400，得 %d", code)
	}
}

// TestScheduleSetBadBody 缺 enabled 字段 / 非法 JSON → 400（不静默当成启用）。
func TestScheduleSetBadBody(t *testing.T) {
	h := newSchedHandler(t, "")
	for _, body := range []string{`{}`, `{"enabled":null}`, `not json`, ``} {
		if code, _ := schedSet(t, h, "checkin", body); code != http.StatusBadRequest {
			t.Errorf("body=%q 应 400，得 %d", body, code)
		}
	}
}

// TestScheduleEndpointsRequireAuth 管理端点需鉴权。
func TestScheduleEndpointsRequireAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool: testPoolWith(), Upstream: up, APIKey: "sk-secret",
		TaskSwitches:   func() []TaskSwitch { return nil },
		SetTaskEnabled: func(string, bool) bool { return true },
	})
	// 无凭据 → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/schedule", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /schedule 无凭据应 401，得 %d", rec.Code)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/schedule/checkin", strings.NewReader(`{"enabled":true}`)))
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("POST /schedule 无凭据应 401，得 %d", rec2.Code)
	}
	// 带 key → 放行
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/schedule", nil)
	req3.Header.Set("Authorization", "Bearer sk-secret")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Errorf("带 key 应 200，得 %d", rec3.Code)
	}
}

// TestScheduleDisabledWithoutScheduler 未注入调度器 → 501（与 /tasks 同口径）。
func TestScheduleDisabledWithoutScheduler(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	if code, _ := schedGet(t, h); code != http.StatusNotImplemented {
		t.Errorf("GET /schedule 应 501，得 %d", code)
	}
	if code, _ := schedSet(t, h, "checkin", `{"enabled":true}`); code != http.StatusNotImplemented {
		t.Errorf("POST /schedule 应 501，得 %d", code)
	}
}

// TestWriteScheduleSwitchRejectsUnknownKey 未知 key 不写进配置（避免塞入无人读的字段）。
func TestWriteScheduleSwitchRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"listen":":7863"}`), 0o600)
	if err := WriteScheduleSwitch(p, "bogus", true); err == nil {
		t.Error("未知 key 应报错")
	}
	// 文件不应被改动。
	raw, _ := os.ReadFile(p)
	if strings.Contains(string(raw), "bogus") {
		t.Errorf("不应写入未知字段: %s", raw)
	}
}
