// e2e_harness.go 端到端验证用：把真实 server.Handler 接到假上游上起一个 HTTP 服务，
// 用 curl 验证 /v1/responses 与 /v1/messages 的真实 SSE 事件序列。
//
// 这不是生产代码，仅供离线端到端验证（清单 §4）。它复用与 cmd/server 相同的
// handler 装配路径（pool + session + upstream client），只把上游 base URL
// 指向本地假上游。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func main() {
	authDir := os.Getenv("E2E_AUTH_DIR")
	stateFile := os.Getenv("E2E_STATE_FILE")
	stub := os.Getenv("E2E_STUB_BASE") // 如 http://127.0.0.1:18777
	listen := os.Getenv("E2E_LISTEN")
	mode := os.Getenv("E2E_PROMPT_MODE")
	promptText := os.Getenv("E2E_PROMPT_TEXT")
	if mode == "" {
		mode = "passthrough"
	}

	auths, err := auth.LoadDir(authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s)", len(auths))

	p := pool.New(stateFile)
	defer p.Flush()
	p.SetStore(redisstore.Noop{})
	p.SyncToDir(auths)
	p.SetBreaker(3, 30*time.Minute, 6*time.Hour)
	p.SetMaxInFlight(3)
	p.SetSoftRateMax(2 * time.Hour)
	p.SetWeights(0.5, 5.0)

	sessRouter := session.New(session.Config{
		TTL: 30 * time.Minute, GCInterval: 5 * time.Minute,
		Store: redisstore.Noop{}, Available: p.AvailableUIDs,
	})
	sessRouter.StartGC()
	defer sessRouter.StopGC()

	up := upstream.New()
	up.ChatBaseCN = stub
	up.BillingBaseCN = stub
	up.HTTP.Timeout = 30 * time.Second
	up.IdleTimeout = 60 * time.Second
	up.SanitizeFingerprints = true

	stats := server.NewStats(filepath.Join(filepath.Dir(stateFile), "stats.json"))
	defer stats.Flush()

	// 保活入口：与 cmd/server 同一条装配路径（scheduler.KeepaliveAll），
	// 使 /keepalive 端点可在离线环境端到端验证（假上游返回新 token）。
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up, KeepaliveHours: []int{22}})

	h := server.NewHandler(server.Config{
		Pool: p, Upstream: up, APIKey: "sk-e2e-test",
		Session: sessRouter, StickyCount: sessRouter.Count,
		RedisMode: "noop", SoftCooldown: 600 * time.Second,
		PromptMode: mode, PromptText: promptText,
		MaxBodyBytes: 8 << 20, Stats: stats,
		Keepalive: func() ([]server.KeepaliveOutcome, error) {
			items, err := sch.KeepaliveAll()
			if err != nil {
				return nil, err
			}
			out := make([]server.KeepaliveOutcome, 0, len(items))
			for _, it := range items {
				out = append(out, server.KeepaliveOutcome{
					UID: it.UID, Nickname: it.Nickname, Status: string(it.Status),
					ExpiresAt: it.ExpiresAt, Extends: it.Extends, Detail: it.Detail,
				})
			}
			return out, nil
		},
	})

	log.Printf("e2e harness listening on %s (prompt_mode=%s)", listen, mode)
	srv := &http.Server{Addr: listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		time.Sleep(30 * time.Second)
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("harness bye")
}
