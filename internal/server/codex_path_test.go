package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestCodexPathVariants 记录 Codex 直连时两种 base_url 写法的实际结果。
//
// 背景：Codex 把 model_providers.<x>.base_url **原样拼接** `/responses`：
//
//	base_url = "http://host:7863/"    → POST /responses     ← 我们**不提供**
//	base_url = "http://host:7863/v1"  → POST /v1/responses  ← 我们提供
//
// 本用例把这两种情况都钉住，便于日后判断"是配置问题还是网关缺路由"。
func TestCodexPathVariants(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	cases := []struct {
		path    string
		want404 bool
		desc    string
	}{
		// 只断言"路由是否存在"：空请求体会让处理器返回 400/200（各端点宽严不同），
		// 但只要**不是 404** 就说明路由命中；404 才是"没有这个端点"。
		{"/v1/responses", false, "正式路径：base_url 带 /v1 时 Codex 请求"},
		{"/responses", false, "别名：base_url 不带 /v1 时 Codex 请求（兼容，避免 404）"},
		{"/v1/messages", false, "正式路径：Anthropic（Claude Code）"},
		{"/messages", false, "别名：Anthropic base_url 不带 /v1"},
		{"/v1/chat/completions", false, "OpenAI 通用端点"},
		// 反向断言：不存在的路径必须 404，别把兜底路由写成"什么都接"。
		{"/v1/nope", true, "不存在的端点应 404"},
		{"/nope", true, "不存在的无前缀端点应 404"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", tc.path, nil)
			h.ServeHTTP(rec, req)
			is404 := rec.Code == http.StatusNotFound
			if is404 != tc.want404 {
				t.Errorf("%s: code=%d want404=%v（%s）", tc.path, rec.Code, tc.want404, tc.desc)
			}
		})
	}
}
