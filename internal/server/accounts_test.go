package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/oauth"
)

// fakeOAuth 假上游：可切换 pending/成功，并记录被调用的 state。
type fakeOAuth struct {
	mu       sync.Mutex
	srv      *httptest.Server
	states   []string // /auth/token 收到的 state 序列
	done     bool     // true = 登录已完成（返回 token）
	failNext bool     // true = token 端点返回 5xx
	uid      string   // 成功时返回的 uid
	nick     string
	started  int   // StartLogin 被调用次数
	stateSeq int64 // 原子自增，保证并发下 state 唯一
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{uid: "uid-new-001", nick: "新账号"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/v2/plugin/auth/state"):
			f.started++
			// 每次 start 返回不同 state，便于识别会话串号。
			// 必须用原子计数而非时间戳：并发 start 在同一微秒内会撞出相同 state，
			// 那样就测不出"会话隔离"了（假上游自己制造的碰撞会污染断言）。
			st := fmt.Sprintf("st-%d", atomic.AddInt64(&f.stateSeq, 1))
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"state":"` + st + `","authUrl":"https://login.example/` + st + `"}}`))
		case strings.Contains(r.URL.Path, "/v2/plugin/auth/token"):
			st := r.URL.Query().Get("state")
			f.states = append(f.states, st)
			if f.failNext {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
				return
			}
			if !f.done {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":4001,"msg":"login ing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"accessToken":"AT-1","refreshToken":"RT-1","expiresIn":3600,"domain":"www.codebuddy.cn"}}`))
		case strings.Contains(r.URL.Path, "/v2/plugin/login/account"):
			// 不同 state → 不同账号（真实场景：不同用户/不同号登录）。
			// 用 state 派生 uid，避免并发用例都写同一个文件名。
			st := r.URL.Query().Get("state")
			_, _ = w.Write([]byte(`{"code":0,"msg":"","data":{"uid":"` + f.uidFor(st) + `","enterpriseId":"ent-1","nickname":"` + f.nick + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	// 把 oauth 的上游指向假服务。
	old := oauth.SetBaseURLForTest(f.srv.URL)
	t.Cleanup(func() { oauth.SetBaseURLForTest(old) })
	return f
}

// setDone 标记登录完成。
func (f *fakeOAuth) setDone(v bool) {
	f.mu.Lock()
	f.done = v
	f.mu.Unlock()
}

// uidFor 按 state 派生 uid：默认单账号用例（state 为空或固定）仍得到 f.uid，
// 并发用例中每个会话拿到不同 uid，避免写同一个文件。
func (f *fakeOAuth) uidFor(state string) string {
	if state == "" {
		return f.uid
	}
	return f.uid + "-" + strings.TrimPrefix(state, "st-")
}

// seenStates 返回 token 端点收到的 state 列表副本。
func (f *fakeOAuth) seenStates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.states...)
}

// newAccountsHandler 构造带账号管理能力的 handler（authDir 为临时目录）。
func newAccountsHandler(t *testing.T, authDir string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith()
	return NewHandler(Config{Pool: p, Upstream: up, AuthDir: authDir, APIKey: ""})
}

