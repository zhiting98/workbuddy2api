package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newKeepaliveS 构造带一个可刷新账号的调度器。
// expiresIn 为上游返回的 accessToken 剩余秒数。
//
// 注意：账号必须带 FilePath，否则 SaveAtomic 会报 "no FilePath set" 而判失败——
// 生产里这个字段由 auth.LoadDir 设置（auth.go:171），测试需自己补上。
func newKeepaliveS(t *testing.T, uid string, expiresIn int64) (*Scheduler, *pool.Pool, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  "new-at-" + uid,
				"refreshToken": "new-rt-" + uid,
				"expiresIn":    expiresIn,
			},
		})
	}))
	t.Cleanup(srv.Close)

	p := pool.New("")
	a := &auth.Auth{UID: uid, AccessToken: "old-at", RefreshToken: "old-rt", ExpiresAt: 1}
	a.FilePath = filepath.Join(t.TempDir(), "workbuddy-"+uid+".json")
	p.Add(a)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p, a
}

// TestKeepaliveAllReturnsOutcomes 保活返回逐账号结果，并回传续期后的到期时间。
func TestKeepaliveAllReturnsOutcomes(t *testing.T) {
	const days = 60
	s, _, a := newKeepaliveS(t, "u1", days*86400)

	out, err := s.KeepaliveAll()
	if err != nil {
		t.Fatalf("KeepaliveAll: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len=%d want 1", len(out))
	}
	oc := out[0]
	if oc.Status != KeepaliveOK {
		t.Errorf("status=%v want ok (detail=%s)", oc.Status, oc.Detail)
	}
	if a.AccessToken != "new-at-u1" {
		t.Errorf("token 未刷新: %q", a.AccessToken)
	}
	// Extends 应约等于 60 天（允许秒级误差）。
	if oc.Extends < days-0.1 || oc.Extends > days+0.1 {
		t.Errorf("extends_days=%.3f want ~%d", oc.Extends, days)
	}
	if oc.ExpiresAt <= time.Now().Unix() {
		t.Errorf("expires_at=%d 应在未来", oc.ExpiresAt)
	}
}

// TestKeepaliveAllSkipsDisabledAndCredentialless 禁用账号与无 refreshToken 的账号跳过，
// **不**打上游、**不**计入失败。
func TestKeepaliveAllSkipsDisabledAndCredentialless(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"x","expiresIn":3600}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	okA := &auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	okA.FilePath = filepath.Join(t.TempDir(), "workbuddy-ok.json")
	p.Add(okA)
	p.Add(&auth.Auth{UID: "nocred", AccessToken: "at", ExpiresAt: 1}) // 无 refreshToken
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	p.Disable("dis", "test")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.KeepaliveAll()
	if err != nil {
		t.Fatalf("KeepaliveAll: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("上游被调用 %d 次，want 1（禁用/无凭证的账号不该打上游）", got)
	}
	byUID := map[string]KeepaliveOutcome{}
	for _, oc := range out {
		byUID[oc.UID] = oc
	}
	if byUID["ok"].Status != KeepaliveOK {
		t.Errorf("ok 账号 status=%v", byUID["ok"].Status)
	}
	if byUID["nocred"].Status != KeepaliveSkipped || byUID["nocred"].Detail != "no credentials" {
		t.Errorf("nocred=%+v want skipped/no credentials", byUID["nocred"])
	}
	if byUID["dis"].Status != KeepaliveSkipped || byUID["dis"].Detail != "disabled" {
		t.Errorf("dis=%+v want skipped/disabled", byUID["dis"])
	}
}

// TestKeepaliveAllBusy 已有一次保活在跑时返回 ErrKeepaliveBusy（与定时撞车）。
// 手动按钮与定时 22:00 必须互斥，否则同账号会被并发 refresh。
func TestKeepaliveAllBusy(t *testing.T) {
	s, _, _ := newKeepaliveS(t, "u1", 3600)
	// 手动持锁，模拟已有一次保活在执行。
	s.keepaliveMu.Lock()
	defer s.keepaliveMu.Unlock()

	if _, err := s.KeepaliveAll(); err != ErrKeepaliveBusy {
		t.Errorf("err=%v want ErrKeepaliveBusy", err)
	}
}

// TestKeepaliveAllReportsFailure 刷新失败如实回报为 fail（不静默吞掉），
// 且 detail 带原因，便于看板定位是哪个号有问题。
func TestKeepaliveAllReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":500,"msg":"server error"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "bad", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, _ := s.KeepaliveAll()
	if len(out) != 1 || out[0].Status != KeepaliveFail {
		t.Fatalf("out=%+v want 1 条 fail", out)
	}
	if strings.TrimSpace(out[0].Detail) == "" {
		t.Error("fail 应带 detail 原因")
	}
}

// TestKeepaliveAllSavesToDisk 刷新成功后必须落盘——否则重启回退到旧 token，
// "保活"就成了假象。用一个真实临时文件验证。
func TestKeepaliveAllSavesToDisk(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"fresh-at","refreshToken":"fresh-rt","expiresIn":86400}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)
	// 先按标准命名落一份旧凭证，再刷新写回。
	if _, err := a.Save(dir); err != nil {
		t.Fatalf("save old: %v", err)
	}

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	if _, err := s.KeepaliveAll(); err != nil {
		t.Fatalf("KeepaliveAll: %v", err)
	}

	// 从磁盘读回，确认是新 token（而非仅内存更新）。
	loaded, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded=%d want 1", len(loaded))
	}
	if loaded[0].AccessToken != "fresh-at" {
		t.Errorf("磁盘上 accessToken=%q want fresh-at（保活必须落盘）", loaded[0].AccessToken)
	}
	if loaded[0].RefreshToken != "fresh-rt" {
		t.Errorf("磁盘上 refreshToken=%q want fresh-rt", loaded[0].RefreshToken)
	}
}

// TestRunKeepaliveNowStillWorks RunKeepaliveNow 是定时入口，改成委托后行为不变
// （既有测试依赖它，这里显式锚定一次）。
func TestRunKeepaliveNowStillWorks(t *testing.T) {
	s, _, a := newKeepaliveS(t, "u1", 3600)
	s.RunKeepaliveNow()
	if a.AccessToken != "new-at-u1" {
		t.Errorf("RunKeepaliveNow 未刷新 token: %q", a.AccessToken)
	}
}
