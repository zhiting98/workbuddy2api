// Package panel 任务自动化 Runner：任务中心扫描 + 一键完成 + 执行队列。
//
// 不依赖 HTTP handler / mux / 登录会话——被 internal/server 当作纯依赖注入，
// 由 server 包暴露 /tasks/* 接口；定时触发由 scheduler 调用 Runner.RunQueue。
//
// 语义：
//   - per-account 互斥：同一账号的任务动作（单任务/全量/队列项）串行，
//     重复触发返回「仍在执行」。
//   - 账号内串行、账号间并发（执行队列用信号量限制并发）。
package panel

import (
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Runner 任务自动化执行器。
type Runner struct {
	pool *pool.Pool
	up   *upstream.Client

	// taskMu/taskLocks per-account 互斥：同一账号的任务动作同时只允许一条在跑。
	taskMu    sync.Mutex
	taskLocks map[string]*sync.Mutex

	// 队列状态（taskcenter.go）。
	queueMu sync.Mutex
	queue   *queueState
}

// NewRunner 构建 Runner。pool/up 必须非 nil。
func NewRunner(pool *pool.Pool, up *upstream.Client) *Runner {
	return &Runner{
		pool:      pool,
		up:        up,
		taskLocks: map[string]*sync.Mutex{},
		queue:     &queueState{},
	}
}

// tryLockAccount 尝试锁定账号的任务执行；已在执行返回 false。
func (r *Runner) tryLockAccount(uid string) bool {
	r.taskMu.Lock()
	mu := r.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		r.taskLocks[uid] = mu
	}
	r.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount 释放账号任务锁（与 tryLockAccount 配对）。
func (r *Runner) unlockAccount(uid string) {
	r.taskMu.Lock()
	mu := r.taskLocks[uid]
	r.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// accountByUID 取账号凭证；不存在返回 nil。
func (r *Runner) accountByUID(uid string) *auth.Auth {
	return r.pool.AuthByUID(uid)
}

// AccountByUID 导出单账号凭证（供 server /tasks/list 用）；不存在返回 nil。
func (r *Runner) AccountByUID(uid string) *auth.Auth {
	return r.accountByUID(uid)
}

// reportGap 连续上报之间的间隔（对齐上游脚本实测口径，避免风控）。
var reportGap = 1050 * time.Millisecond

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数（上游计分异步）。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)
