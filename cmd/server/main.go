// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/config"
	"workbuddy2api/internal/panel"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/redisstore"
	"workbuddy2api/internal/scheduler"
	"workbuddy2api/internal/server"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Flush() // 进程退出前强制落盘（后台 flush 每 5s 一次，退出时补一次）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	// token 用量统计：持久化到 data/stats.json（与 state_file 同目录）。
	stats := server.NewStats(filepath.Join(filepath.Dir(cfg.StateFile), "stats.json"))
	defer stats.Flush()

	// 积分快照趋势：每 5 分钟采样一次所有账号剩余积分（合计 + 每账号明细），落盘 data/credits_snapshots.json。
	creditTrack := server.NewCreditTrack(filepath.Join(filepath.Dir(cfg.StateFile), "credits_snapshots.json"), nil)
	defer creditTrack.Close()

	// 调用流水：记录每次 /v1/chat/completions 实际使用的账号与时间，落盘 data/call_log.json。
	callTrack := server.NewCallTrack(filepath.Join(filepath.Dir(cfg.StateFile), "call_log.json"))
	defer callTrack.Close()

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA 覆盖（issue #42）：非空才改写，空 = 现状 clientUA（指纹净化考虑）。
	up.UserAgent = cfg.Upstream.UserAgent

	// 任务自动化 Runner（任务中心扫描 + 一键完成 + 执行队列）。
	tasks := panel.NewRunner(p, up)

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		CheckinDisabled:     !cfg.Schedule.CheckinEnabled,
		TravelDisabled:      !cfg.Schedule.TravelEnabled,
		ActivityDisabled:    !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:   !cfg.Schedule.KeepaliveEnabled,
		TaskHours:           cfg.Schedule.TaskHours,
		// 定时任务自动跑与看板「全部执行」共用同一并发度（config.schedule.task_concurrency，默认 10）。
		RunTasks: func() { tasks.RunQueue(cfg.Schedule.TaskConcurrency, config.DefaultTaskConcurrencyMax) },
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}

	h := server.NewHandler(server.Config{
		Pool:     p,
		Upstream: up,
		// 多 key：config 的单值 api_key + 数组 api_keys 合并生效。
		APIKey:  cfg.APIKey,
		APIKeys: cfg.EffectiveAPIKeys(),
		// 看板生成的 key 落盘到 data/api_keys.json（与 state_file 同目录，
		// 便于"备份 data/ 即备份全部状态"的一致性）。
		APIKeyPath: filepath.Join(filepath.Dir(cfg.StateFile), "api_keys.json"),
		AuthDir:    cfg.AuthDir, // 看板「添加账号」的落盘目录
		// 内置看板凭据（两者都非空才启用看板）。
		DashboardUser: cfg.Dashboard.User,
		DashboardPass: cfg.Dashboard.Pass,
		// 首次安装向导把凭据写回这个文件；未配置看板凭据时 `/` 变为安装向导。
		SetupConfigPath: *cfgPath,
		Session:         sessRouter,
		StickyCount:     sessCount,
		RedisMode:       redisMode,
		SoftCooldown:    cfg.SoftRateDur,
		PromptMode:      cfg.Prompt.Mode,
		PromptText:      cfg.PromptText,
		MaxBodyBytes:    int64(cfg.Server.MaxBodyMB) << 20, // MB → 字节
		Stats:           stats,
		CreditTrack:     creditTrack,
		CallTrack:       callTrack,
		Tasks:           tasks,
		// 任务队列并发度（看板可覆盖；缺省用配置值）。
		TaskConcurrency:    cfg.Schedule.TaskConcurrency,
		TaskConcurrencyMax: config.DefaultTaskConcurrencyMax,
		// 手动保活入口：把 scheduler 的结果类型转成 server 的最小视图。
		// 转一层是为了不让 server 包反向依赖 scheduler（依赖方向保持单向）。
		Keepalive: func() ([]server.KeepaliveOutcome, error) {
			items, err := sch.KeepaliveAll()
			if err != nil {
				return nil, err
			}
			out := make([]server.KeepaliveOutcome, 0, len(items))
			for _, it := range items {
				out = append(out, server.KeepaliveOutcome{
					UID:       it.UID,
					Nickname:  it.Nickname,
					Status:    string(it.Status),
					ExpiresAt: it.ExpiresAt,
					Extends:   it.Extends,
					Detail:    it.Detail,
				})
			}
			return out, nil
		},
		// 定时任务开关：读取与运行时切换（立即生效，并写回 config.json）。
		TaskSwitches: func() []server.TaskSwitch {
			items := sch.TaskSwitches()
			out := make([]server.TaskSwitch, 0, len(items))
			for _, it := range items {
				out = append(out, server.TaskSwitch{
					Key: it.Key, Name: it.Name, Enabled: it.Enabled,
					Hours: it.Hours, ConfigKey: it.ConfigKey,
				})
			}
			return out
		},
		SetTaskEnabled: sch.SetTaskEnabled,
		// 写回配置文件；*cfgPath 可能为空（纯默认配置启动）→ 由 server 侧
		// 判为"仅内存生效"并如实回报。这里只在有路径时才写。
		PersistScheduleSwitch: func(key string, enabled bool) error {
			if strings.TrimSpace(*cfgPath) == "" {
				return fmt.Errorf("未指定 -config 路径")
			}
			return server.WriteScheduleSwitch(*cfgPath, key, enabled)
		},
	})

	// 采样器需要 handler 的聚合逻辑（同一口径）。
	creditTrack.SetSampler(h.SampleCredits)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}
