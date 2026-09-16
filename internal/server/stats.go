// stats.go token 用量统计：累计总量 + 分模型 + 分账号 + 按小时趋势，持久化供 /stats 展示。
package server

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// UsageTotals 某一维度的 token 累计。
type UsageTotals struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
	Requests   int64 `json:"requests"`
}

func (u *UsageTotals) add(prompt, completion, total int) {
	u.Prompt += int64(prompt)
	u.Completion += int64(completion)
	u.Total += int64(total)
	u.Requests++
}

// Stats 记录 token 用量并周期性持久化，供 /stats 返回数量与趋势。
type Stats struct {
	mu    sync.Mutex
	path  string
	dirty bool

	Total   UsageTotals            `json:"total"`
	ByModel map[string]UsageTotals `json:"by_model"`
	ByUID   map[string]UsageTotals `json:"by_uid"`
	Hourly  map[string]UsageTotals `json:"hourly"` // key: 2006-01-02T15（本地时区）

	stopCh chan struct{}
}

// statsHourlyRetention 按小时桶保留时长（超出即裁剪，控制文件体积）。
const statsHourlyRetention = 14 * 24 * time.Hour

// NewStats 构造并加载既有统计（无文件则从零开始），后台每 15s 落盘一次。
func NewStats(path string) *Stats {
	s := &Stats{
		path:    path,
		ByModel: map[string]UsageTotals{},
		ByUID:   map[string]UsageTotals{},
		Hourly:  map[string]UsageTotals{},
		stopCh:  make(chan struct{}),
	}
	s.load()
	go s.flusher()
	return s
}

// Record 记录一次请求的 token 用量（无 token 数据的请求忽略）。
func (s *Stats) Record(model, uid string, prompt, completion, total, status int) {
	if status != 200 || (prompt == 0 && completion == 0 && total == 0) {
		return
	}
	if total == 0 {
		total = prompt + completion
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	key := now.Format("2006-01-02T15")

	s.Total.add(prompt, completion, total)

	if model == "" {
		model = "-"
	}
	m := s.ByModel[model]
	m.add(prompt, completion, total)
	s.ByModel[model] = m

	if uid != "" {
		u := s.ByUID[uid]
		u.add(prompt, completion, total)
		s.ByUID[uid] = u
	}

	h := s.Hourly[key]
	h.add(prompt, completion, total)
	s.Hourly[key] = h

	s.pruneLocked(now)
	s.dirty = true
}

// pruneLocked 裁剪超出保留期的按小时桶。
func (s *Stats) pruneLocked(now time.Time) {
	cutoff := now.Add(-statsHourlyRetention).Format("2006-01-02T15")
	for k := range s.Hourly {
		if k < cutoff {
			delete(s.Hourly, k)
		}
	}
}

func (s *Stats) flusher() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.Flush()
		}
	}
}

// Flush 脏时原子落盘（tmp + rename）。
func (s *Stats) Flush() {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return
	}
	dirty := true
	raw, err := json.Marshal(s)
	s.dirty = false
	s.mu.Unlock()
	if err != nil || len(raw) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		log.Printf("stats: mkdir failed: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("stats: write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("stats: rename failed: %v", err)
		return
	}
	_ = dirty
}

func (s *Stats) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var disk Stats
	if err := json.Unmarshal(raw, &disk); err != nil {
		log.Printf("stats: load parse failed: %v", err)
		return
	}
	s.Total = disk.Total
	if disk.ByModel != nil {
		s.ByModel = disk.ByModel
	}
	if disk.ByUID != nil {
		s.ByUID = disk.ByUID
	}
	if disk.Hourly != nil {
		s.Hourly = disk.Hourly
	}
}

// DimEntry 维度条目（分模型/分账号）。
type DimEntry struct {
	Name string `json:"name"`
	UsageTotals
}

// HourEntry 按小时趋势条目。
type HourEntry struct {
	Hour string `json:"hour"`
	UsageTotals
}

// StatsSnapshot /stats 返回结构（各维度按 total 降序，趋势按时间升序）。
type StatsSnapshot struct {
	Total       UsageTotals `json:"total"`
	ByModel     []DimEntry  `json:"by_model"`
	ByUID       []DimEntry  `json:"by_uid"`
	Hourly      []HourEntry `json:"hourly"`
	GeneratedAt int64       `json:"generated_at"`
}

// Snapshot 返回当前统计快照。
func (s *Stats) Snapshot() StatsSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := StatsSnapshot{Total: s.Total, GeneratedAt: time.Now().Unix()}
	for name, u := range s.ByModel {
		out.ByModel = append(out.ByModel, DimEntry{Name: name, UsageTotals: u})
	}
	for name, u := range s.ByUID {
		out.ByUID = append(out.ByUID, DimEntry{Name: name, UsageTotals: u})
	}
	for h, u := range s.Hourly {
		out.Hourly = append(out.Hourly, HourEntry{Hour: h, UsageTotals: u})
	}
	sort.Slice(out.ByModel, func(i, j int) bool { return out.ByModel[i].Total > out.ByModel[j].Total })
	sort.Slice(out.ByUID, func(i, j int) bool { return out.ByUID[i].Total > out.ByUID[j].Total })
	sort.Slice(out.Hourly, func(i, j int) bool { return out.Hourly[i].Hour < out.Hourly[j].Hour })
	return out
}
