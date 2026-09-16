// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool     *pool.Pool
	Upstream *upstream.Client
	APIKey   string // 单值鉴权密钥（历史字段；空且 APIKeys 也空 = 不鉴权）
	// APIKeys config 提供的多个鉴权密钥（与 APIKey 合并生效）。
	// 看板生成的 key 另存在 APIKeyPath 指向的文件里，两者也合并生效。
	APIKeys []string
	// APIKeyPath 看板生成的 key 的持久化路径（如 data/api_keys.json）。
	// 空 = 仅内存（不落盘）：看板仍能生成 key，但重启即失效，故生产应配置。
	APIKeyPath string
	MaxRotate  int // 单请求最多换号次数，默认 3
	// MaxBodyBytes 聊天请求体大小上限；<=0 兜底 8<<20（8MB）。
	// 超限直接 413 request_body_too_large（不再静默截断喂给上游，issue #41）。
	MaxBodyBytes int64
	// AuthDir 账号凭证目录（auths/）。供「添加账号」写入 workbuddy-<uid>.json；
	// 为空时该功能返回 503，其余端点不受影响。
	AuthDir string
	// DashboardUser/DashboardPass 看板登录凭据（HTTP Basic Auth）。
	// 两者都非空时启用内置看板（同容器提供页面 + /api/* 转发）；
	// 任一为空则不注册看板路由（退化为纯 API 网关）。
	//
	// 与 APIKey 分离的原因：看板是给人用的管理界面，api_key 是给程序用的；
	// 分开后可以把看板密码交给运维而不必交出 API 密钥。
	DashboardUser string
	DashboardPass string
	// SetupConfigPath config.json 路径，供首次安装向导把凭据写回磁盘。
	// 为空时向导仍可用，但只改内存（会明确提示重启后失效）。
	SetupConfigPath string
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	Stats *Stats // token 用量统计（可选；nil = 不统计）
	// CreditTrack 积分快照趋势（可选；nil = 不启用 /credits/history）。
	CreditTrack *CreditTrack
	// CallTrack 调用流水（可选；nil = 不记录 /calls）。
	CallTrack *CallTrack

	// Tasks 任务自动化运行器（可选；nil = /tasks/* 接口返回 501）。
	Tasks *panel.Runner
	// TaskConcurrency / TaskConcurrencyMax 任务队列并发度与上限（<=0 回落 panel 默认）。
	// 默认 10 / 100，对应 config.schedule.task_concurrency。
	TaskConcurrency    int
	TaskConcurrencyMax int

	// Keepalive 手动保活入口（可选；nil = /keepalive 返回 501）。
	//
	// 为什么注入函数而不是 *scheduler.Scheduler：scheduler 依赖 pool，
	// 而 server 也依赖 pool——直接引用会让 server→scheduler→pool 与既有装配方向
	// 产生不必要的耦合。函数注入只表达"能触发一次全量保活"这一个能力，
	// 与 Tasks（panel.Runner）的注入风格一致，也便于测试替换。
	// 返回 (逐账号结果, 错误)；错误为 scheduler.ErrKeepaliveBusy 表示已有一次在跑。
	Keepalive func() ([]KeepaliveOutcome, error)

	// TaskSwitches / SetTaskEnabled 定时任务开关的读取与运行时切换
	// （可选；nil = /schedule 返回 501）。同样是函数注入：server 不反向依赖 scheduler。
	TaskSwitches func() []TaskSwitch
	// SetTaskEnabled 切换一类任务，返回 false 表示 key 未知。
	SetTaskEnabled func(key string, enabled bool) bool
	// PersistScheduleSwitch 把开关写回 config.json（可选；nil = 不落盘，仅内存生效）。
	PersistScheduleSwitch func(key string, enabled bool) error
}

// TaskSwitch 一类定时任务的开关状态（由 scheduler 侧转换而来，字段与其保持一致）。
type TaskSwitch struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	Hours     []int  `json:"hours"`
	ConfigKey string `json:"config_key"`
}

