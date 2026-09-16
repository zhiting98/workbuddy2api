package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// fakeUpstream 假上游：回答任务列表 + 上报/领奖类请求。
//
// 关键能力是**记录同时在途的请求数峰值**，用来证明并发真的生效
// （而不是只看代码"应该并行了"）。
//
// 注意：扫描阶段（RunQueue 里并发拉每账号任务列表）本身也是并发的，
// 会污染"队列执行并发"的观测。因此提供 scanHold/execHold 两档延迟 +
// resetPeak，让测试能把两个阶段分开测量。
type fakeUpstream struct {
	mu       sync.Mutex
	inFlight int
	maxSeen  int32 // 峰值（atomic，便于断言）
	// scanHold 任务列表请求的延迟；execHold 执行类请求的延迟。
	scanHold time.Duration
	execHold time.Duration
	// taskCodes 每个账号返回的待办任务 code。
	taskCodes []string
}

func (f *fakeUpstream) enter() {
	f.mu.Lock()
	f.inFlight++
	cur := f.inFlight
	f.mu.Unlock()
	for {
		old := atomic.LoadInt32(&f.maxSeen)
		if int32(cur) <= old || atomic.CompareAndSwapInt32(&f.maxSeen, old, int32(cur)) {
			break
		}
	}
}
func (f *fakeUpstream) leave() {
	f.mu.Lock()
	f.inFlight--
	f.mu.Unlock()
}
func (f *fakeUpstream) peak() int { return int(atomic.LoadInt32(&f.maxSeen)) }

// resetPeak 归零峰值计数（用于把扫描阶段与执行阶段分开观测）。
func (f *fakeUpstream) resetPeak() { atomic.StoreInt32(&f.maxSeen, 0) }

// isListTasks 判断是否任务列表请求（据此选延迟档）。
func isListTasks(r *http.Request) bool { return strings.Contains(r.URL.Path, "tasks") }

// newFakeServer 起假上游并返回 client + 观测器。
func newFakeServer(t *testing.T, taskCodes []string, scanHold, execHold time.Duration) (*upstream.Client, *fakeUpstream) {
	t.Helper()
	f := &fakeUpstream{taskCodes: taskCodes, scanHold: scanHold, execHold: execHold}
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.enter()
		defer f.leave()
		if isListTasks(r) {
			if f.scanHold > 0 {
				time.Sleep(f.scanHold)
			}
		} else if f.execHold > 0 {
			time.Sleep(f.execHold)
		}
		w.Header().Set("Content-Type", "application/json")

		if isListTasks(r) {
			// 任务列表形态：{data:{tasks:[...]}}；accept_status 非 claimed、target/current 全 0
			// → growthPending 判为"待办"，驱动队列真的去跑动作。
			if len(f.taskCodes) == 0 {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"tasks": []any{}}})
				return
			}
			tasks := make([]map[string]any, 0, len(f.taskCodes))
			for _, c := range f.taskCodes {
				tasks = append(tasks, map[string]any{
					"task_code":     c,
					"title":         c,
					"accept_status": "accepted",
					"target":        0,
					"current":       0,
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"tasks": tasks},
			})
			return
		}
		// 执行类请求（上报/领奖）：一律成功，让队列推进。
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// 让上游基址指向假服务：chatBase 直接返回 ChatBaseCN。
	up := upstream.New()
	up.ChatBaseCN = srv.URL
	up.BillingBaseCN = srv.URL
	up.WebBaseCN = srv.URL
	return up, f
}

// newRunnerWithAccounts 构造带 N 个账号的 Runner（假 token，不联网）。
func newRunnerWithAccounts(t *testing.T, up *upstream.Client, n int) *Runner {
	t.Helper()
	dir := t.TempDir()
	var auths []*auth.Auth
	for i := 0; i < n; i++ {
		a := &auth.Auth{
			AccessToken: fmt.Sprintf("tok-%d", i),
			UID:         fmt.Sprintf("uid-%03d", i),
			Nickname:    fmt.Sprintf("acct-%d", i),
			ExpiresAt:   time.Now().Add(24 * time.Hour).Unix(),
		}
		auths = append(auths, a)
	}
	p := pool.New(dir + "/state.json")
	p.SyncToDir(auths)
	return NewRunner(p, up)
}

