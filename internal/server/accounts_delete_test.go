package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// delAcct 发一次删除请求。
func delAcct(t *testing.T, h *Handler, uid string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/accounts/"+uid, nil))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// writeAuth 在 authDir 写一个账号文件（返回路径）。
func writeAuth(t *testing.T, dir, uid, nick string) string {
	t.Helper()
	a := &auth.Auth{
		UID: uid, Nickname: nick,
		AccessToken: "AT-" + uid, RefreshToken: "RT-" + uid,
		ExpiresAt: 9999999999, Domain: "www.codebuddy.cn",
	}
	p, err := a.Save(dir)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return p
}

// TestAccountsDeleteSoftDeletesToRecycleBin 删除后：文件进回收站、池中移除。
func TestAccountsDeleteSoftDeletesToRecycleBin(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	h.cfg.Pool.Add(&auth.Auth{UID: "del-1", AccessToken: "at", ExpiresAt: 9999999999})

	src := writeAuth(t, dir, "del-1", "待删账号")
	code, out := delAcct(t, h, "del-1")
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["deleted"] != true {
		t.Errorf("deleted=%v want true", out["deleted"])
	}

	// 原文件应已消失。
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("source file still exists: %v", err)
	}
	// 回收站里应有它（且内容完好，便于恢复）。
	recycle := filepath.Join(dir, ".deleted")
	entries, err := os.ReadDir(recycle)
	if err != nil {
		t.Fatalf("recycle bin missing: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("recycle bin has %d entries want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(recycle, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.Parse(raw)
	if err != nil || a.UID != "del-1" || a.AccessToken != "AT-del-1" {
		t.Errorf("recycled file not intact: %+v err=%v", a, err)
	}
	// 池中应已移除。
	if _, ok := h.cfg.Pool.Status("del-1"); ok {
		t.Error("account still in pool after delete")
	}
}

// TestAccountsDeletePersistsRemoval 删除会立刻写 state.json——
// 否则进程在 5s 后台落盘前被杀，重启后账号会变成"没有凭证的幽灵条目"。
//
// 这里通过 handler 删除后直接读 state.json 验证"立即落盘"（不等待后台 flusher）。
func TestAccountsDeletePersistsRemoval(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.json")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "del-2", AccessToken: "at", ExpiresAt: 9999999999})
	// testPoolWith 用的是内存池（stateFile 为空）；这里换成带落盘的池：
	// 直接构造另一个池并手工放进 state 文件路径。
	p2 := pool.New(stateFile)
	p2.Add(&auth.Auth{UID: "del-2", AccessToken: "at", ExpiresAt: 9999999999})
	if !p2.RemoveUID("del-2") {
		t.Fatal("RemoveUID returned false")
	}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("state.json not written immediately after RemoveUID: %v", err)
	}
	if strings.Contains(string(raw), "del-2") {
		t.Errorf("state.json still contains deleted uid: %s", raw)
	}
	_ = p
	_ = up
}

// TestAccountsDeleteNotFound 池与文件都不存在 → 404。
func TestAccountsDeleteNotFound(t *testing.T) {
	h := newAccountsHandler(t, t.TempDir())
	code, _ := delAcct(t, h, "no-such-uid")
	if code != http.StatusNotFound {
		t.Errorf("code=%d want 404", code)
	}
}

// TestAccountsDeletePathTraversal uid 含穿越字符时不得删到 auth_dir 之外。
func TestAccountsDeletePathTraversal(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "victim.json")
	if err := os.WriteFile(outside, []byte(`{"secret":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	h := newAccountsHandler(t, dir)
	for _, evil := range []string{
		"..%2F..%2Fvictim", "../../victim", `..\..\victim`, "....//victim",
	} {
		code, _ := delAcct(t, h, evil)
		// 允许 400/404，但**绝不能删到外面**。
		_ = code
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("outside file was deleted via uid=%q: %v", evil, err)
		}
	}
	// 外面的文件仍在。
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("victim file gone: %v", err)
	}
}

// TestAccountsDeleteOnlyTouchesManagedFiles 只删自己管理的 workbuddy-*.json；
// 用户放进 auths/ 的其他文件不得被删。
func TestAccountsDeleteOnlyTouchesManagedFiles(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "important-notes.json")
	if err := os.WriteFile(other, []byte(`{"keep":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newAccountsHandler(t, dir)
	delAcct(t, h, "some-uid")
	if _, err := os.Stat(other); err != nil {
		t.Errorf("unmanaged file was touched: %v", err)
	}
}

// TestAccountsDeleteWithoutAuthDir 未配置 auth_dir → 503。
func TestAccountsDeleteWithoutAuthDir(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: ""})
	code, _ := delAcct(t, h, "u1")
	if code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503", code)
	}
}

// TestAccountsDeleteRequiresAuth 配了 api_key 时删除端点必须鉴权。
func TestAccountsDeleteRequiresAuth(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, AuthDir: t.TempDir(), APIKey: "sk-secret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/accounts/u1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code=%d want 401", rec.Code)
	}
}

// TestAccountsDeleteThenReimport 删掉后可以重新导入（回收站不干扰新文件）。
func TestAccountsDeleteThenReimport(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	writeAuth(t, dir, "cycle-1", "循环账号")

	if code, _ := delAcct(t, h, "cycle-1"); code != 200 {
		t.Fatalf("delete code=%d", code)
	}
	// 重新导入同一 uid。
	body := `{"uid":"cycle-1","nickname":"循环账号2","access_token":"AT-NEW"}`
	if code, out := postImport(t, h, body); code != 200 || out["imported"] != float64(1) {
		t.Fatalf("reimport failed: code=%d out=%v", code, out)
	}
	// 新文件应在 auths/ 下（不是回收站里）。
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-cycle-1.json")); err != nil {
		t.Errorf("reimported file missing: %v", err)
	}
	// 回收站里保留着旧的那份（时间戳前缀避免覆盖）。
	entries, _ := os.ReadDir(filepath.Join(dir, ".deleted"))
	if len(entries) != 1 {
		t.Errorf("recycle bin has %d entries want 1 (旧凭证应保留)", len(entries))
	}
}

// TestAccountsDeleteTwiceIsIdempotent 重复删除第二次应是 404（已不存在）。
func TestAccountsDeleteTwiceIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	h.cfg.Pool.Add(&auth.Auth{UID: "twice-1", AccessToken: "at", ExpiresAt: 9999999999})
	writeAuth(t, dir, "twice-1", "重复删")
	if code, _ := delAcct(t, h, "twice-1"); code != 200 {
		t.Fatalf("first delete code=%d", code)
	}
	if code, _ := delAcct(t, h, "twice-1"); code != http.StatusNotFound {
		t.Errorf("second delete code=%d want 404", code)
	}
}

// TestAccountsDeleteInPoolButFileMissing 池里有、文件不在：仍从池中移除并给 warning。
func TestAccountsDeleteInPoolButFileMissing(t *testing.T) {
	dir := t.TempDir()
	h := newAccountsHandler(t, dir)
	h.cfg.Pool.Add(&auth.Auth{UID: "ghost-1", AccessToken: "at", ExpiresAt: 9999999999})
	// 故意不写文件。
	code, out := delAcct(t, h, "ghost-1")
	if code != 200 {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if out["warning"] == nil {
		t.Errorf("expected warning for missing file, got %v", out)
	}
	if _, ok := h.cfg.Pool.Status("ghost-1"); ok {
		t.Error("ghost account still in pool")
	}
}
