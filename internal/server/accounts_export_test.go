package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// postExport 发一次导出请求。
func postExport(t *testing.T, h *Handler, body string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts/export", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestAccountsExportRequiresConfirm 未带确认短语一律 400——导出含明文 token，
// 不能因为"按钮点错了"就吐出来。
func TestAccountsExportRequiresConfirm(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	if code, _ := postImport(t, h, importNested); code != 200 {
		t.Fatal("准备账号失败")
	}

	for _, body := range []string{
		``,                      // 空 body
		`{}`,                    // 无 confirm
		`{"confirm":""}`,        // 空串
		`{"confirm":"export"}`,  // 小写不匹配（大小写敏感）
		`{"confirm":"YES"}`,     // 错短语
		`{"confirm":"EXPORT "}`, // 带空格（TrimSpace 后匹配 → 应通过，此处故意不加尾空格测别的）
		`not json`,              // 非法 JSON
	} {
		code, resp := postExport(t, h, body)
		// 最后那个 `{"confirm":"EXPORT "}` 会被 TrimSpace 接受，故单独排除
		if strings.TrimSpace(body) == `{"confirm":"EXPORT "}` {
			continue
		}
		if code != http.StatusBadRequest {
			t.Errorf("body=%q code=%d want 400; resp=%s", body, code, resp)
		}
	}
}

// TestAccountsExportRoundTrip 核心保证：导出的内容能被「导入 JSON」原样读回。
//
// 这是备份功能的**唯一**验收标准——导出文件若导不回来，备份就是假的。
// 故这里不比对字段名，而是直接把导出结果喂给 ParseImport，逐字段核对。
func TestAccountsExportRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	if code, out := postImport(t, h, importNested); code != 200 {
		t.Fatalf("导入准备失败: code=%d out=%v", code, out)
	}

	code, body := postExport(t, h, `{"confirm":"EXPORT"}`)
	if code != 200 {
		t.Fatalf("export code=%d body=%s", code, body)
	}

	// 1) 导出结果必须能被 ParseImport 接受（含 access_token + uid）。
	accounts, results, err := auth.ParseImport([]byte(body))
	if err != nil {
		t.Fatalf("导出内容无法被解析：%v\nbody=%s", err, body)
	}
	if len(accounts) != 1 || len(results) != 1 || !results[0].OK {
		t.Fatalf("应解析出 1 个可用账号，实得 accounts=%d results=%+v", len(accounts), results)
	}

	// 2) 逐字段核对：token 必须与导入时一致（不能丢、不能被改写）。
	a := accounts[0]
	if a.AccessToken != "AT-NESTED" {
		t.Errorf("accessToken=%q want AT-NESTED", a.AccessToken)
	}
	if a.RefreshToken != "RT-NESTED" {
		t.Errorf("refreshToken=%q want RT-NESTED", a.RefreshToken)
	}
	if a.UID != "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d" {
		t.Errorf("uid=%q", a.UID)
	}
	if a.ExpiresAt != 1794405600 {
		t.Errorf("expiresAt=%d want 1794405600（绝对秒，不得被改成毫秒或相对秒）", a.ExpiresAt)
	}
	if a.Nickname != "示例昵称甲" {
		t.Errorf("nickname=%q want 示例昵称甲", a.Nickname)
	}
	if a.Domain != "www.codebuddy.cn" {
		t.Errorf("domain=%q", a.Domain)
	}
}

// TestAccountsExportFormatIsNested 导出必须是嵌套形，与 auths/*.json 同构——
// 这样"导出文件"与"auths 目录里的文件"可直接互相替换，备份才能手动恢复。
func TestAccountsExportFormatIsNested(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	postImport(t, h, importNested)

	_, body := postExport(t, h, `{"confirm":"EXPORT"}`)
	var arr []map[string]any
	if err := json.Unmarshal([]byte(body), &arr); err != nil {
		t.Fatalf("导出应为 JSON 数组：%v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("len=%d want 1", len(arr))
	}
	// 嵌套形特征：顶层只有 account / auth 两个键。
	if _, ok := arr[0]["account"]; !ok {
		t.Error("缺少 account 键（应为嵌套形）")
	}
	authObj, ok := arr[0]["auth"].(map[string]any)
	if !ok {
		t.Fatal("缺少 auth 键（应为嵌套形）")
	}
	for _, k := range []string{"accessToken", "refreshToken", "expiresAt", "domain"} {
		if _, ok := authObj[k]; !ok {
			t.Errorf("auth.%s 缺失", k)
		}
	}
}

// TestAccountsExportHeaders 响应头：带下载文件名 + no-store（凭证不得被缓存）。
func TestAccountsExportHeaders(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	postImport(t, h, importNested)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts/export", strings.NewReader(`{"confirm":"EXPORT"}`))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") || !strings.Contains(cd, "workbuddy-accounts-") {
		t.Errorf("Content-Disposition=%q want attachment + workbuddy-accounts-*.json", cd)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("Cache-Control=%q want no-store（凭证不得缓存）", cc)
	}
}

// TestAccountsExportEmptyPool 空池导出空数组（而非报错）：备份一个还没加号的网关应正常。
func TestAccountsExportEmptyPool(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	code, body := postExport(t, h, `{"confirm":"EXPORT"}`)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, body)
	}
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("空池应导出 []，实得 %q", body)
	}
}

// TestAccountsExportRequiresAuthDir 未配置 auth_dir → 503（与其他账号端点同口径）。
func TestAccountsExportRequiresAuthDir(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, AuthDir: ""})
	code, _ := postExport(t, h, `{"confirm":"EXPORT"}`)
	if code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503", code)
	}
}

// TestAccountsExportRequiresAPIKey 配置了 api_key 时必须鉴权（token 比 status 更敏感）。
func TestAccountsExportRequiresAPIKey(t *testing.T) {
	dir := t.TempDir()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, AuthDir: dir, APIKey: "sk-secret"})

	// 无凭据 → 401
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/accounts/export", strings.NewReader(`{"confirm":"EXPORT"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无 api_key 时 code=%d want 401", rec.Code)
	}

	// 带正确 api_key → 放行
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/accounts/export", strings.NewReader(`{"confirm":"EXPORT"}`))
	req2.Header.Set("Authorization", "Bearer sk-secret")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Errorf("带 api_key 时 code=%d want 200 body=%s", rec2.Code, rec2.Body)
	}
}