// waitQueueDone 轮询等待队列结束；返回是否在超时前结束。
func waitQueueDone(t *testing.T, r *Runner, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !r.QueueStatus().Running {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestRunQueueClampsToMax 并发度按上限钳制，并**如实回传生效值**。
func TestRunQueueClampsToMax(t *testing.T) {
	up, _ := newFakeServer(t, []string{"chat_5"}, 0, 0)
	r := newRunnerWithAccounts(t, up, 1)

	total, eff, running := r.RunQueue(500, DefaultConcurrencyMax)
	if running {
		t.Fatal("队列不应已在运行")
	}
	if total == 0 {
		t.Fatal("应扫描出待办")
	}
	if eff != DefaultConcurrencyMax {
		t.Errorf("effective=%d want %d（超上限应钳到上限）", eff, DefaultConcurrencyMax)
	}
	st := r.QueueStatus()
	if st.ConcReq != 500 {
		t.Errorf("ConcReq=%d want 500", st.ConcReq)
	}
	if st.Conc != DefaultConcurrencyMax {
		t.Errorf("Conc=%d want %d", st.Conc, DefaultConcurrencyMax)
	}
	if st.ConcMax != DefaultConcurrencyMax {
		t.Errorf("ConcMax=%d want %d", st.ConcMax, DefaultConcurrencyMax)
	}
	waitQueueDone(t, r, 30*time.Second)
}

// TestRunQueueDefaultsWhenZero 未指定并发（0/负数）回落默认 10。
func TestRunQueueDefaultsWhenZero(t *testing.T) {
	for _, in := range []int{0, -3} {
		up, _ := newFakeServer(t, []string{"chat_5"}, 0, 0)
		r := newRunnerWithAccounts(t, up, 1)
		_, eff, _ := r.RunQueue(in, DefaultConcurrencyMax)
		if eff != DefaultConcurrency {
			t.Errorf("in=%d effective=%d want %d", in, eff, DefaultConcurrency)
		}
		waitQueueDone(t, r, 30*time.Second)
	}
}

// TestQueueRealConcurrency 核心行为测试：并发度 N 时，**执行阶段同时在途的请求数应 > 1**。
//
// 这是对"worker pool 真的并行了"的实证，而不是对实现的复述：
// 用假上游记录在途峰值，串行实现下峰值必为 1。
//
// 关键：扫描阶段本身就是并发的（RunQueue 并发拉每账号任务列表），
// 所以必须在扫描完成后 resetPeak()，只观测"队列执行"这一段。
func TestQueueRealConcurrency(t *testing.T) {
	const accts = 6
	up, f := newFakeServer(t, []string{"chat_5"}, 300*time.Millisecond, 200*time.Millisecond)
	r := newRunnerWithAccounts(t, up, accts)

	_, eff, running := r.RunQueue(accts, DefaultConcurrencyMax)
	if running {
		t.Fatal("队列不应已在运行")
	}
	if eff != accts {
		t.Fatalf("effective=%d want %d", eff, accts)
	}
	// 此刻扫描已结束（RunQueue 内部 wg.Wait 后才启 worker），刷新观测窗口。
	f.resetPeak()

	if !waitQueueDone(t, r, 40*time.Second) {
		t.Fatal("队列未在预期时间内结束")
	}
	if peak := f.peak(); peak < 2 {
		t.Errorf("执行阶段在途峰值=%d，并发未生效（串行实现下应恒为 1）", peak)
	} else {
		t.Logf("执行阶段在途峰值=%d（并发 %d 账号），确认并行生效", peak, accts)
	}
}

// TestQueueSerialWhenConcurrencyOne 并发 1 时执行阶段在途峰值应恒为 1（对照组）。
//
// 超时给得宽：单个 chat_5 任务本身就要 ~15s（5 条上报 × reportGap 1.05s
// + 达标回读 claimPollGap 3s×最多 4 次），4 个账号串行需 ~60s。
// 这里恰好也印证了"账号内串行"的语义。
func TestQueueSerialWhenConcurrencyOne(t *testing.T) {
	const accts = 4
	up, f := newFakeServer(t, []string{"chat_5"}, 200*time.Millisecond, 150*time.Millisecond)
	r := newRunnerWithAccounts(t, up, accts)

	if _, _, running := r.RunQueue(1, DefaultConcurrencyMax); running {
		t.Fatal("队列不应已在运行")
	}
	f.resetPeak() // 只观测执行阶段
	if !waitQueueDone(t, r, 180*time.Second) {
		t.Fatal("队列未在预期时间内结束")
	}
	if peak := f.peak(); peak != 1 {
		t.Errorf("并发 1 时执行阶段在途峰值=%d want 1（账号间不应并行）", peak)
	} else {
		t.Log("并发 1：执行阶段在途峰值=1，确认串行")
	}
}

// TestRunQueueAlreadyRunning 队列执行中重复启动应被拒绝。
func TestRunQueueAlreadyRunning(t *testing.T) {
	up, _ := newFakeServer(t, []string{"chat_5"}, 0, 300*time.Millisecond)
	r := newRunnerWithAccounts(t, up, 2)

	if _, _, running := r.RunQueue(2, DefaultConcurrencyMax); running {
		t.Fatal("首次启动不应报告 already running")
	}
	if _, _, running := r.RunQueue(2, DefaultConcurrencyMax); !running {
		t.Error("执行中重复启动应返回 alreadyRunning=true")
	}
	waitQueueDone(t, r, 30*time.Second)
}

// TestRunQueueNoPending 无待办时 total=0 且不置 running。
func TestRunQueueNoPending(t *testing.T) {
	up, _ := newFakeServer(t, nil, 0, 0) // 空任务列表
	r := newRunnerWithAccounts(t, up, 2)
	total, eff, running := r.RunQueue(4, DefaultConcurrencyMax)
	if total != 0 {
		t.Errorf("total=%d want 0", total)
	}
	if eff != 0 {
		t.Errorf("effective=%d want 0（无待办不应报告并发）", eff)
	}
	if running {
		t.Error("无待办不应是 alreadyRunning")
	}
}
