package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postKeepalive 发一次保活请求。
func postKeepalive(t *testing.T, h *Handler) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/keepalive", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestKeepaliveEndpointCounts 保活端点按 status 汇总成功/失败/跳过。
func TestKeepaliveEndpointCounts(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool: testPoolWith(), Upstream: up,
		Keepalive: func() ([]KeepaliveOutcome, error) {
			return []KeepaliveOutcome{
				{UID: "u1", Status: "ok", ExpiresAt: 1794405600, Extends: 60},
				{UID: "u2", Status: "fail", Detail: "boom"},
				{UID: "u3", Status: "skipped", Detail: "disabled"},
			}, nil
		},
	})
	code, out := postKeepalive(t, h)
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["total"] != float64(3) || out["ok_count"] != float64(1) ||
		out["fail_count"] != float64(1) || out["skip_count"] != float64(1) {
		t.Errorf("计数不对: %v", out)
	}
}

// TestKeepaliveEndpointBusy 已有一次在跑 → 409（不是 500，也不是静默成功）。
func TestKeepaliveEndpointBusy(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool: testPoolWith(), Upstream: up,
		Keepalive: func() ([]KeepaliveOutcome, error) {
			return nil, errors.New("keepalive already running")
		},
	})
	code, out := postKeepalive(t, h)
	if code != http.StatusConflict {
		t.Errorf("code=%d want 409; out=%v", code, out)
	}
}

// TestKeepaliveEndpointDisabled 未注入 Keepalive → 501（与 /tasks 未启用同口径）。
func TestKeepaliveEndpointDisabled(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	code, _ := postKeepalive(t, h)
	if code != http.StatusNotImplemented {
		t.Errorf("code=%d want 501", code)
	}
}

// TestKeepaliveEndpointRequiresAuth 配置 api_key 时需鉴权（保活会改写全部凭证）。
func TestKeepaliveEndpointRequiresAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool: testPoolWith(), Upstream: up, APIKey: "sk-secret",
		Keepalive: func() ([]KeepaliveOutcome, error) {
			return []KeepaliveOutcome{{UID: "u1", Status: "ok"}}, nil
		},
	})

	// 无凭据 → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/keepalive", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 api_key code=%d want 401", rec.Code)
	}

	// 带正确 api_key → 放行
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/keepalive", strings.NewReader("{}"))
	req2.Header.Set("Authorization", "Bearer sk-secret")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Errorf("带 api_key code=%d want 200 body=%s", rec2.Code, rec2.Body)
	}
}

// TestKeepaliveEndpointEmptyPool 空池也回 200（total=0），不报错。
func TestKeepaliveEndpointEmptyPool(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{
		Pool: testPoolWith(), Upstream: up,
		Keepalive: func() ([]KeepaliveOutcome, error) { return nil, nil },
	})
	code, out := postKeepalive(t, h)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if out["total"] != float64(0) {
		t.Errorf("total=%v want 0", out["total"])
	}
}
