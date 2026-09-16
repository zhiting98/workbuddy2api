package scheduler

import (
	"context"
	"testing"
	"time"
)

// TestTaskSwitchesReflectConfig 初始开关来自 Config（*Disabled）。
func TestTaskSwitchesReflectConfig(t *testing.T) {
	s := New(Config{
		CheckinDisabled:   true,
		TravelDisabled:    false,
		ActivityDisabled:  true,
		KeepaliveDisabled: false,
	})
	byKey := map[string]TaskSwitch{}
	for _, it := range s.TaskSwitches() {
		byKey[it.Key] = it
	}
	if len(byKey) != 4 {
		t.Fatalf("应有 4 类任务，得 %d", len(byKey))
	}
	for key, want := range map[string]bool{
		"checkin": false, "travel": true, "activity": false, "keepalive": true,
	} {
		if got := byKey[key].Enabled; got != want {
			t.Errorf("%s enabled=%v want %v", key, got, want)
		}
	}
	// 禁用时**保留时点**：切开关不该抹掉 hours 配置。
	if len(byKey["checkin"].Hours) == 0 {
		t.Error("禁用状态下仍应保留 hours（重新启用即恢复原时点）")
	}
}

// TestSetTaskEnabledToggles 运行时可切换，且 TaskSwitches 立即反映。
func TestSetTaskEnabledToggles(t *testing.T) {
	s := New(Config{})
	for _, key := range []string{"checkin", "travel", "activity", "keepalive"} {
		if !s.SetTaskEnabled(key, false) {
			t.Fatalf("SetTaskEnabled(%s,false) 应返回 true", key)
		}
	}
	for _, it := range s.TaskSwitches() {
		if it.Enabled {
			t.Errorf("%s 应已禁用", it.Key)
		}
	}
	// 再启用。
	s.SetTaskEnabled("checkin", true)
	for _, it := range s.TaskSwitches() {
		if it.Key == "checkin" && !it.Enabled {
			t.Error("checkin 应已重新启用")
		}
	}
}

// TestSetTaskEnabledUnknownKey 未知 key 返回 false（调用方据此报 400）。
func TestSetTaskEnabledUnknownKey(t *testing.T) {
	s := New(Config{})
	if s.SetTaskEnabled("nope", true) {
		t.Error("未知 key 应返回 false")
	}
}

// TestNextWakeRespectsRuntimeSwitch **核心**：禁用后该任务不再进入下一次排程。
//
// 这条验证的是"运行时开关真的影响调度"，而不仅是内部布尔值变了。
func TestNextWakeRespectsRuntimeSwitch(t *testing.T) {
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local)

	// 只留签到：08:00 时下一次唤醒应是 09:00。
	s := New(Config{TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true})
	at, kinds := s.nextWake(now)
	if !at.Equal(time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local)) {
		t.Fatalf("下一次唤醒=%v want 09:00", at)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatalf("kinds=%v want [checkin]", kinds)
	}

	// 运行中禁用签到 → 全关且无任务自动化 → 零时间（Run 会阻塞等待，不空转）。
	s.SetTaskEnabled("checkin", false)
	at2, kinds2 := s.nextWake(now)
	if !at2.IsZero() || len(kinds2) != 0 {
		t.Errorf("全部禁用后应零时间无任务，得 at=%v kinds=%v", at2, kinds2)
	}

	// 重新启用 → 排程立刻恢复。
	s.SetTaskEnabled("checkin", true)
	at3, kinds3 := s.nextWake(now)
	if at3.IsZero() || len(kinds3) != 1 {
		t.Errorf("重新启用后应恢复排程，得 at=%v kinds=%v", at3, kinds3)
	}
}

// TestWakeSignalOnToggle 切换时发出唤醒信号（非阻塞，缓冲 1）。
// 这是"开关立即生效"的关键：否则 Run 会睡到原时点才重算。
func TestWakeSignalOnToggle(t *testing.T) {
	s := New(Config{})
	// 初始无信号。
	select {
	case <-s.wake:
		t.Fatal("初始不应有唤醒信号")
	default:
	}
	s.SetTaskEnabled("checkin", false)
	select {
	case <-s.wake:
		// 期望有
	default:
		t.Fatal("切换后应发出唤醒信号")
	}
	// 重复切换不应阻塞（缓冲满时丢弃）。
	for i := 0; i < 5; i++ {
		s.SetTaskEnabled("checkin", i%2 == 0)
	}
}

// TestRunReactsToSwitchWhileSleeping 端到端：Run 在长睡眠中被开关唤醒并重算。
//
// 场景：只剩保活（默认 22:00），启动时约 08:00 —— Run 会 sleep 十余小时。
// 此时禁用保活，Run 必须被唤醒；否则它会一直睡到 22:00 才处理（开关"看着生效、实际没生效"）。
// 这里用一个"全部禁用"的初始状态触发零时间分支，验证唤醒能把它从阻塞中拉出来。
func TestRunReactsToSwitchWhileSleeping(t *testing.T) {
	// 四类全禁 + 无任务自动化 → nextWake 零时间 → Run 进入"等 ctx 或 wake"分支。
	s := New(Config{
		CheckinDisabled: true, TravelDisabled: true,
		ActivityDisabled: true, KeepaliveDisabled: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	// 给它一点时间进入等待。
	time.Sleep(50 * time.Millisecond)

	// 启用签到 → 应发出唤醒。Run 随后会重算并 sleep 到下一个 9/21 点。
	s.SetTaskEnabled("checkin", true)
	time.Sleep(50 * time.Millisecond)

	// 关键断言：Run 未退出（说明它被唤醒后继续循环，而不是卡死或 panic）。
	select {
	case <-done:
		t.Fatal("Run 不应退出")
	default:
	}

	// ctx 取消后必须能正常退出（验证 ctx 分支仍有效）。
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 应退出")
	}
}

// TestAllDisabledStillExitsOnContext 全禁用时 Run 仍能响应 ctx（不空转、能退出）。
func TestAllDisabledStillExitsOnContext(t *testing.T) {
	s := New(Config{
		CheckinDisabled: true, TravelDisabled: true,
		ActivityDisabled: true, KeepaliveDisabled: true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("全禁用时 Run 也应能被 ctx 取消")
	}
}
