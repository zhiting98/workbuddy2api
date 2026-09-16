// taskcenter.go 任务中心：全账号任务扫描 + 执行队列（可配并发）。
//
// 语义：
//   - 扫描（ScanAll）：并发拉取每账号的成长任务列表，汇总出「未完成且可自动化」
//     的待办清单（只读，不执行）。
//   - 执行队列（RunQueue + QueueStatus）：把待办项按账号分组排队执行——账号内
//     串行（复用 per-account 锁），账号间并发（信号量限制，默认 1）。队列状态可轮询。
package panel

import (
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ScanAccountItem 单账号扫描结果。
type ScanAccountItem struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname"`
	Growth    []upstream.Task `json:"growth,omitempty"`
	GrowthErr string          `json:"growth_error,omitempty"`
}

// growthPending 任务是否"未完成且可自动化"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		return false
	}
	return autoActionFor(t.TaskCode) != nil
}

// 队列并发度的默认值与上限（与 config.DefaultTaskConcurrency* 同口径）。
//
// 在 panel 内也保留一份兜底：Runner 可能被直接构造（测试 / 其他调用方）
// 而不经过 config 归一，此时 RunQueue 传 0 也要有确定行为。
const (
	DefaultConcurrency    = 10
	DefaultConcurrencyMax = 100
)

// ScanAll 扫描全部账号：成长任务（未完成+可自动化）。只读操作，并发拉取。
func (r *Runner) ScanAll() []ScanAccountItem {
	states := r.pool.List()
	items := make([]ScanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := r.pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.Nickname
			if tasks, err := r.up.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
			} else {
				for _, t := range tasks {
					if growthPending(t) {
						it.Growth = append(it.Growth, t)
					}
				}
			}
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	log.Printf("panel: 队列扫描完成：全部账号待办 %d 项", pending)
	return items
}

// queueItem 队列执行单元。
type QueueItem struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Code     string `json:"code"`
	Status   string `json:"status"` // pending | running | done | skipped | error
	Message  string `json:"message,omitempty"`
}

// QueueStatus 队列状态快照（轮询用）。
type QueueStatus struct {
	Running   bool        `json:"running"`
	Total     int         `json:"total"`
	Conc      int         `json:"conc"`     // 本次队列**实际生效**的并发度
	ConcReq   int         `json:"conc_req"` // 请求的并发度（可能被上限钳制，便于前端如实回显）
	ConcMax   int         `json:"conc_max"` // 当前允许的上限
	Started   bool        `json:"started"`
	StartedAt time.Time   `json:"started_at"`
	Items     []QueueItem `json:"items"`
}

// queueState 队列运行状态。
type queueState struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	items     []QueueItem
	conc      int
	concReq   int
	concMax   int
}

// QueueStatus 返回当前队列状态快照。
func (r *Runner) QueueStatus() QueueStatus {
	q := r.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]QueueItem, len(q.items))
	copy(items, q.items)
	return QueueStatus{
		Running:   q.running,
		Total:     len(items),
		Conc:      q.conc,
		ConcReq:   q.concReq,
		ConcMax:   q.concMax,
		Started:   !q.startedAt.IsZero(),
		StartedAt: q.startedAt,
		Items:     items,
	}
}

// queueAccount 队列执行的账号单元。
type queueAccount struct {
	a    *auth.Auth
	grow []upstream.Task
}

