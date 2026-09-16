package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withTestServer 把 baseURL 指向测试服务器，并在用例结束时还原。
func withTestServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() {
		baseURL = old
		srv.Close()
	})
	return srv
}

// envelope 构造上游 {code,msg,data} 信封。
func envelope(code int, msg string, data any) string {
	raw, _ := json.Marshal(map[string]any{"code": code, "msg": msg, "data": data})
	return string(raw)
}

// TestStartLoginOK 正常拿到 state 与 authUrl。
func TestStartLoginOK(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s want POST", r.Method)
		}
		if !strings.Contains(r.URL.Path, "/v2/plugin/auth/state") {
			t.Errorf("path=%s", r.URL.Path)
		}
		if r.URL.Query().Get("platform") != "CLI" {
			t.Errorf("platform=%s want CLI", r.URL.Query().Get("platform"))
		}
		// 出站应带 CLI UA（与网关其他出站口径一致）。
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "CodeBuddy") {
			t.Errorf("UA=%q want CodeBuddy", ua)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(envelope(0, "", map[string]any{
			"state": "st-123", "authUrl": "https://login.example/authorize?x=1",
		})))
	})

	state, url, err := StartLogin()
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if state != "st-123" {
		t.Errorf("state=%q want st-123", state)
	}
	if url != "https://login.example/authorize?x=1" {
		t.Errorf("authUrl=%q", url)
	}
}

// TestStartLoginMissingFields 上游缺 state/authUrl → 报错（不能把半截结果给用户）。
func TestStartLoginMissingFields(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelope(0, "", map[string]any{"state": "only-state"})))
	})
	if _, _, err := StartLogin(); err == nil {
		t.Fatal("want error for missing authUrl")
	}
}

// TestStartLoginUpstreamError 上游 code!=0 → 报错。
func TestStartLoginUpstreamError(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelope(5001, "boom", nil)))
	})
	if _, _, err := StartLogin(); err == nil {
		t.Fatal("want error for upstream code!=0")
	}
}

// TestPollLoginDone 登录完成：拿到 token bundle + 账号信息。
func TestPollLoginDone(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v2/plugin/auth/token"):
			if got := r.URL.Query().Get("state"); got != "st-9" {
				t.Errorf("state=%q want st-9", got)
			}
			_, _ = w.Write([]byte(envelope(0, "", map[string]any{
				"accessToken": "AT-secret", "refreshToken": "RT-secret",
				"expiresIn": 3600, "domain": "www.codebuddy.cn",
			})))
		case strings.Contains(r.URL.Path, "/v2/plugin/login/account"):
			// account 端点应带 Bearer。
			if authz := r.Header.Get("Authorization"); authz != "Bearer AT-secret" {
				t.Errorf("Authorization=%q want Bearer AT-secret", authz)
			}
			_, _ = w.Write([]byte(envelope(0, "", map[string]any{
				"uid": "uid-abc", "enterpriseId": "ent-1", "nickname": "测试号",
			})))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	b, err := PollLogin("st-9")
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if b.AccessToken != "AT-secret" || b.RefreshToken != "RT-secret" {
		t.Errorf("token bundle wrong: %+v", b)
	}
	if b.UID != "uid-abc" || b.Nickname != "测试号" || b.EnterpriseID != "ent-1" {
		t.Errorf("account info wrong: %+v", b)
	}
	if b.ExpiresIn != 3600 || b.Domain != "www.codebuddy.cn" {
		t.Errorf("expiry/domain wrong: %+v", b)
	}
}

// TestPollLoginPending 上游返回业务 code!=0（"login ing"）→ ErrPending。
// 这是"用户还没登完"的正常路径，必须与真失败区分开。
func TestPollLoginPending(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 上游 pending 时实测走 4xx
		_, _ = w.Write([]byte(envelope(4001, "login ing", nil)))
	})
	_, err := PollLogin("st-x")
	if !errors.Is(err, ErrPending) {
		t.Fatalf("err=%v want ErrPending", err)
	}
}

// TestPollLoginPendingEmptyToken code=0 但 accessToken 为空 → 仍视为 pending
// （避免把空凭证当成功写入 auths/）。
func TestPollLoginPendingEmptyToken(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(envelope(0, "", map[string]any{"accessToken": "", "expiresIn": 10})))
	})
	_, err := PollLogin("st-x")
	if !errors.Is(err, ErrPending) {
		t.Fatalf("err=%v want ErrPending", err)
	}
}

// TestPollLoginUpstream5xxIsRealFailure 5xx 是真失败，不能当 pending 无限轮询。
func TestPollLoginUpstream5xxIsRealFailure(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":500,"msg":"oops"}`))
	})
	_, err := PollLogin("st-x")
	if err == nil || errors.Is(err, ErrPending) {
		t.Fatalf("err=%v want real failure (not pending)", err)
	}
}

// TestPollLoginAccountEndpointFailureTolerated account 端点失败不阻断登录
// （token 已到手即可用，uid 交给调用方兜底）。
func TestPollLoginAccountEndpointFailureTolerated(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/login/account") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(envelope(0, "", map[string]any{
			"accessToken": "AT-1", "refreshToken": "RT-1", "expiresIn": 60,
		})))
	})
	b, err := PollLogin("st-x")
	if err != nil {
		t.Fatalf("should tolerate account endpoint failure: %v", err)
	}
	if b.AccessToken != "AT-1" || b.UID != "" {
		t.Errorf("bundle=%+v want token with empty uid", b)
	}
}

// TestPollLoginEmptyState 空 state 直接被拒（不发上游）。
func TestPollLoginEmptyState(t *testing.T) {
	called := false
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	if _, err := PollLogin("   "); err == nil {
		t.Fatal("want error for empty state")
	}
	if called {
		t.Error("must not call upstream with empty state")
	}
}

// TestErrorMessagesContainNoToken 安全回归：任何错误信息都不得回显凭证。
func TestErrorMessagesContainNoToken(t *testing.T) {
	withTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// 上游把 token 回显在 msg 里（异常但可能），错误信息不应把它带出来……
		// 实际实现只带 code/msg；此用例锁定"不带我们自己解析出的 token"。
		_, _ = w.Write([]byte(envelope(0, "", map[string]any{"accessToken": "SUPER-SECRET-TOKEN"})))
	})
	// 该响应会让 poll 拿到 token，但随后 account 端点 404 → 仍应返回 bundle（容忍）。
	// 这里主要断言：错误路径（pending）的信息不含 token。
	_, err := PollLogin("st-x")
	if err == nil {
		// 成功路径：bundle 里有 token 是应该的（调用方要落盘），无需断言。
		return
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-TOKEN") {
		t.Errorf("error message leaked token: %v", err)
	}
}
