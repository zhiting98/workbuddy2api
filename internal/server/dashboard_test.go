package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// newDashboardHandler 构造启用看板的 handler。
func newDashboardHandler(t *testing.T, user, pass string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{
		Pool: p, Upstream: up, AuthDir: t.TempDir(),
		APIKey: "sk-api", DashboardUser: user, DashboardPass: pass,
	})
}

// basic 构造 Basic 头。
func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// ---------------------------------------------------------------------------
// 页面托管
// ---------------------------------------------------------------------------

// TestDashboardServesPageWithAuth 正确凭据可拿到看板页面。
func TestDashboardServesPageWithAuth(t *testing.T) {
	h := newDashboardHandler(t, "ops", "secret")
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", basic("ops", "secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type=%q want text/html", ct)
	}
	body := rec.Body.String()
	// 页面关键元素在位（确认嵌的是真页面而不是空文件）。
	for _, want := range []string{"<!DOCTYPE html>", "WorkBuddy", "acctBody", "addAcctBtn"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

// TestDashboardRequiresAuth 无凭据 / 错凭据 → 401，且带 WWW-Authenticate（触发浏览器登录框）。
func TestDashboardRequiresAuth(t *testing.T) {
	h := newDashboardHandler(t, "ops", "secret")
	for _, tc := range []struct {
		name, hdr string
	}{
		{"无头", ""},
		{"错密码", basic("ops", "wrong")},
		{"错用户", basic("nope", "secret")},
		{"畸形 base64", "Basic !!!notbase64!!!"},
		{"无冒号", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))},
		{"非 Basic", "Bearer sk-api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			if tc.hdr != "" {
				req.Header.Set("Authorization", tc.hdr)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("code=%d want 401", rec.Code)
			}
			if wa := rec.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "Basic") {
				t.Errorf("WWW-Authenticate=%q want Basic challenge", wa)
			}
			// 未授权时不得泄露页面内容。
			if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
				t.Error("401 response leaked dashboard HTML")
			}
		})
	}
}