// RunQueue 启动执行队列（全账号，growth 任务）。
//
// concurrency 限**账号间**并发（账号内仍严格串行）；maxConc 为上限（<=0 用默认）。
// 返回 (total, effectiveConcurrency, alreadyRunning)——effective 是**钳制后真正生效**
// 的值，调用方应如实回显，避免"前端传 50、实际跑 4"这类静默偏差。
func (r *Runner) RunQueue(concurrency, maxConc int) (total, effective int, alreadyRunning bool) {
	if maxConc <= 0 {
		maxConc = DefaultConcurrencyMax
	}
	req := concurrency
	if concurrency < 1 {
		concurrency = DefaultConcurrency
	}
	if concurrency > maxConc {
		concurrency = maxConc
	}
	q := r.queue
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return 0, 0, true
	}
	q.mu.Unlock()

	// 扫描待办。
	states := r.pool.List()
	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := r.pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			one := queueAccount{a: a}
			if tasks, err := r.up.ListTasks(a); err == nil {
				for _, t := range tasks {
					if growthPending(t) {
						one.grow = append(one.grow, t)
					}
				}
				sort.Slice(one.grow, func(i, j int) bool {
					return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
				})
			}
			if len(one.grow) > 0 {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()

	// 并发度不必超过实际待办账号数：多出来的 worker 只会空转。
	// 这里**有意不**把 concurrency 回写成缩小值——回显给用户的是"配置生效值"，
	// 而 worker 数按账号数取小，两者语义不同（见 queueState.conc 注释）。
	workers := concurrency
	if workers > len(accts) && len(accts) > 0 {
		workers = len(accts)
	}

	var items []QueueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, QueueItem{UID: one.a.UID, Nickname: one.a.Nickname, Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: 队列启动：无可执行待办（全部账号任务已完成）")
		return 0, 0, false
	}

	q.mu.Lock()
	q.running = true
	q.startedAt = time.Now()
	q.items = items
	q.conc = concurrency // 对外回显：钳制后的配置值
	q.concReq = req
	q.concMax = maxConc
	q.mu.Unlock()

	go r.runQueueItems(accts, items, workers)
	if concurrency != req {
		log.Printf("panel: 队列启动：%d 项（并发 %d，请求 %d 已按上限 %d 调整）", len(items), concurrency, req, maxConc)
	} else {
		log.Printf("panel: 队列启动：%d 项（并发 %d）", len(items), concurrency)
	}
	return len(items), concurrency, false
}

// runQueueItems 队列执行主体：worker pool 消费账号队列。
//
// 账号内串行（一个 worker 包干该账号的全部任务，复用 per-account 锁），
// 账号间并发（worker 数 = 并发度）。
//
// 为什么用固定 worker 数而不是"每账号一个 goroutine + 信号量"：
// 账号数大时（如 50/200）后者会一次性起同样数量的 goroutine 全部阻塞在信号量上，
// 协程数与内存随账号数线性膨胀，而这只是排队等待、毫无收益。worker pool 的
// 协程数与账号数**解耦**，只跟并发度相关。
func (r *Runner) runQueueItems(accts []queueAccount, items []QueueItem, workers int) {
	q := r.queue
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: 队列执行结束（共 %d 项）", len(items))
	}()

	if workers < 1 {
		workers = 1
	}
	jobs := make(chan queueAccount)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for one := range jobs {
				r.runAccountJobs(one, q)
			}
		}()
	}
	for _, one := range accts {
		jobs <- one
	}
	close(jobs)
	wg.Wait()
}

// runAccountJobs 执行单个账号的全部待办（账号内严格串行）。
func (r *Runner) runAccountJobs(one queueAccount, q *queueState) {
	if !r.tryLockAccount(one.a.UID) {
		r.queueSetUID(one.a.UID, func(it *QueueItem) {
			it.Status, it.Message = "skipped", "该账号有其它任务动作在执行，跳过"
		})
		return
	}
	defer r.unlockAccount(one.a.UID)
	for i := range q.items {
		uid := q.snapshotUID(i)
		if uid != one.a.UID {
			continue
		}
		r.queueMarkAt(i, "running", "")
		msg, err := r.runGrowthQueued(one.a, q.snapshotCode(i))
		if err != nil {
			r.queueMarkAt(i, "error", err.Error())
		} else {
			r.queueMarkAt(i, "done", msg)
		}
		time.Sleep(reportGap)
	}
}

func (q *queueState) snapshotUID(i int) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID
}

func (q *queueState) snapshotCode(i int) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].Code
}

func (r *Runner) queueMarkAt(i int, status, msg string) {
	q := r.queue
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

func (r *Runner) queueSetUID(uid string, fn func(*QueueItem)) {
	q := r.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// runGrowthQueued 执行单个成长任务（动作 + 回读 + 自动领奖）。
func (r *Runner) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("任务 %s 无自动动作", code)
	}
	before, err := r.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "该账号无此任务", nil
	}
	if before.Claimed {
		return "已完成（已领取）", nil
	}
	msg, err := act.run(r, a)
	if err != nil {
		return "", err
	}
	after, _ := r.taskByCodeWaiting(a, code)
	if after != nil && after.Claimable {
		if credit, energy, cerr := r.up.ClaimReward(a, code); cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；自动领奖 +%d 分 +%d 能", credit, energy)
		}
	}
	if after != nil {
		msg += "（进度 " + taskProgressText(after) + "）"
	}
	log.Printf("panel: 队列 growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}
