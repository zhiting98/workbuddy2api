// creditsnap.go 积分快照：周期性采样所有账号剩余积分（总量 + 每账号明细），形成时间序列，
// 供 /credits/history 展示"积分使用趋势"（消耗速率、曲线、各账号余额）。
//
// 设计要点：
//   - 采样来源为 /credits 同一口径（Upstream.UserResource 聚合）——真实"可花费余额"。
//   - 每条快照同时记录合计与每账号明细，便于定位是哪个号在消耗。
//   - 内存保留最近 creditSnapRetention 条，落盘 data/credits_snapshots.json。
//   - 采样间隔默认 5 分钟；总量下降即为消耗，上升为补充/签到。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CreditAccountSnapshot 单个账号在同一时刻的剩余积分。
type CreditAccountSnapshot struct {
	UID    string `json:"uid"`
	Name   string `json:"name,omitempty"`
	Remain int64  `json:"remain"`
}

// CreditSnapshot 某一时刻的积分快照（合计 + 每账号明细）。
type CreditSnapshot struct {
	TS       int64                   `json:"ts"`       // Unix 秒
	Remain   int64                   `json:"remain"`   // 所有账号剩余积分合计
	Accounts int                     `json:"accounts"` // 成功采样的账号数
	OK       bool                    `json:"ok"`       // 本采样是否有效
	Detail   []CreditAccountSnapshot `json:"detail,omitempty"`
}

const (
	creditSnapRetention = 2016 // ≈ 7 天 @ 5min
	creditSnapInterval  = 5 * time.Minute
)

// CreditTrack 负责积分快照采样与持久化。
type CreditTrack struct {
	mu       sync.Mutex
	path     string
	dirty    bool
	Snaps    []CreditSnapshot `json:"snapshots"`
	stopCh   chan struct{}
	sampleFn func() []CreditAccountSnapshot
}

// NewCreditTrack 构造；sampleFn 由外部注入（复用 handler 的 credits 聚合逻辑）。
func NewCreditTrack(path string, sampleFn func() []CreditAccountSnapshot) *CreditTrack {
	c := &CreditTrack{
		path:     path,
		stopCh:   make(chan struct{}),
		sampleFn: sampleFn,
	}
	c.load()
	go c.loop()
	return c
}

// SetSampler 注入采样函数（可在构造后设置）。返回空切片视为无效采样，不记录。
func (c *CreditTrack) SetSampler(fn func() []CreditAccountSnapshot) {
	c.mu.Lock()
	c.sampleFn = fn
	c.mu.Unlock()
}

func (c *CreditTrack) loop() {
	// 启动后 20s 先采一次，避免重启即空白。
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-timer.C:
			c.Sample()
			timer.Reset(creditSnapInterval)
		}
	}
}

// Sample 立刻采样一次并写入序列。
func (c *CreditTrack) Sample() {
	c.mu.Lock()
	fn := c.sampleFn
	c.mu.Unlock()
	if fn == nil {
		return
	}
	detail := fn()
	if len(detail) == 0 {
		return
	}
	var remain int64
	for _, d := range detail {
		remain += d.Remain
	}
	c.Add(CreditSnapshot{
		TS:       time.Now().Unix(),
		Remain:   remain,
		Accounts: len(detail),
		OK:       true,
		Detail:   detail,
	})
}

// Add 追加一条快照（供采样与测试使用）。
func (c *CreditTrack) Add(s CreditSnapshot) {
	if !s.OK {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// 同一秒去重（避免手动刷新叠加）。
	if n := len(c.Snaps); n > 0 && c.Snaps[n-1].TS == s.TS {
		c.Snaps[n-1] = s
	} else {
		c.Snaps = append(c.Snaps, s)
	}
	if len(c.Snaps) > creditSnapRetention {
		c.Snaps = c.Snaps[len(c.Snaps)-creditSnapRetention:]
	}
	c.dirty = true
}

// History 返回快照序列（时间升序）。
func (c *CreditTrack) History() []CreditSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CreditSnapshot, len(c.Snaps))
	copy(out, c.Snaps)
	return out
}

func (c *CreditTrack) load() {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var disk struct {
		Snapshots []CreditSnapshot `json:"snapshots"`
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("creditsnap: load parse failed: %v", err)
		return
	}
	if disk.Snapshots != nil {
		c.Snaps = disk.Snapshots
	}
}

// Flush 脏时原子落盘。
func (c *CreditTrack) Flush() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	raw, err := json.Marshal(map[string]any{"snapshots": c.Snaps})
	c.dirty = false
	c.mu.Unlock()
	if err != nil || len(raw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		log.Printf("creditsnap: mkdir failed: %v", err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("creditsnap: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("creditsnap: rename failed: %v", err)
	}
}

// Close 停止后台采样并落盘。
func (c *CreditTrack) Close() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	c.Flush()
}
