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

// newKeyHandler 构造带指定 config key 的 handler（key 落盘到临时目录）。
func newKeyHandler(t *testing.T, cfgKeys []string, single string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	return NewHandler(Config{
		Pool: testPoolWith(), Upstream: up,
		APIKey:     single,
		APIKeys:    cfgKeys,
		APIKeyPath: filepath.Join(t.TempDir(), "api_keys.json"),
	})
}

// doReq 发一个带指定凭据的请求，返回状态码。
func doReq(t *testing.T, h *Handler, method, path, bearer, xKey string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if xKey != "" {
		req.Header.Set("x-api-key", xKey)
	}
	h.ServeHTTP(rec, req)
	return rec.Code
}

// doAuthed 直接命中内部路由但**带上看板已鉴权标记**——等价于看板 /api/* 的转发路径。
//
// 为什么需要它：/keys 是一组管理端点，与 /status、/accounts/* 同口径，需要鉴权。
// 看板页面通过 dashboardAPI 转发时会打上 dashAuthed 标记（页面本身已过 Basic Auth，
// 所以无需内嵌 api_key）。测试要覆盖"看板能管理 key"这条真实路径，就得模拟该标记。
func doAuthed(t *testing.T, h *Handler, method, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestMultiKeyBothAccepted 多个 config key 都能通过鉴权（本次功能的核心）。
func TestMultiKeyBothAccepted(t *testing.T) {
	h := newKeyHandler(t, []string{"sk-second", "sk-third"}, "sk-first")

	for _, k := range []string{"sk-first", "sk-second", "sk-third"} {
		if code := doReq(t, h, "GET", "/status", k, ""); code != 200 {
			t.Errorf("Bearer %s -> %d, want 200", k, code)
		}
		if code := doReq(t, h, "GET", "/status", "", k); code != 200 {
			t.Errorf("x-api-key %s -> %d, want 200", k, code)
		}
	}
	// 未知 key 仍拒绝。
	if code := doReq(t, h, "GET", "/status", "sk-nope", ""); code != 401 {
		t.Errorf("未知 key -> %d, want 401", code)
	}
}

// TestSingleAPIKeyStillWorks 向后兼容：只配单值 api_key（老配置）必须照常鉴权。
// 这是回归重点——若 handler 只把 APIKeys 传进 store，老配置会**静默变成不鉴权**。
func TestSingleAPIKeyStillWorks(t *testing.T) {
	h := newKeyHandler(t, nil, "sk-only")
	if code := doReq(t, h, "GET", "/status", "sk-only", ""); code != 200 {
		t.Errorf("单值 api_key 应通过，得 %d", code)
	}
	if code := doReq(t, h, "GET", "/status", "sk-other", ""); code != 401 {
		t.Errorf("错的 key 应 401，得 %d", code)
	}
}

// TestNoKeyMeansNoAuth 一个 key 都没配 = 不鉴权（历史语义，保证老配置不被锁死）。
func TestNoKeyMeansNoAuth(t *testing.T) {
	h := newKeyHandler(t, nil, "")
	if code := doReq(t, h, "GET", "/status", "", ""); code != 200 {
		t.Errorf("未配 key 应放行，得 %d", code)
	}
}

// TestAllRevokedMeansFailClosed 全部吊销后必须**拒绝**，绝不能退回"不鉴权"。
//
// 这是 fail-open 陷阱：若用"没有可用 key 即不鉴权"，那么用户吊销掉泄露的 key
// 反而会让网关彻底敞开——本意收紧却变成最不安全。
func TestAllRevokedMeansFailClosed(t *testing.T) {
	h := newKeyHandler(t, nil, "sk-leaked")
	// 吊销唯一的 key。
	if err := h.keys.setEnabled("sk-leaked", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// 该 key 失效。
	if code := doReq(t, h, "GET", "/status", "sk-leaked", ""); code != 401 {
		t.Errorf("已吊销的 key 应 401，得 %d", code)
	}
	// 且无人能调（fail-closed），而不是"没 key 就放行"。
	if code := doReq(t, h, "GET", "/status", "", ""); code != 401 {
		t.Errorf("全部吊销后未带 key 应 401（fail-closed），得 %d", code)
	}
}

// TestGenerateAndPersist 生成的 key 立即可用且已落盘（重启后仍有效）。
func TestGenerateAndPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKey: "sk-cfg", APIKeyPath: path})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/keys", strings.NewReader(`{"name":"laptop"}`))
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("生成失败: code=%d body=%s", rec.Code, rec.Body)
	}
	var out struct {
		Key APIKeyView `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	newKey := out.Key.Key
	if !strings.HasPrefix(newKey, "sk-") || len(newKey) < 32 {
		t.Fatalf("生成的 key 形态不对: %q", newKey)
	}
	if out.Key.Name != "laptop" {
		t.Errorf("name=%q want laptop", out.Key.Name)
	}
	if out.Key.Source != keySourceGenerated {
		t.Errorf("source=%v want generated", out.Key.Source)
	}

	// 1) 立即可用
	if code := doReq(t, h, "GET", "/status", newKey, ""); code != 200 {
		t.Errorf("新生成的 key 应立即可用，得 %d", code)
	}
	// 2) 已落盘
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("未落盘: %v", err)
	}
	if !strings.Contains(string(raw), newKey) {
		t.Errorf("磁盘上没有新 key")
	}
	// 3) 模拟重启：新 handler 从同一文件加载，key 仍有效
	//    （config 的 APIKey 也要照原样传入——生产里它来自 config.json）。
	h2 := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKey: "sk-cfg", APIKeyPath: path})
	if code := doReq(t, h2, "GET", "/status", newKey, ""); code != 200 {
		t.Errorf("重启后生成的 key 应仍有效，得 %d", code)
	}
	// 老 config key 也应仍在
	if code := doReq(t, h2, "GET", "/status", "sk-cfg", ""); code != 200 {
		t.Errorf("重启后 config key 应仍有效，得 %d", code)
	}
}

// TestConfigKeyCannotBeDeleted config 源的 key 拒绝删除（否则下次启动又回来，误导用户）。
func TestConfigKeyCannotBeDeleted(t *testing.T) {
	h := newKeyHandler(t, nil, "sk-cfg")
	if code := doAuthed(t, h, "DELETE", "/keys/sk-cfg"); code != http.StatusBadRequest {
		t.Errorf("删除 config key 应 400，得 %d", code)
	}
	// 但可以吊销。
	if code := doAuthed(t, h, "POST", "/keys/sk-cfg/disable"); code != 200 {
		t.Errorf("吊销 config key 应 200，得 %d", code)
	}
	if code := doReq(t, h, "GET", "/status", "sk-cfg", ""); code != 401 {
		t.Errorf("吊销后应 401，得 %d", code)
	}
	// 恢复。
	if code := doAuthed(t, h, "POST", "/keys/sk-cfg/enable"); code != 200 {
		t.Errorf("恢复应 200，得 %d", code)
	}
	if code := doReq(t, h, "GET", "/status", "sk-cfg", ""); code != 200 {
		t.Errorf("恢复后应 200，得 %d", code)
	}
}

// TestKeysEndpointsRequireAuth /keys 是管理端点，**必须鉴权**（不同于看板 Basic Auth 路径）。
// 防止有人误以为"看板能访问"就等于"裸奔可访问"。
func TestKeysEndpointsRequireAuth(t *testing.T) {
	h := newKeyHandler(t, nil, "sk-cfg")
	// 无凭据直接调 → 401
	if code := doReq(t, h, "GET", "/keys", "", ""); code != 401 {
		t.Errorf("无凭据 GET /keys 应 401，得 %d", code)
	}
	if code := doReq(t, h, "POST", "/keys", "", ""); code != 401 {
		t.Errorf("无凭据 POST /keys 应 401，得 %d", code)
	}
	// 带正确 key → 放行
	if code := doReq(t, h, "GET", "/keys", "sk-cfg", ""); code != 200 {
		t.Errorf("带 key GET /keys 应 200，得 %d", code)
	}
}

// TestGeneratedKeyCanBeDeleted 生成的 key 可删除，且删除已落盘（重启不复活）。
func TestGeneratedKeyCanBeDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKeyPath: path})

	v, err := h.keys.generate("tmp")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if code := doReq(t, h, "GET", "/status", v.Key, ""); code != 200 {
		t.Fatalf("生成后应可用，得 %d", code)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/keys/"+v.Key, nil)
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("删除应 200，得 %d body=%s", rec.Code, rec.Body)
	}
	if code := doReq(t, h, "GET", "/status", v.Key, ""); code != 401 {
		t.Errorf("删除后应 401，得 %d", code)
	}
	// 重启后不应复活（内存与磁盘都已移除）。
	h2 := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKeyPath: path})
	if code := doReq(t, h2, "GET", "/status", v.Key, ""); code != 401 {
		t.Errorf("删除的 key 重启后不应复活，得 %d", code)
	}
	// **关键**：删光所有 key 后仍必须鉴权（fail-closed）。
	// 若这里变成 200，说明网关在"用户清理干净"后反而裸奔了——比不删更危险。
	if code := doReq(t, h2, "GET", "/status", "", ""); code != 401 {
		t.Errorf("删光 key 后未带凭据应 401（fail-closed），得 %d", code)
	}
	// 重启后同样（initialized 已持久化）。
	if code := doReq(t, h2, "GET", "/status", "sk-anything", ""); code != 401 {
		t.Errorf("删光 key 后任意 key 都应 401，得 %d", code)
	}
}

// TestDeleteLastGeneratedKeyStaysFailClosed 删掉唯一的生成 key 后，网关**不得**
// 退回"不鉴权"（fail-open 陷阱）。这是 initialized 标志要解决的核心场景。
func TestDeleteLastGeneratedKeyStaysFailClosed(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	path := filepath.Join(t.TempDir(), "api_keys.json")
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKeyPath: path})

	// 没有任何 config key，后台刚生成一个 —— 此时已启用鉴权。
	v, err := h.keys.generate("only")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if code := doReq(t, h, "GET", "/status", "", ""); code != 401 {
		t.Fatalf("生成 key 后未带凭据应 401，得 %d", code)
	}

	// 删掉它 —— 用户以为"清理干净了"。
	if code := doAuthed(t, h, "DELETE", "/keys/"+v.Key); code != 200 {
		t.Fatalf("删除应 200，得 %d", code)
	}

	// 仍是鉴权态：不该因为"现在没有 key"就放行。
	if code := doReq(t, h, "GET", "/status", "", ""); code != 401 {
		t.Errorf("删掉唯一 key 后不应退回不鉴权（fail-open），得 %d", code)
	}
	// 重启后也保持鉴权态。
	h2 := NewHandler(Config{Pool: testPoolWith(), Upstream: up, APIKeyPath: path})
	if code := doReq(t, h2, "GET", "/status", "", ""); code != 401 {
		t.Errorf("重启后仍应鉴权（initialized 已持久化），得 %d", code)
	}
}

// TestKeysListMasksAndSources 列表返回掩码与来源标记，且不含已删除项。
func TestKeysListMasksAndSources(t *testing.T) {
	h := newKeyHandler(t, []string{"sk-second"}, "sk-first")
	if _, err := h.keys.generate("phone"); err != nil {
		t.Fatalf("generate: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/keys", nil)
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	var out struct {
		Keys    []APIKeyView `json:"keys"`
		Total   int          `json:"total"`
		Enabled int          `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Total != 3 || out.Enabled != 3 {
		t.Errorf("total=%d enabled=%d want 3/3", out.Total, out.Enabled)
	}
	bySrc := map[apiKeySource]int{}
	for _, k := range out.Keys {
		bySrc[k.Source]++
		if k.Preview == "" {
			t.Errorf("key %q 缺 preview", k.Key)
		}
		// 掩码不应等于明文（否则"掩码"没意义）。
		if len(k.Key) > 13 && k.Preview == k.Key {
			t.Errorf("preview 未掩码: %q", k.Preview)
		}
	}
	if bySrc[keySourceConfig] != 2 || bySrc[keySourceGenerated] != 1 {
		t.Errorf("来源分布不对: %v", bySrc)
	}
}

