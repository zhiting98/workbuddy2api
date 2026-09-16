package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// importNested 嵌套形（单对象）。
const importNested = `{
  "account": {"enterpriseId":"","nickname":"示例昵称甲","uid":"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"},
  "auth": {"accessToken":"AT-NESTED","domain":"www.codebuddy.cn","expiresAt":1794405600,"refreshToken":"RT-NESTED"}
}`

// importArray snake_case 数组（含冗余字段）。
const importArray = `[
  {"uid":"d8e01255-0168-464f-b967-f24a3010782a","nickname":"Kingen","access_token":"AT-KINGEN",
   "refresh_token":"RT-KINGEN","expires_in":5184000,"refresh_expires_in":7776000,
   "domain":"www.codebuddy.cn","enterprise_id":"","source":"platform","credits":2092,"tag":"0913","created_at":1789229652},
  {"uid":"7b6c855c-77c4-4f7c-8e60-10000f624283","nickname":"Angel Luis","access_token":"AT-ANGEL",
   "refresh_token":"RT-ANGEL","expires_in":5184000,"domain":"www.codebuddy.cn","credits":2094,"tag":"0913"}
]`

// postImport 发一次导入请求。
func postImport(t *testing.T, h *Handler, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestAccountsImportNestedFormat 导入嵌套形并落盘入池。
func TestAccountsImportNestedFormat(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	code, out := postImport(t, h, importNested)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["imported"] != float64(1) || out["failed"] != float64(0) {
		t.Errorf("imported=%v failed=%v want 1/0", out["imported"], out["failed"])
	}
	// 落盘且文件能被 auth 包读回。
	path := dir + "/workbuddy-a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil || a.AccessToken != "AT-NESTED" {
		t.Errorf("parsed=%+v err=%v", a, err)
	}
	// 入池。
	if _, ok := h.cfg.Pool.Status("a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"); !ok {
		t.Error("imported account not added to pool")
	}
}

// TestAccountsImportArraySnakeCase 导入数组 + snake_case，两条都成功。
func TestAccountsImportArraySnakeCase(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	code, out := postImport(t, h, importArray)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	if out["imported"] != float64(2) || out["failed"] != float64(0) {
		t.Fatalf("imported=%v failed=%v want 2/0 (results=%v)", out["imported"], out["failed"], out["results"])
	}
	for _, uid := range []string{"d8e01255-0168-464f-b967-f24a3010782a", "7b6c855c-77c4-4f7c-8e60-10000f624283"} {
		if _, ok := h.cfg.Pool.Status(uid); !ok {
			t.Errorf("uid=%s not in pool", uid)
		}
		if _, err := os.Stat(dir + "/workbuddy-" + uid + ".json"); err != nil {
			t.Errorf("uid=%s file missing: %v", uid, err)
		}
	}
}

// TestAccountsImportPartialSuccess 部分失败：好条目照常导入，坏条目逐条报错。
func TestAccountsImportPartialSuccess(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	body := `[
	  {"uid":"ok-1","nickname":"Good","access_token":"AT-1","expires_in":100},
	  {"uid":"","access_token":"AT-2"},
	  {"uid":"ok-2","access_token":"AT-3"}
	]`
	code, out := postImport(t, h, body)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if out["imported"] != float64(2) || out["failed"] != float64(1) {
		t.Errorf("imported=%v failed=%v want 2/1", out["imported"], out["failed"])
	}
	results, _ := out["results"].([]any)
	if len(results) != 3 {
		t.Fatalf("results=%v want 3 entries", out["results"])
	}
	// 坏条目的 error 必须可读且不含 token。
	bad, _ := results[1].(map[string]any)
	if bad["ok"] != false || bad["error"] == nil {
		t.Errorf("bad entry=%v", bad)
	}
	if s, _ := bad["error"].(string); strings.Contains(s, "AT-2") {
		t.Errorf("error leaked token: %s", s)
	}
}

// TestAccountsImportResponseHasNoToken 安全回归：响应体绝不含 token。
func TestAccountsImportResponseHasNoToken(t *testing.T) {
	h := newAccountsHandler(t, t.TempDir())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts/import", strings.NewReader(importArray))
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, secret := range []string{"AT-KINGEN", "AT-ANGEL", "RT-KINGEN", "RT-ANGEL", "AT-NESTED"} {
		if strings.Contains(body, secret) {
			t.Errorf("import response leaked %q: %s", secret, body)
		}
	}
}

// TestAccountsImportIdempotent 重复导入同一账号：覆盖更新，不产生重复条目。
func TestAccountsImportIdempotent(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	for i := 0; i < 2; i++ {
		code, out := postImport(t, h, importNested)
		if code != 200 || out["imported"] != float64(1) {
			t.Fatalf("run %d: code=%d out=%v", i, code, out)
		}
	}
	entries, _ := os.ReadDir(dir)
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "workbuddy") && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("auths has %d files want 1（重复导入应覆盖而非新增）", n)
	}
}

// TestAccountsImportInvalidJSON 畸形 JSON → 400 且不打上游。
func TestAccountsImportInvalidJSON(t *testing.T) {
	h := newAccountsHandler(t, t.TempDir())
	for _, body := range []string{`{"uid":`, `hello`, `[]`} {
		code, _ := postImport(t, h, body)
		if code != http.StatusBadRequest {
			t.Errorf("body=%q code=%d want 400", body, code)
		}
	}
}

// TestAccountsImportWithoutAuthDir 未配置 auth_dir → 503。
func TestAccountsImportWithoutAuthDir(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith()
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: ""})
	code, _ := postImport(t, h, importNested)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503", code)
	}
}

// TestAccountsImportRequiresAuth 配了 api_key 时导入端点必须鉴权。
func TestAccountsImportRequiresAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir(), APIKey: "sk-secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts/import", strings.NewReader(importNested))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

// TestAccountsImportPathTraversalRejected uid 含路径穿越字符时不得逃出 authDir。
func TestAccountsImportPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	body := `{"uid":"../../../tmp/evil","nickname":"evil","access_token":"AT-X"}`
	code, _ := postImport(t, h, body)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	// 不得在 authDir 之外写出文件。
	if _, err := os.Stat(dir + "/../../../tmp/evil.json"); err == nil {
		t.Error("path traversal escaped authDir")
	}
	// 文件应落在 authDir 内且名字被净化。
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "evil") {
			found = true
		}
		if strings.ContainsAny(e.Name(), `/\`) {
			t.Errorf("unsafe filename %q", e.Name())
		}
	}
	if !found {
		t.Errorf("expected a sanitized file inside authDir, got %v", entries)
	}
}