// KeepaliveOutcome 是保活单账号结果的最小视图（由 scheduler 侧转换而来，
// 避免 server 包反向依赖 scheduler 的具体类型）。
type KeepaliveOutcome struct {
	UID       string  `json:"uid"`
	Nickname  string  `json:"nickname,omitempty"`
	Status    string  `json:"status"`
	ExpiresAt int64   `json:"expires_at,omitempty"`
	Extends   float64 `json:"extends_days,omitempty"`
	Detail    string  `json:"detail,omitempty"`
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// logins 看板「添加账号」的登录会话表（见 accounts.go）。
	logins *loginSessions
	// setup 首次安装向导运行态（见 setup.go）。
	setup setupState
	// dashboardOnce 保证 /api/* 只注册一次（安装完成后动态注册时用）。
	dashboardOnce sync.Once
	// keys 多 API Key 存储（见 keys.go）。永不为 nil——NewHandler 保证初始化。
	keys *keyStore
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 8 << 20 // 请求体上限兜底 8MB
	}
	// 多 key 存储：config 提供的 key 与看板生成的 key 合并生效。
	// **必须把 cfg.APIKey 也一并纳入**——authOK 只问 keys，不再直接读 cfg.APIKey；
	// 若只传 APIKeys，那么"只配了单值 api_key"的老配置与全部既有测试都会失效
	// （从"需要鉴权"变成"完全不鉴权"，等于静默敞开）。
	// APIKeyPath 为空时仅内存运行（不落盘），便于测试。
	cfgKeys := cfg.APIKeys
	if strings.TrimSpace(cfg.APIKey) != "" {
		cfgKeys = append([]string{cfg.APIKey}, cfgKeys...)
	}
	h := &Handler{
		cfg:    cfg,
		mux:    http.NewServeMux(),
		logins: newLoginSessions(),
		setup:  setupState{configPath: cfg.SetupConfigPath},
		keys:   newKeyStore(cfg.APIKeyPath, cfgKeys),
	}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responses)) // OpenAI Responses API（Codex v0.154+ 只认此端点）
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.messages))   // Anthropic Messages API（Claude Code 只认此端点）
	// 兼容不带 /v1 前缀的写法（见下方 registerAliases 注释）。
	h.registerNoV1Aliases()
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /credits", h.withAuth(h.credits))
	h.mux.HandleFunc("GET /stats", h.withAuth(h.stats))
	h.mux.HandleFunc("GET /credits/history", h.withAuth(h.creditsHistory))
	h.mux.HandleFunc("GET /calls", h.withAuth(h.calls))
	h.mux.HandleFunc("GET /tasks/scan", h.withAuth(h.tasksScan))
	h.mux.HandleFunc("POST /tasks/run", h.withAuth(h.tasksRun))
	h.mux.HandleFunc("POST /tasks/queue", h.withAuth(h.tasksQueueStart))
	h.mux.HandleFunc("GET /tasks/queue", h.withAuth(h.tasksQueueStatus))
	h.mux.HandleFunc("GET /tasks/list", h.withAuth(h.tasksList))
	// 手动保活：立即刷新全部账号 token（与定时 22:00 共用同一实现与互斥）。
	h.mux.HandleFunc("POST /keepalive", h.withAuth(h.keepaliveRun))
	// API Key 管理（多 key 并行有效）。走与账号管理相同的 api_key 鉴权；
	// 看板页面经 /api/* 转发时已通过 Basic Auth（dashAuthed），故无需内嵌密钥。
	h.mux.HandleFunc("GET /keys", h.withAuth(h.keysList))
	h.mux.HandleFunc("POST /keys", h.withAuth(h.keysCreate))
	h.mux.HandleFunc("DELETE /keys/{key}", h.withAuth(h.keysDelete))
	h.mux.HandleFunc("POST /keys/{key}/disable", h.withAuth(h.keysSetEnabled(false)))
	h.mux.HandleFunc("POST /keys/{key}/enable", h.withAuth(h.keysSetEnabled(true)))
	// 定时任务开关（看板可运行时切换，立即生效 + 写回 config.json）。
	h.mux.HandleFunc("GET /schedule", h.withAuth(h.scheduleList))
	h.mux.HandleFunc("POST /schedule/{key}", h.withAuth(h.scheduleSet))
	// 看板账号管理：OAuth 添加账号（凭证不经过浏览器）+ 批量导入 + 复活被禁账号。
	h.mux.HandleFunc("POST /accounts/login/start", h.withAuth(h.accountsLoginStart))
	h.mux.HandleFunc("GET /accounts/login/poll", h.withAuth(h.accountsLoginPoll))
	h.mux.HandleFunc("POST /accounts/import", h.withAuth(h.accountsImport))
	// 导出完整凭证（含明文 token）用于备份/迁移；需显式确认短语，见 accountsExport。
	h.mux.HandleFunc("POST /accounts/export", h.withAuth(h.accountsExport))
	h.mux.HandleFunc("POST /accounts/{uid}/revive", h.withAuth(h.accountsRevive))
	h.mux.HandleFunc("DELETE /accounts/{uid}", h.withAuth(h.accountsDelete))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// 根路径 `/`：按运行态分发（未安装 → 安装向导；已安装 → 看板）。
	h.registerRoot()
	// 内置看板数据入口（/api/*，Basic Auth）。未配置凭据时不注册。
	h.registerDashboard()
	// 首次安装向导：仅在**未配置看板凭据**时注册（装好后这些路由不存在 → 404）。
	h.registerSetup()
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// registerNoV1Aliases 注册不带 `/v1` 前缀的端点别名。
//
// 背景：客户端把 base_url **原样拼接**端点路径，而各家对 base_url 是否含 `/v1`
// 的约定不一致：
//
//	Codex    base_url="http://host:7863"    → POST /responses     ← 别名命中
//	Codex    base_url="http://host:7863/v1" → POST /v1/responses  ← 正式路径
//	Claude   ANTHROPIC_BASE_URL 通常含 /v1  → POST /v1/messages
//
// 社区工具（如 cc-switch）生成的配置有的带 `/v1` 有的不带，用户遇到 404 时
// 很难判断是"网关缺路由"还是"base_url 少写了 /v1"。两种都接受，
// 代价只是一条别名路由，收益是不必让每个人去猜。
//
// 注意：只对 `/responses`、`/messages` 做别名（这两个是客户端唯一入口）；
// `/v1/models`、`/v1/chat/completions` 保持原样，避免与看板的 `/api/*` 等
// 路径产生意外重叠。
func (h *Handler) registerNoV1Aliases() {
	h.mux.HandleFunc("POST /responses", h.withAuth(h.responses))
	h.mux.HandleFunc("POST /messages", h.withAuth(h.messages))
}