// startLogin 调 start 端点并解出 id/auth_url。
func startLogin(t *testing.T, h *Handler) (id, authURL string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/accounts/login/start", nil))
	if rec.Code != 200 {
		t.Fatalf("start code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		ID      string `json:"id"`
		AuthURL string `json:"auth_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("start body not JSON: %v (%s)", err, rec.Body)
	}
	if out.ID == "" || out.AuthURL == "" {
		t.Fatalf("start missing id/auth_url: %s", rec.Body)
	}
	return out.ID, out.AuthURL
}

// poll 调一次 poll 端点。
func poll(t *testing.T, h *Handler, id string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/login/poll?id="+id, nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// ---------------------------------------------------------------------------
// start / poll 基本流程
// ---------------------------------------------------------------------------

// TestAccountsLoginStartReturnsURL start 返回 id 与授权链接，且**不回传 state**。
func TestAccountsLoginStartReturnsURL(t *testing.T) {
	newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	id, authURL := startLogin(t, h)
	if !strings.Contains(authURL, "login.example") {
		t.Errorf("auth_url=%q", authURL)
	}
	// 断言响应里没有 state（减少泄露面）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/accounts/login/start", nil))
	if strings.Contains(rec.Body.String(), `"state"`) {
		t.Errorf("start response must not expose state: %s", rec.Body)
	}
	_ = id
}

// TestAccountsLoginPollPending 未完成时返回 pending（前端据此继续轮询）。
func TestAccountsLoginPollPending(t *testing.T) {
	newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	id, _ := startLogin(t, h)
	code, out := poll(t, h, id)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if out["status"] != "pending" {
		t.Errorf("status=%v want pending", out["status"])
	}
}

// TestAccountsLoginPollDone 完成后：落盘 + 入池 + 返回账号信息。
func TestAccountsLoginPollDone(t *testing.T) {
	f := newFakeOAuth(t)
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	id, _ := startLogin(t, h)
	f.setDone(true)

	code, out := poll(t, h, id)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["status"] != "done" {
		t.Fatalf("status=%v want done", out["status"])
	}
	acct, _ := out["account"].(map[string]any)
	if acct["nickname"] != "新账号" || acct["uid"] == "" {
		t.Errorf("account=%v", out["account"])
	}
	uid, _ := acct["uid"].(string)

	// 落盘：文件名必须是 workbuddy-<uid>.json（否则重启后 LoadDir 扫不到）。
	path := filepath.Join(dir, "workbuddy-"+uid+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("auth file not written: %v", err)
	}
	// 权限必须是 0600。Windows 不支持 Unix 权限位（os.WriteFile 0600 会呈现 0666），
	// 故仅在类 Unix 上断言——生产容器是 Alpine，那里这个断言才有意义。
	st, _ := os.Stat(path)
	if perm := st.Mode().Perm(); perm != 0o600 {
		if runtime.GOOS == "windows" {
			t.Logf("跳过权限断言（Windows 不支持 Unix 权限位，实测 perm=%o）", perm)
		} else {
			t.Errorf("file perm=%o want 600", perm)
		}
	}
	// 内容必须是 auth 包能解析的嵌套形，且 token 正确。
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("written file not parseable by auth.Parse: %v", err)
	}
	if a.UID != uid || a.AccessToken != "AT-1" || a.RefreshToken != "RT-1" {
		t.Errorf("parsed auth wrong: %+v", a)
	}
	if a.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expiresAt not in future: %d", a.ExpiresAt)
	}

	// 入池：新账号应出现在池里。
	if _, ok := h.cfg.Pool.Status(uid); !ok {
		t.Error("new account not added to pool")
	}
}

// TestAccountsLoginPollDoneNoTokenInResponse 安全回归：响应体绝不含任何 token。
func TestAccountsLoginPollDoneNoTokenInResponse(t *testing.T) {
	f := newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	id, _ := startLogin(t, h)
	f.setDone(true)
	_, _ = poll(t, h, id)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/login/poll?id="+id, nil))
	body := rec.Body.String()
	for _, bad := range []string{"AT-1", "RT-1", "accessToken", "refreshToken", "access_token", "refresh_token"} {
		if strings.Contains(body, bad) {
			t.Errorf("poll response leaked %q: %s", bad, body)
		}
	}
}

// TestAccountsLoginPollIdempotent 重复 poll 已完成会话不重复调上游、不重复入池。
func TestAccountsLoginPollIdempotent(t *testing.T) {
	f := newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	id, _ := startLogin(t, h)
	f.setDone(true)
	_, _ = poll(t, h, id)
	before := len(f.seenStates())
	_, out := poll(t, h, id)
	after := len(f.seenStates())
	if after != before {
		t.Errorf("repeat poll hit upstream again: %d -> %d", before, after)
	}
	if out["status"] != "done" {
		t.Errorf("status=%v want done", out["status"])
	}
}

// TestAccountsLoginPollUnknownID 未知/过期 id → 404（不能 500 或静默 pending）。
func TestAccountsLoginPollUnknownID(t *testing.T) {
	newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/login/poll?id=nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404 body=%s", rec.Code, rec.Body)
	}
}

// TestAccountsLoginPollMissingID 缺 id 参数 → 400。
func TestAccountsLoginPollMissingID(t *testing.T) {
	newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/login/poll", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// TestAccountsLoginPollUpstreamFailure 上游 5xx → failed（可回报错误）。
func TestAccountsLoginPollUpstreamFailure(t *testing.T) {
	f := newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())
	id, _ := startLogin(t, h)
	f.mu.Lock()
	f.failNext = true
	f.mu.Unlock()

	code, out := poll(t, h, id)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if out["status"] != "failed" {
		t.Fatalf("status=%v want failed (%v)", out["status"], out)
	}
	// 失败信息不得含 token。
	if s, _ := out["error"].(string); strings.Contains(s, "AT-1") {
		t.Errorf("error leaked token: %s", s)
	}
}

// TestAccountsLoginStartWithoutAuthDir 未配置 auth_dir → 503（明确失败，不静默）。
func TestAccountsLoginStartWithoutAuthDir(t *testing.T) {
	newFakeOAuth(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith()
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: ""})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/accounts/login/start", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------------------
// 并发：这是从 CLI 单文件 state 迁移到服务端后最关键的回归
// ---------------------------------------------------------------------------

// TestAccountsLoginConcurrentSessionsIsolated 两个并发登录会话必须各自独立：
// 各自拿到不同 id，poll 时带上各自的 state，结果不串号。
//
// 背景：CLI 把 state 写死在 /tmp/wb2api-login-state.json，两人同时登录会互相覆盖。
// 服务端改为按会话隔离，本用例锁定该语义。
func TestAccountsLoginConcurrentSessionsIsolated(t *testing.T) {
	f := newFakeOAuth(t)
	h := newAccountsHandler(t, t.TempDir())

	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/accounts/login/start", nil))
			var out struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			ids[i] = out.ID
		}(i)
	}
	wg.Wait()

	// 所有 id 必须互不相同（不可碰撞）。
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" {
			t.Fatal("empty session id")
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}

	// 并发 poll：每个会话各自向上游发自己的 state。
	f.setDone(true)
	var wg2 sync.WaitGroup
	for i := 0; i < n; i++ {
		wg2.Add(1)
		go func(i int) {
			defer wg2.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/accounts/login/poll?id="+ids[i], nil))
			var out map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if out["status"] != "done" {
				t.Errorf("session %d status=%v want done", i, out["status"])
			}
		}(i)
	}
	wg2.Wait()

	// 上游收到的 state 应全部互不相同（证明没有互相覆盖）。
	states := f.seenStates()
	uniq := map[string]bool{}
	for _, s := range states {
		if s == "" {
			t.Fatal("empty state sent upstream")
		}
		uniq[s] = true
	}
	if len(uniq) != len(states) {
		t.Errorf("state collision: %d unique of %d (%v)", len(uniq), len(states), states)
	}
}

// TestLoginSessionsExpiry 会话过期后被清理，poll 返回 404。
func TestLoginSessionsExpiry(t *testing.T) {
	s := newLoginSessions()
	defer s.Close()
	id := s.create("st-1", "https://x")
	if _, ok := s.get(id); !ok {
		t.Fatal("session should exist")
	}
	// 手动把创建时间拨到 TTL 之前。
	s.mu.Lock()
	s.m[id].CreatedAt = time.Now().Add(-loginSessionTTL - time.Minute)
	s.mu.Unlock()
	if _, ok := s.get(id); ok {
		t.Error("expired session should be gone")
	}
}

// TestLoginSessionsGCOne gcOnce 清理过期项。
func TestLoginSessionsGCOne(t *testing.T) {
	s := newLoginSessions()
	defer s.Close()
	live := s.create("a", "u")
	dead := s.create("b", "u")
	s.mu.Lock()
	s.m[dead].CreatedAt = time.Now().Add(-2 * loginSessionTTL)
	s.mu.Unlock()

	if n := s.gcOnce(time.Now()); n != 1 {
		t.Errorf("gcOnce removed %d want 1", n)
	}
	if _, ok := s.get(live); !ok {
		t.Error("live session must survive GC")
	}
}

// TestLoginSessionsFinishIdempotent finish 只在 pending 时生效（防重复入池）。
func TestLoginSessionsFinishIdempotent(t *testing.T) {
	s := newLoginSessions()
	defer s.Close()
	id := s.create("a", "u")
	if !s.finish(id, loginStatusDone, "", &loginAccountInfo{UID: "x"}) {
		t.Fatal("first finish should transition")
	}
	if s.finish(id, loginStatusFailed, "second", nil) {
		t.Error("second finish must be a no-op")
	}
	sess, _ := s.get(id)
	if sess.Status != loginStatusDone {
		t.Errorf("status=%v want done (unchanged)", sess.Status)
	}
}

// TestNewSessionIDUnguessable id 必须足够长且随机（不可枚举，否则同网段可窃取他人会话）。
func TestNewSessionIDUnguessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := newSessionID()
		if len(id) < 16 {
			t.Fatalf("session id too short: %q", id)
		}
		if seen[id] {
			t.Fatalf("session id collision: %q", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// 复活端点
// ---------------------------------------------------------------------------

// TestAccountsRevive 被禁账号可复活；未禁用的幂等返回 changed=false。
func TestAccountsRevive(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Disable("u1", "12153 session dead")
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/accounts/u1/revive", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["changed"] != true {
		t.Errorf("changed=%v want true", out["changed"])
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Error("account still disabled after revive")
	}

	// 再次复活：幂等，changed=false。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/accounts/u1/revive", nil))
	_ = json.Unmarshal(rec2.Body.Bytes(), &out)
	if out["changed"] != false {
		t.Errorf("second revive changed=%v want false", out["changed"])
	}
}

// TestAccountsReviveUnknownUID 未知 uid → 404。
func TestAccountsReviveUnknownUID(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/accounts/nope/revive", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

// TestAccountsEndpointsRequireAuth 配置 api_key 后，账号管理端点必须鉴权。
func TestAccountsEndpointsRequireAuth(t *testing.T) {
	newFakeOAuth(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir(), APIKey: "sk-secret"})

	for _, tc := range []struct{ method, path string }{
		{"POST", "/accounts/login/start"},
		{"GET", "/accounts/login/poll?id=x"},
		{"POST", "/accounts/u1/revive"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without key: code=%d want 401", tc.method, tc.path, rec.Code)
		}
	}
}
