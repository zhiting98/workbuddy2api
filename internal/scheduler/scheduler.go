// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive 四类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即四类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool

	// TaskHours 任务自动化触发时点（默认 [10]，与签到错开）。
	TaskHours []int

	// RunTasks 定时触发的任务自动化回调（可选；nil = 不执行任务自动化）。
	// 幂等：已 claimed 的任务跳过；per-account 锁与手动执行互斥。
	RunTasks func()
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// switchMu 保护下面四个**运行时可改**的开关。
	//
	// 为什么不能直接用 cfg.*Disabled：那些是构造时定死的值，而看板要能在运行中
	// 开关任务。改 cfg 需要加锁且 cfg 是值类型（Run 循环会并发读），故单独抽出来。
	// cfg 里的 *Disabled 仅作为**初始值**（见 New）。
	switchMu     sync.RWMutex
	checkinOff   bool
	travelOff    bool
	activityOff  bool
	keepaliveOff bool
	// wake 用于打断 Run 的休眠：开关变化时必须立刻重算下一次唤醒时刻，
	// 否则要等当前 sleep 到期（最长可达 12 小时）才生效。
	// 带缓冲 1：重复通知只保留一个，不阻塞调用方。
	wake chan struct{}

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex
	// keepaliveMu 串行化保活（同上，手动按钮与定时 22:00 撞车时只有一个在跑）。
	keepaliveMu sync.Mutex
}

// TaskSwitch 一类定时任务的开关状态（供看板展示与切换）。
type TaskSwitch struct {
	// Key 任务标识：checkin / travel / activity / keepalive。
	Key string `json:"key"`
	// Name 展示名。
	Name string `json:"name"`
	// Enabled 当前是否启用。
	Enabled bool `json:"enabled"`
	// Hours 触发时点（本地小时，0-23）。禁用时仍保留——改变开关不清除时点配置。
	Hours []int `json:"hours"`
	// ConfigKey 对应的 config.json 字段名，便于前端提示"写回哪个键"。
	ConfigKey string `json:"config_key"`
}

// TaskSwitches 返回四类任务的当前开关与时点（顺序固定，便于前端稳定渲染）。
func (s *Scheduler) TaskSwitches() []TaskSwitch {
	s.switchMu.RLock()
	defer s.switchMu.RUnlock()
	return []TaskSwitch{
		{Key: "checkin", Name: "签到", Enabled: !s.checkinOff,
			Hours: append([]int(nil), s.cfg.CheckinHours...), ConfigKey: "checkin_enabled"},
		{Key: "travel", Name: "猫猫旅行", Enabled: !s.travelOff,
			Hours: append([]int(nil), s.cfg.TravelHours...), ConfigKey: "travel_enabled"},
		{Key: "activity", Name: "活跃上报", Enabled: !s.activityOff,
			Hours: append([]int(nil), s.cfg.ActivityHours...), ConfigKey: "activity_enabled"},
		{Key: "keepalive", Name: "Token 保活", Enabled: !s.keepaliveOff,
			Hours: append([]int(nil), s.cfg.KeepaliveHours...), ConfigKey: "keepalive_enabled"},
	}
}