// authOK 报告请求是否携带合法凭据。两种头任一带对即通过：
//   - `Authorization: Bearer <key>`：OpenAI 系客户端（Codex / 各 SDK）；
//   - `x-api-key: <key>`：Anthropic 系客户端（Claude Code 只发这个头，不带 Authorization）。
//
// 支持**多个 key 并行有效**（见 keys.go）：config 的 api_key/api_keys 与看板生成的
// key 合并生效，任一命中即放行。校验用常量时间比较，避免时序侧信道逐字符猜 key。
//
// 鉴权开关的语义（重要，改动需谨慎）：
//   - **从未配置过任何 key** → 不鉴权（保持历史语义：api_key 为空 = 直接放行）。
//     若改成"无 key 即拒绝"，老配置升级后会全体 401。
//   - **配置过但全部被吊销** → 拒绝一切请求（fail-closed）。
//     不能因为"没有可用 key"就退回不鉴权——那会让"吊销泄露的 key"反而把网关敞开。
//
// 例外：来自内置看板的 /api/* 转发（dashboardAPI）已通过 Basic Auth，
// 其请求带 dashAuthed 标记，此处直接放行——否则看板页面就得内嵌 api_key。
func (h *Handler) authOK(r *http.Request) bool {
	if dashAuthed(r) {
		return true
	}
	if !h.keys.configured() {
		return true // 从未配置过 key：不鉴权（历史语义，见上方说明）
	}
	if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && h.keys.valid(got) {
		return true
	}
	return h.keys.valid(r.Header.Get("x-api-key"))
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.authOK(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total":   total,
		"service": ServiceName,
	})
}

