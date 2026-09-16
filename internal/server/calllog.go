// calllog.go 调用流水：记录每次 /v1/chat/completions 实际使用的账号与时间，
// 形成"谁在什么时候被调用"的记录，落盘 data/call_log.json，供 /calls 查询。
//
// 设计要点：
//   - 请求出口统一记录一次（含全部账号不可用等失败路径，此时 uid 为空）。
//   - 内存保留最近 callLogRetention 条，后台每 15s 原子落盘（与 stats 同节奏）。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// CallRecord 一次调用的流水记录。
type CallRecord struct {
	TS       int64  `json:"ts"`                 // Unix 秒
	UID      string `json:"uid,omitempty"`      // 实际使用的账号；全部不可用时为空
	Nickname string `json:"nickname,omitempty"` // 账号昵称
	Model    string `json:"model"`              // 请求模型
	Mode     string `json:"mode"`               // "stream" | "sync"
	Status   int    `json:"status"`             // HTTP 状态码
}

const (
	callLogRetention = 5000 // 保留最近条数
	callLogInterval  = 15 * time.Second
)

// CallTrack 负责调用流水的记录与持久化。
type CallTrack struct {
	mu     sync.Mutex
	path   string
	dirty  bool
	Calls  []CallRecord `json:"calls"`
	stopCh chan struct{}
}

// NewCallTrack 构造并加载既有流水（无文件则从零开始），后台周期性落盘。
func NewCallTrack(path string) *CallTrack {
	c := &CallTrack{
		path:   path,
		Calls:  []CallRecord{},
		stopCh: make(chan struct{}),
	}
	c.load()
	go c.flusher()
	return c
}

// Record 记录一次调用（uid 为空表示本次请求没有可用账号）。
func (c *CallTrack) Record(uid, nickname, model, mode string, status int) {
	if model == "" {
		model = "-"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Calls = append(c.Calls, CallRecord{
		TS:       time.Now().Unix(),
		UID:      uid,
		Nickname: nickname,
		Model:    model,
		Mode:     mode,
		Status:   status,
	})
	if len(c.Calls) > callLogRetention {
		c.Calls = c.Calls[len(c.Calls)-callLogRetention:]
	}
	c.dirty = true
}

// Recent 返回最近 limit 条记录（时间升序）；limit<=0 时返回全部。
func (c *CallTrack) Recent(limit int) []CallRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.Calls)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]CallRecord, limit)
	copy(out, c.Calls[n-limit:])
	return out
}

func (c *CallTrack) flusher() {
	t := time.NewTicker(callLogInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-t.C:
			c.Flush()
		}
	}
}

func (c *CallTrack) load() {
	raw, err := os.ReadFile(c.path)
	if err != nil {
		return
	}
	var disk CallTrack
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("calllog: load parse failed: %v", err)
		return
	}
	if disk.Calls != nil {
		c.Calls = disk.Calls
	}
}

// Flush 脏时原子落盘（tmp + rename）。
func (c *CallTrack) Flush() {
	c.mu.Lock()
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	raw, err := json.Marshal(c)
	c.dirty = false
	c.mu.Unlock()
	if err != nil || len(raw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		log.Printf("calllog: mkdir failed: %v", err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("calllog: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("calllog: rename failed: %v", err)
	}
}

// Close 停止后台落盘并做最后一次 Flush。
func (c *CallTrack) Close() {
	select {
	case <-c.stopCh:
	default:
		close(c.stopCh)
	}
	c.Flush()
}

// parseCallLimit 解析 /calls?limit= 查询参数；非法/缺省回落 defaultLimit。
func parseCallLimit(raw string, defaultLimit int) int {
	if raw == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if n > callLogRetention {
		return callLogRetention
	}
	return n
}