// TestUnconfiguredFlag 未配置任何 key 时列表显式提示（前端据此红字告警）。
func TestUnconfiguredFlag(t *testing.T) {
	h := newKeyHandler(t, nil, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/keys", nil)
	req = req.WithContext(context.WithValue(req.Context(), dashAuthedKey{}, true))
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["unconfigured"] != true || out["auth_enabled"] != false {
		t.Errorf("未配置 key 应 unconfigured=true auth_enabled=false，得 %v", out)
	}
}

// TestKeyStoreDuplicateAndBlankKeys 配置里的空串与重复项被规整（不产生多余条目）。
func TestKeyStoreDuplicateAndBlankKeys(t *testing.T) {
	ks := newKeyStore("", []string{"sk-a", "", "sk-a", "  ", " sk-b "})
	if got := len(ks.cfgKeys); got != 2 {
		t.Errorf("规整后应 2 个 key，得 %d: %v", got, ks.cfgKeys)
	}
	if !ks.valid("sk-a") || !ks.valid("sk-b") {
		t.Errorf("去空白后应仍有效: %v", ks.cfgKeys)
	}
	if ks.valid("") {
		t.Error("空 key 不应有效")
	}
}

// TestConfigEffectiveKeysMerge 单值 api_key 与数组 api_keys 合并且去重。
func TestConfigEffectiveKeysMerge(t *testing.T) {
	// 注意：这是在 cmd/server 包里的类型，这里只验证 server 侧的合并来源
	// （config.EffectiveAPIKeys 由 cmd/server 的测试覆盖）。
	ks := newKeyStore("", []string{"sk-a", "sk-b", "sk-b"})
	if len(ks.cfgKeys) != 2 {
		t.Errorf("去重后应 2 个，得 %d", len(ks.cfgKeys))
	}
}