// credits 查询所有账号实时积分余额（直调上游 billing 接口，不读缓存）。
func (h *Handler) credits(w http.ResponseWriter, r *http.Request) {
	sts := h.cfg.Pool.List()
	type acct struct {
		UID       string `json:"uid"`
		Nickname  string `json:"nickname,omitempty"`
		Remain    *int64 `json:"remain"`
		ExpiresAt int64  `json:"expires_at"`     // accessToken 到期（Unix 秒）
		ExpiresIn int64  `json:"expires_in_sec"` // 距到期剩余秒数，<0 表示已过期
		OK        bool   `json:"ok"`
		Error     string `json:"error,omitempty"`
	}
	out := make([]acct, len(sts))
	var wg sync.WaitGroup
	for i, st := range sts {
		wg.Add(1)
		go func(i int, uid, nick string) {
			defer wg.Done()
			res := acct{UID: uid, Nickname: nick}
			a := h.cfg.Pool.AuthByUID(uid)
			if a == nil {
				res.Error = "auth not found"
				out[i] = res
				return
			}
			res.ExpiresAt = a.ExpiresAtUnix()
			if res.ExpiresAt > 0 {
				res.ExpiresIn = res.ExpiresAt - time.Now().Unix()
			}
			remain, err := h.cfg.Upstream.UserResource(a)
			if err != nil {
				res.Error = err.Error()
			} else {
				res.Remain = &remain
				res.OK = true
			}
			out[i] = res
		}(i, st.UID, st.Nickname)
	}
	wg.Wait()
	var total int64
	okCount := 0
	for _, a := range out {
		if a.OK {
			okCount++
			if a.Remain != nil {
				total += *a.Remain
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "workbuddy",
		"ts":      time.Now().Unix(),
		"total": map[string]any{
			"remain":   total,
			"accounts": len(sts),
			"ok":       okCount,
		},
		"accounts": out,
	})
}

// stats 返回 token 用量统计（累计 + 分模型 + 分账号 + 按小时趋势）。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Stats == nil {
		writeJSON(w, http.StatusOK, StatsSnapshot{})
		return
	}
	writeJSON(w, http.StatusOK, h.cfg.Stats.Snapshot())
}

// SampleCredits 采集所有账号当前剩余积分明细，供 CreditTrack 周期采样。
// 返回空切片表示本次无有效采样（不能据此改写趋势）。
func (h *Handler) SampleCredits() []CreditAccountSnapshot {
	sts := h.cfg.Pool.List()
	out := make([]CreditAccountSnapshot, len(sts))
	ok := make([]bool, len(sts))
	var wg sync.WaitGroup
	for i, st := range sts {
		wg.Add(1)
		go func(i int, uid, nick string) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			remain, err := h.cfg.Upstream.UserResource(a)
			if err != nil {
				return
			}
			out[i] = CreditAccountSnapshot{UID: uid, Name: nick, Remain: remain}
			ok[i] = true
		}(i, st.UID, st.Nickname)
	}
	wg.Wait()
	detail := make([]CreditAccountSnapshot, 0, len(sts))
	for i := range out {
		if ok[i] {
			detail = append(detail, out[i])
		}
	}
	return detail
}

// calls 返回最近的调用流水（账号 + 时间），供 /calls 查询。
func (h *Handler) calls(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CallTrack == nil {
		writeJSON(w, http.StatusOK, map[string]any{"calls": []CallRecord{}})
		return
	}
	limit := parseCallLimit(r.URL.Query().Get("limit"), 200)
	calls := h.cfg.CallTrack.Recent(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"calls":        calls,
		"total":        len(h.cfg.CallTrack.Recent(0)),
		"limit":        limit,
		"generated_at": time.Now().Unix(),
	})
}