// TestDashboardDisabledWithoutCreds 未配置凭据 → 看板不启用，`/` 变为安装向导。
//
// 这是相对早期实现的**行为变更**：早期未配置时 `/` 返回 404（静默），
// 用户无从判断"是没打包进去还是没配凭据"（本仓库历史上多次因此困惑）。
// 现在改为展示安装向导，装好后 `/` 才变为受保护的看板。
func TestDashboardDisabledWithoutCreds(t *testing.T) {
	for _, tc := range []struct{ name, user, pass string }{
		{"都为空", "", ""},
		{"只有用户", "ops", ""},
		{"只有密码", "", "secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDashboardHandler(t, tc.user, tc.pass)
			if h.dashboardEnabled() {
				t.Fatal("dashboard should be disabled")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("code=%d want 200 (安装向导)", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "安装向导") {
				t.Errorf("未配置时应展示安装向导，实际: %.80s", rec.Body.String())
			}
			// 数据入口 /api/* 此时不应存在（未安装，没有数据可看）。
			rec2 := httptest.NewRecorder()
			h.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/status", nil))
			if rec2.Code != http.StatusNotFound {
				t.Errorf("/api/status code=%d want 404 (未安装时无数据入口)", rec2.Code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// /api/* 转发
// ---------------------------------------------------------------------------

// TestDashboardAPIProxyForwards /api/xxx → 内部 /xxx（剥前缀）。
func TestDashboardAPIProxyForwards(t *testing.T) {
	h := newDashboardHandler(t, "ops", "secret")
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Authorization", basic("ops", "secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "accounts") {
		t.Errorf("body=%s want status payload", rec.Body)
	}
}

// TestDashboardAPIProxyRequiresAuth 数据接口同样受 Basic Auth 保护——
// 不能出现"页面要登录、数据接口裸奔"的裂缝。
func TestDashboardAPIProxyRequiresAuth(t *testing.T) {
	h := newDashboardHandler(t, "ops", "secret")
	for _, path := range []string{"/api/status", "/api/calls", "/api/accounts/login/start"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without creds: code=%d want 401", path, rec.Code)
		}
	}
}

// TestDashboardAPIProxyUnknownPath404 未知 /api 路径 → 404（不回落成 HTML）。
func TestDashboardAPIProxyUnknownPath404(t *testing.T) {
	h := newDashboardHandler(t, "ops", "secret")
	req := httptest.NewRequest("GET", "/api/nope", nil)
	req.Header.Set("Authorization", basic("ops", "secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

// TestDashboardAPIPostForwarded POST 也能转发（添加账号 / 复活走 POST）。
func TestDashboardAPIPostForwarded(t *testing.T) {
	f := newFakeOAuth(t)
	h := newDashboardHandler(t, "ops", "secret")
	req := httptest.NewRequest("POST", "/api/accounts/login/start", nil)
	req.Header.Set("Authorization", basic("ops", "secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "auth_url") {
		t.Errorf("body=%s want auth_url", rec.Body)
	}
	_ = f
}

// TestDashboardAPIReviveForwarded 带路径参数的端点在 /api 前缀下同样可用
// （验证剥前缀后 PathValue 仍能被正确解析）。
func TestDashboardAPIReviveForwarded(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Disable("u1", "12153 session dead")
	h := NewHandler(Config{
		Pool: p, Upstream: up, AuthDir: t.TempDir(),
		DashboardUser: "ops", DashboardPass: "secret",
	})

	req := httptest.NewRequest("POST", "/api/accounts/u1/revive", nil)
	req.Header.Set("Authorization", basic("ops", "secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if st, _ := p.Status("u1"); st.Disabled {
		t.Error("revive through /api/ did not take effect")
	}
}

// ---------------------------------------------------------------------------
// 与 API 客户端鉴权的隔离
// ---------------------------------------------------------------------------

// TestDashboardCredsSeparateFromAPIKey 两种凭据互不通用：
//   - 看板密码不能当 api_key 调 API
//   - api_key 不能当看板密码登录
//
// 这是"凭据分离"的核心语义：把看板密码给运维，不必交出 API 密钥。
func TestDashboardCredsSeparateFromAPIKey(t *testing.T) {
	h := newDashboardHandler(t, "ops", "secret")

	// 看板密码当 Bearer 调 API → 401。
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", basic("ops", "secret"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("dashboard creds accepted as API key: code=%d want 401", rec.Code)
	}

	// api_key 当 Basic 登看板 → 401。
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Authorization", basic("sk-api", "sk-api"))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("api_key accepted as dashboard login: code=%d want 401", rec2.Code)
	}
}

// TestParseBasicAuth 解析边界。
func TestParseBasicAuth(t *testing.T) {
	for _, tc := range []struct {
		in         string
		user, pass string
		ok         bool
	}{
		{basic("a", "b"), "a", "b", true},
		{"basic " + base64.StdEncoding.EncodeToString([]byte("x:y")), "x", "y", true}, // 大小写不敏感
		{basic("u", "p:with:colons"), "u", "p:with:colons", true},                     // 密码含冒号：按第一个冒号切
		{"", "", "", false},
		{"Bearer x", "", "", false},
		{"Basic", "", "", false},
		{"Basic !!!", "", "", false},
		{"Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon")), "", "", false},
		{basic("", ""), "", "", true}, // 空用户名密码：交给凭据比较去拒绝
	} {
		u, p, ok := parseBasicAuth(tc.in)
		if ok != tc.ok || (ok && (u != tc.user || p != tc.pass)) {
			t.Errorf("parseBasicAuth(%q) = (%q,%q,%v) want (%q,%q,%v)", tc.in, u, p, ok, tc.user, tc.pass, tc.ok)
		}
	}
}
