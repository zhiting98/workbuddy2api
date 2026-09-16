package scheduler

import (
	"testing"
	"time"
)

// TestEnableDoesNotFireImmediately 验证：运行中启用一类任务**不会立即触发**，
// 而是排到下一个配置时点。
//
// 这决定了开关的实用性边界——用户若在 14:00 打开签到（时点 9/21），
// 真正执行要等到 21:00。想"立刻签到"必须另有手动入口。
func TestEnableDoesNotFireImmediately(t *testing.T) {
	now := time.Date(2026, 9, 11, 14, 0, 0, 0, time.Local) // 14:00
	s := New(Config{
		TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true,
		CheckinDisabled: true, // 初始禁用
	})
	// 启用签到
	s.SetTaskEnabled("checkin", true)

	at, kinds := s.nextWake(now)
	want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local) // 下一个时点 21:00
	if !at.Equal(want) {
		t.Errorf("启用后下一次唤醒=%v want %v（不应立即触发）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
	if !at.After(now) {
		t.Errorf("唤醒时刻 %v 必须晚于 now %v", at, now)
	}
}

// TestNextFireNeverReturnsPast 边界：nextFire 恒返回**严格未来**时刻。
// 恰好落在整点时也应排到次日，而不是"当前时刻"（否则会在启用瞬间触发）。
func TestNextFireNeverReturnsPast(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"恰在时点整点",
			time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local),
			// 9:00 整不算"之后"，故排到 21:00（另一时点），而非立即。
			time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local),
		},
		{
			"时点前一刻",
			time.Date(2026, 9, 11, 8, 59, 59, 0, time.Local),
			time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local),
		},
		{
			"当日时点已过",
			time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local),
			time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local), // 次日 9:00
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextFire(tc.now, []int{9, 21})
			if !got.Equal(tc.want) {
				t.Errorf("nextFire(%v)=%v want %v", tc.now, got, tc.want)
			}
			if !got.After(tc.now) {
				t.Errorf("必须严格晚于 now：got=%v now=%v", got, tc.now)
			}
		})
	}
}