// creditsHistory 返回积分总量快照序列（供趋势图/消耗速率）。
func (h *Handler) creditsHistory(w http.ResponseWriter, r *http.Request) {
	if h.cfg.CreditTrack == nil {
		writeJSON(w, http.StatusOK, map[string]any{"snapshots": []CreditSnapshot{}})
		return
	}
	snaps := h.cfg.CreditTrack.History()
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshots":    snaps,
		"interval_sec": int(creditSnapInterval.Seconds()),
		"generated_at": time.Now().Unix(),
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
	})
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
	// modelsFetchAttempts 单次刷新最多尝试几个账号。
	// 一个账号拉取失败（token 过期/该号无权限/网络抖动）不代表整池不可用，
	// 换号重试能显著提高成功率；上限防止全池失败时把请求拖太久。
	modelsFetchAttempts = 3
)

// models 返回模型列表：**始终来自上游**（按账号拉取，缓存 1h）。
//
// 不再使用写死的静态模型表：
//   - 静态表会"撒谎"——空池时也返回一串模型，客户端以为可用，一发请求才 503；
//   - 上游模型目录会变（新增/下线/权限差异），写死的表迟早与实际不符。
//
// 因此拿不到时**如实返回空列表**（HTTP 200 + 空 data），让调用方明确知道
// "当前没有可用模型"，而不是拿到一份伪造的清单。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 从上游拉取模型列表并包装成 OpenAI 格式（含 context_length）。
// 无可用账号或拉取失败时返回空切片。
func (h *Handler) modelList() []map[string]any {
	infos := h.fetchDynamicModels()
	out := make([]map[string]any, 0, len(infos))
	for _, mi := range infos {
		entry := map[string]any{
			"id":                mi.ID,
			"object":            "model",
			"created":           1753600000,
			"owned_by":          "workbuddy",
			"context_length":    mi.ContextWindow,
			"max_output_tokens": mi.MaxTokens,
		}
		if mi.ContextWindow == 0 {
			entry["context_length"] = 131072 // 兜底
		}
		out = append(out, entry)
	}
	return out
}

