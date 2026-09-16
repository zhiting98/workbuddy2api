package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestModelsSingleAccountNegativeCache 单账号池拉取失败后，负缓存必须生效。
//
// 这是对一处真实缺陷的回归：早期循环写成"PickExcluding 返回 nil 就直接 return"，
// 而池里只有 1 个账号时，第二轮必然拿到 nil → 提前返回、**跳过了写 lastFail**，
// 于是每次请求都重新打上游（负缓存形同虚设）。多账号池反而看不出来。
func TestModelsSingleAccountNegativeCache(t *testing.T) {
	resetModelsCache()

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 500, `boom`, false
	})
	// 池里**只有 1 个**账号——正是暴露该缺陷的场景。
	p := testPoolWith(&auth.Auth{UID: "only", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetBreaker(99, time.Hour, time.Hour) // 关掉熔断干扰，只测负缓存
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	}
	req()
	first := calls
	if first == 0 {
		t.Fatal("首次应尝试上游")
	}
	// 后续请求应被负缓存挡住。
	for i := 0; i < 3; i++ {
		req()
	}
	if calls != first {
		t.Errorf("单账号池负缓存失效：首次 %d 次，之后累计 %d 次", first, calls)
	}
}

// TestModelsEmptyPoolNoNegativeCache 空池不写负缓存：
// 账号是后来才添加的，若空池时写了负缓存，刚加完账号 5 分钟内仍拉不到模型。
func TestModelsEmptyPoolNoNegativeCache(t *testing.T) {
	resetModelsCache()

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, `{"code":0,"data":{"models":[{"id":"m1","maxInputTokens":1000,"maxOutputTokens":100}],"agents":[{"name":"cli","models":["m1"]}]}}`, false
	})
	p := testPoolWith() // 空池
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if calls != 0 {
		t.Errorf("空池不应打上游，实际 %d 次", calls)
	}
	// 空池不应污染负缓存。
	dynamicModelsCache.RLock()
	lf := dynamicModelsCache.lastFail
	dynamicModelsCache.RUnlock()
	if !lf.IsZero() {
		t.Errorf("空池不应写负缓存（否则新加账号 5 分钟内拉不到模型），lastFail=%v", lf)
	}

	// 加一个账号后应立即能拉到（不被负缓存挡住）。
	p.Add(&auth.Auth{UID: "new", AccessToken: "at", ExpiresAt: 9999999999})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if calls == 0 {
		t.Error("加入账号后应立刻拉取上游")
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if data, _ := resp["data"].([]any); len(data) != 1 {
		t.Errorf("应返回 1 个模型，实际 %v", resp["data"])
	}
}

// TestModelsRetriesAcrossAccounts 单个账号拉取失败时应换号重试。
func TestModelsRetriesAcrossAccounts(t *testing.T) {
	resetModelsCache()

	var goodCalls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		// bad 号（token at-bad）返回 500；good 号返回正常列表。
		if authz == "Bearer at-bad" {
			return 500, `boom`, false
		}
		goodCalls++
		return 200, `{"code":0,"data":{"models":[{"id":"ok-model","maxInputTokens":1000,"maxOutputTokens":100}],"agents":[{"name":"cli","models":["ok-model"]}]}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetBreaker(99, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	data, _ := resp["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("应换号重试拿到模型，实际 %v 条（goodCalls=%d）", len(data), goodCalls)
	}
	if data[0].(map[string]any)["id"] != "ok-model" {
		t.Errorf("model id=%v", data[0])
	}
}