// SetTaskEnabled 运行中启用/禁用一类任务，并唤醒调度循环重算下一次唤醒时刻。
// 未知 key 返回 false（调用方据此报 400）。
//
// 幂等：设置为当前值也安全（只多一次无谓的唤醒重算）。
func (s *Scheduler) SetTaskEnabled(key string, enabled bool) bool {
	s.switchMu.Lock()
	switch key {
	case "checkin":
		s.checkinOff = !enabled
	case "travel":
		s.travelOff = !enabled
	case "activity":
		s.activityOff = !enabled
	case "keepalive":
		s.keepaliveOff = !enabled
	default:
		s.switchMu.Unlock()
		return false
	}
	s.switchMu.Unlock()

	// 唤醒 Run 重算：开→关要立刻取消已排的时点；关→开要立刻排上。
	// 非阻塞发送：无人在等（如 Run 未启动）时直接丢弃，不阻塞调用方。
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.TaskHours) == 0 {
		cfg.TaskHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	s := &Scheduler{
		cfg:        cfg,
		adoptTried: make(map[string]string),
		// 运行时开关以 cfg 为**初始值**；此后由 SetTaskEnabled 独立维护，
		// 不回写 cfg（cfg 是值类型，Run 循环会并发读，改它需额外同步）。
		checkinOff:   cfg.CheckinDisabled,
		travelOff:    cfg.TravelDisabled,
		activityOff:  cfg.ActivityDisabled,
		keepaliveOff: cfg.KeepaliveDisabled,
		wake:         make(chan struct{}, 1),
	}
	return s
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// ErrKeepaliveBusy 已有一次保活正在执行（手动入口与定时撞车）。
var ErrKeepaliveBusy = errors.New("keepalive already running")

// KeepaliveStatus 单账号保活结果。
type KeepaliveStatus string

const (
	KeepaliveOK      KeepaliveStatus = "ok"      // 刷新成功并落盘
	KeepaliveSkipped KeepaliveStatus = "skipped" // 禁用账号 / 无 refreshToken，未参与
	KeepaliveFail    KeepaliveStatus = "fail"    // 刷新或落盘失败
)

// KeepaliveOutcome 单账号保活结果（供手动保活回执与日志汇总）。
// ExpiresAt 是刷新后的 accessToken 到期时间（Unix 秒；刷新失败时为原值）。
type KeepaliveOutcome struct {
	UID       string          `json:"uid"`
	Nickname  string          `json:"nickname,omitempty"`
	Status    KeepaliveStatus `json:"status"`
	ExpiresAt int64           `json:"expires_at,omitempty"`
	// Extends 刷新后的剩余天数（前端直接显示"延长到 N 天"）。
	Extends float64 `json:"extends_days,omitempty"`
	Detail  string  `json:"detail,omitempty"`
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskAutoTask
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
//
// 开关读的是**运行时可改**状态（不看 cfg.*Disabled），这样看板切换后立即生效。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.switchMu.RLock()
	checkinOff, travelOff := s.checkinOff, s.travelOff
	activityOff, keepaliveOff := s.activityOff, s.keepaliveOff
	s.switchMu.RUnlock()

	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	if s.cfg.RunTasks != nil {
		slots = append(slots, slot{nextFire(now, s.cfg.TaskHours), taskAutoTask})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// Run 主循环，阻塞直到 ctx 取消。
//
// 除「时点到期」与「ctx 取消」外，还监听 s.wake：看板切换任务开关时立即重算
// 下一次唤醒时刻。否则关掉的任务可能仍会在已排的时点触发（最长等一个间隔），
// 而新打开的任务要等当前 sleep 到期（最长 12 小时）才排上——开关形同虚设。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 全部任务禁用（且无任务自动化）：不空转，等退出信号**或**开关变化。
			// 必须同时等 wake，否则"全关后在界面上重新打开"将永远不生效。
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue // 重算：可能已有关闭的任务被重新启用
			}
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			// 开关变化：丢弃本次排程，立即重算（未到点的任务不会被执行）。
			timer.Stop()
			continue
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			for _, k := range kinds {
				switch k {
				case taskCheckin:
					s.RunCheckinNow()
				case taskTravel:
					s.RunTravelNow()
				case taskActivity:
					s.RunActivityNow()
				case taskKeepalive:
					s.RunKeepaliveNow()
				case taskAutoTask:
					s.RunTasksNow()
				}
			}
		}
	}
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
func (s *Scheduler) RunCheckinNow() {
	if _, err := s.CheckinAll(); err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
	}
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.UID8(st.UID), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else if err := a.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
				log.Printf("checkin %s save: %v", logfmt.UID8(st.UID), err)
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
				// 出现一整段 400 报文、被误读成签到失败。
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.UID8(st.UID), err)
			}
		} else {
			oc.Status = CheckinOK
		}
		remain, err := s.cfg.Upstream.UserResource(a)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.UID8(st.UID), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （wb2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
func (s *Scheduler) RunActivityNow() {
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessToken == "" {
			continue
		}
		if !first {
			time.Sleep(activityAccountDelay)
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.UID8(a.UID), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.UID8(a.UID), i, count)
			ok++
			if i < count {
				time.Sleep(activityReportGap) // 账号内 5 条之间间隔，避免秒发风控
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)    // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.UID8(a.UID), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.UID8(a.UID))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.UID8(a.UID), days)
	return false
}

// RunKeepaliveNow 定时触发的立即保活：逐账号结果由 KeepaliveAll 汇总，
// 此处只兜住"撞车跳过"（与 RunCheckinNow 同口径）。
func (s *Scheduler) RunKeepaliveNow() {
	if _, err := s.KeepaliveAll(); err != nil {
		log.Printf("scheduled keepalive skipped: %v", err)
	}
}

// KeepaliveAll 全量保活：逐账号刷新 token 并原子落盘。
//
// session 死亡走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
//
// 同一时刻只允许一次保活在跑：手动按钮与定时 22:00 撞车时返回 ErrKeepaliveBusy，
// 避免同一账号被并发 refresh（同账号并发刷新可能触发上游风控，且会写坏 token 文件）。
func (s *Scheduler) KeepaliveAll() ([]KeepaliveOutcome, error) {
	if !s.keepaliveMu.TryLock() {
		return nil, ErrKeepaliveBusy
	}
	defer s.keepaliveMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]KeepaliveOutcome, 0, len(statuses))
	var okN, failN, skipN int
	for _, st := range statuses {
		oc := KeepaliveOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = KeepaliveSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			oc.Status, oc.Detail = KeepaliveSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.UID8(st.UID), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.UID8(st.UID), pool.SessionDeadThreshold())
				}
			}
			oc.Status, oc.Detail = KeepaliveFail, err.Error()
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		if err := a.SaveAtomic(); err != nil {
			// 刷新成功但落盘失败：内存是新 token、磁盘还是旧的，重启后会回退。
			// 如实报失败，不当作成功（否则用户以为保活已生效）。
			log.Printf("keepalive %s save: %v", logfmt.UID8(st.UID), err)
			oc.Status, oc.Detail = KeepaliveFail, "刷新成功但落盘失败: "+err.Error()
			failN++
			out = append(out, oc)
			continue
		}
		exp := a.ExpiresAtUnix()
		oc.Status = KeepaliveOK
		oc.ExpiresAt = exp
		oc.Extends = time.Until(time.Unix(exp, 0)).Hours() / 24
		okN++
		out = append(out, oc)
	}
	log.Printf("[keepalive] 保活完成：成功 %d / 失败 %d / 跳过 %d（共 %d）", okN, failN, skipN, len(out))
	return out, nil
}

// RunTasksNow 定时触发的任务自动化（幂等回调；未注入则静默跳过）。
func (s *Scheduler) RunTasksNow() {
	if s.cfg.RunTasks == nil {
		return
	}
	s.cfg.RunTasks()
}