// fetchDynamicModels 从池中挑账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
//
// 失败处理：
//   - 单账号失败 → 记一次错误（避免下次 Pick 又选中它）并**换号重试**，最多 modelsFetchAttempts 次；
//   - 全部失败 → 记 5min 全局负缓存，冷却期内直接返回空，避免反复打上游。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	tried := map[string]bool{}
	attempted := 0
	for i := 0; i < modelsFetchAttempts; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			// 没有更多可用账号了。两种情形要区分：
			//   - attempted == 0：池里本就没号（空池/全冷却）——不是失败，
			//     不写负缓存，账号一加进来就应立刻能拉到；
			//   - attempted > 0：**试过但全都失败**，必须写负缓存，
			//     否则小池子（如只有 1 个号）下每轮都会重新打上游。
			//     （早期漏了这一步，导致只有 1 个账号时负缓存永不生效。）
			break
		}
		tried[acct.UID] = true
		attempted++
		infos, err := h.cfg.Upstream.FetchModels(acct)
		if err != nil || len(infos) == 0 {
			// 惩罚该账号，避免下次 Pick 又选中同一个反复失败。
			h.cfg.Pool.NoteError(acct.UID)
			continue
		}
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = time.Now()
		dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
		dynamicModelsCache.Unlock()
		return infos
	}

	if attempted == 0 {
		// 池中无可用账号：不是"拉取失败"，只是现在没号。不写负缓存。
		return nil
	}

	// 试过的账号全部失败：进入负缓存，避免冷启动/上游故障时反复打上游。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now()
	dynamicModelsCache.Unlock()
	return nil
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 请求体上限：LimitReader 读 limit+1 以探测"超限"（读到 limit+1 字节即已超），
	// 超限直接 413，不把截断的半截 JSON 喂给上游（issue #41：截断 body 让上游
	// unmarshal 报 unexpected EOF，网关却罚号轮空）。
	// 413 是网关侧的客户端问题，不打上游、不罚账号、不轮转。
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if int64(len(body)) > limit {
		writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_body_too_large",
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（超过上限一半）：小探针（{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(body)*2 >= int(limit) {
		if err := os.WriteFile("/app/data/last_request.json", body, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 账号池挑号 + 轮转 + 上游 SSE 管线全部下沉到 relay（与 /v1/responses、/v1/messages 共用）。
	h.relay(w, r, chatRequest{Body: body, Model: peek.Model, Stream: peek.Stream},
		func(rc io.Reader, acct *auth.Auth) (time.Duration, int) {
			if peek.Stream {
				// 流式：透传上游 SSE 到客户端（逐帧规范化），结束后立即关闭上游 body。
				stats := newChatStatsReaderSince(rc, time.Now())
				_ = upstream.Stream(w, stats)
				if h.cfg.Stats != nil {
					h.cfg.Stats.Record(peek.Model, acct.UID,
						stats.PromptTokens(), statsCompletionTokens(stats), stats.TotalTokens(), http.StatusOK)
				}
				return stats.TTFB(), statsCompletionTokens(stats)
			}
			// 非流式：聚合上游 SSE 为单个 chat.completion。
			resp, err := upstream.Aggregate(rc)
			if err != nil {
				// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
				writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
				return 0, -1
			}
			writeJSON(w, http.StatusOK, resp)
			if h.cfg.Stats != nil {
				if p, c, t, ok := usageTriple(resp); ok {
					h.cfg.Stats.Record(peek.Model, acct.UID, p, c, t, http.StatusOK)
				}
			}
			return 0, completionTokens(resp)
		},
		func(status int, msg string) {
			writeOpenAIError(w, status, "no_healthy_account", msg)
		})
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify），此处不再按原始 status 二次判断。
// 仅在 chatCompletions 轮转循环内调用：调用方已准备好 lastErr 并打算 continue 换号。
//
// 七条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 默认 Cooldown(CoolSoft, soft_rate) 连续触发指数退避（封顶 soft_rate_max）；
//     若上游 body 为模型级 6004 且带重置时间 → CooldownSoftForModel（until=重置墙钟，
//     封顶 soft_rate_max，记录触发模型供切模型豁免）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → NoteSessionDead：累加连续 12153 计数，**达阈值（默认 3）才禁用**。
//     一次 12153 多为临时抖动（网络 / 上游闪断 / refresh 竞态），一次即杀号会误杀健康账号
//     （历史 P0-1：13 个 disabled 号全是误判）。刷新成功 / 任意成功 / 手工复活清计数。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError），passthrough 模式走降级重试。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 模型级 6004 且带「将在 … 重置」时间（issue #31）：冷却到上游明说的重置墙钟
		// （封顶 soft_rate_max），记录触发模型 → 该账号对**其他模型**请求可豁免冷却。
		// 解析失败（无时间文案 / 非 6004）→ 退回既有 600s 基数 + 指数退避现况。
		if upstream.IsModelRateLimit(body) {
			if resetAt, ok := upstream.ParseSoftRateReset(body); ok {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
		}
		// 其余 soft_rate：软冷却基数来自 soft_rate（默认 600s）；同一账号连续触发时
		// pool 内部按 softStreak 指数退避并封顶 soft_rate_max。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
	case upstream.ErrSessionDead:
		// 连续 N 次才禁用（pool.NoteSessionDead 内部计数 + 阈值判定）。
		// 曾直接调 Disable() 而绕过阈值，与 README「连续 3 次才永久禁用」及 pool 层实现不符，
		// 导致单次抖动即杀号；此处必须走 NoteSessionDead。
		h.cfg.Pool.NoteSessionDead(uid)
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截（误报）：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 模式由 chatCompletions 内降级重试处理；custom 模式本不会到此分支。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
