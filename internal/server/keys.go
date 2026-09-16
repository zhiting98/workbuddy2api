// keys.go 网关 API Key 管理：支持多个 key 并行有效，并允许在看板上生成 / 复制 / 吊销。
//
// 背景：原实现只有单个 cfg.APIKey，校验是 `got == h.cfg.APIKey` 的等值比较。
// 单一 key 的实际痛点不是"数量不够"，而是**无法按客户端区分与吊销**——
// 任一客户端泄露 key，就只能给所有人换 key（还有服务中断）。
//
// 两个来源**合并生效**（union，而非谁覆盖谁）：
//   - config.json 的 api_key / api_keys：声明式配置，进程启动时读入，UI 只读。
//     保留单值 api_key 是为了对老配置零影响（老 config 无需改动即可继续用）。
//   - data/api_keys.json：看板生成的 key，UI 可增删。**变更即时落盘**——key 生成后
//     用户会立刻复制并可能马上重启容器，延迟落盘（如 stats 的 15s）会让新 key 失效。
//
// 吊销（revoke）用**墓碑**记录而非直接删除：
//   - 对看板生成的 key：墓碑 + 从生成列表移除（语义等于删除）；
//   - 对 config 里的 key：配置是只读源，删不掉，墓碑让它立即失效
//     （泄露时不必改配置 + 重启，这是吊销能力的关键价值）。
//
// 沿用既有语义：**一个 key 都没有 = 不鉴权**（见 handler.authOK）。
// 若改成"无 key 即拒绝"，老配置（api_key 为空）升级后会全体 401。
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// apiKeySource 标记 key 的来源，决定前端能否删除它。
type apiKeySource string

const (
	// keySourceConfig 来自 config.json 的 api_key / api_keys：声明式，UI 只读
	// （可吊销，但不能删除——删了下次启动又会被配置重新带入）。
	keySourceConfig apiKeySource = "config"
	// keySourceGenerated 由看板生成，落盘在 data/api_keys.json，UI 可增删。
	keySourceGenerated apiKeySource = "generated"
)

// apiKeyRecord 一条看板生成的 key（持久化结构）。
type apiKeyRecord struct {
	Key       string `json:"key"`
	Name      string `json:"name,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// APIKeyView 对外（HTTP）暴露的 key 视图。
//
// 注意 Key 是**明文**：看板需要「复制」功能就必须拿到原值。
// 这与既有安全模型一致——/api/* 走 Basic Auth，且看板本就有「导出凭证」按钮
// （能导出全部账号的明文 token）。key 的敏感度不高于它。
type APIKeyView struct {
	Key       string       `json:"key"`
	Preview   string       `json:"preview"` // 展示用掩码（sk-abcd…wxyz）
	Name      string       `json:"name,omitempty"`
	Source    apiKeySource `json:"source"`
	CreatedAt int64        `json:"created_at,omitempty"`
	Enabled   bool         `json:"enabled"`
}

// keyStore 多 key 存储：config 源 + 生成源 + 吊销墓碑。
type keyStore struct {
	mu   sync.RWMutex
	path string // data/api_keys.json；空 = 不持久化（仅内存，测试用）

	cfgKeys []string        // 来自 config（启动时定，进程内不变）
	gen     []apiKeyRecord  // 看板生成
	revoked map[string]bool // 墓碑：已吊销的 key（两种来源共用）

	// initialized 记录"用户**曾经**启用过 key 体系"，一旦为真就持久为真。
	//
	// 为什么必须有这个标志（否则是 fail-open 漏洞）：
	// 鉴权开关的判定是"是否配置过 key"。若只用「当前有没有 key」来判，那么
	// 用户删掉最后一个（唯一的）生成 key 后，网关会**静默退回不鉴权**——
	// 用户以为"清理干净了"，实际把网关敞开了。
	// 有了 initialized：删空 key 之后仍判为"配置过"，于是继续 401（fail-closed），
	// 而真正从未配置过的老部署（无 config key、无该文件）依旧不鉴权（向后兼容）。
	initialized bool
}

// newKeyStore 构造并加载。cfgKeys 为 config 提供的所有 key（含单值 api_key）。
// path 为空时仅内存运行（不落盘），便于测试与"未配置 state 目录"的场景。
func newKeyStore(path string, cfgKeys []string) *keyStore {
	ks := &keyStore{
		path:    path,
		cfgKeys: normalizeKeys(cfgKeys),
		revoked: map[string]bool{},
	}
	ks.load()
	return ks
}

// normalizeKeys 去空白、去重、丢弃空串（配置里常见的 `["k1","","k1"]`）。
func normalizeKeys(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// keyPreview 生成展示用掩码：保留头尾便于人工核对，中间省略。
// 短 key 全掩码，避免"掩码本身泄露了大半个 key"。
func keyPreview(k string) string {
	const head, tail = 8, 4
	if len(k) <= head+tail+1 {
		return strings.Repeat("•", len(k))
	}
	return k[:head] + "…" + k[len(k)-tail:]
}

// valid 报告该 key 是否可用。**常量时间比较**，避免时序侧信道逐字符猜 key
// （与 checkDashboardCreds 同口径）。
//
// 不提前 return 命中项之外的分支：遍历完所有候选再判定，使耗时与"命中位置"无关。
func (ks *keyStore) valid(key string) bool {
	if key == "" {
		return false
	}
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	if ks.revoked[key] {
		return false
	}
	hit := false
	for _, c := range ks.cfgKeys {
		if subtle.ConstantTimeCompare([]byte(c), []byte(key)) == 1 {
			hit = true
		}
	}
	for _, g := range ks.gen {
		if subtle.ConstantTimeCompare([]byte(g.Key), []byte(key)) == 1 {
			hit = true
		}
	}
	return hit
}

// addConfigKey 把一个 key 加入 config 源（运行期）。
//
// 唯一调用方是安装向导：它生成的 key 会写回 config.json，语义上属于声明式配置，
// 因此登记为 config 源（UI 只读、可吊销不可删）。若不登记，authOK 就查不到它
// ——因为 authOK 已不再直接读 cfg.APIKey，而只问 keys。
//
// 幂等：重复添加同一 key 无副作用。吊销态一并清除（向导刚生成的 key 理应可用）。
func (ks *keyStore) addConfigKey(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for _, c := range ks.cfgKeys {
		if c == key {
			delete(ks.revoked, key)
			return
		}
	}
	ks.cfgKeys = append(ks.cfgKeys, key)
	sort.Strings(ks.cfgKeys)
	delete(ks.revoked, key)
}

// configured 报告鉴权是否**应当启用**（true = 校验 key；false = 完全不鉴权）。
//
// 规则（两条都不能省，否则会出现 fail-open）：
//  1. config 里配了 key → 启用。**即使全部被吊销也算启用**——
//     吊销泄露的 key 是收紧动作，绝不能反过来把网关敞开。
//  2. 当前或曾经有过生成 key → 启用。`initialized` 保证"删掉最后一个 key"
//     不会让网关退回不鉴权（那正是用户以为清理干净、实际裸奔的场景）。
//
// 只有「从未配置过任何 key」才返回 false（不鉴权），以兼容 api_key 为空的历史部署。
func (ks *keyStore) configured() bool {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	return len(ks.cfgKeys) > 0 || len(ks.gen) > 0 || ks.initialized
}

// list 返回全部 key（含已吊销的 config key，便于前端展示与恢复）。
// config 源在前、生成源在后；各自内部按创建时间/名称稳定排序。
func (ks *keyStore) list() []APIKeyView {
	ks.mu.RLock()
	defer ks.mu.RUnlock()

	out := make([]APIKeyView, 0, len(ks.cfgKeys)+len(ks.gen))
	for _, c := range ks.cfgKeys {
		out = append(out, APIKeyView{
			Key:     c,
			Preview: keyPreview(c),
			Source:  keySourceConfig,
			Enabled: !ks.revoked[c],
		})
	}
	for _, g := range ks.gen {
		out = append(out, APIKeyView{
			Key:       g.Key,
			Preview:   keyPreview(g.Key),
			Name:      g.Name,
			Source:    keySourceGenerated,
			CreatedAt: g.CreatedAt,
			Enabled:   true,
		})
	}
	return out
}

// generate 生成一个新 key 并立即落盘。name 可选（便于区分用途）。
// 返回的 view 含**明文 key**——这是唯一一次"必然能拿到明文"的时机，
// 之后列表里也仍会返回明文（为了复制功能），但用户界面默认只显示掩码。
func (ks *keyStore) generate(name string) (APIKeyView, error) {
	h, err := randHex(24) // 48 字符，与安装向导生成口径一致
	if err != nil {
		return APIKeyView{}, fmt.Errorf("生成随机 key 失败: %w", err)
	}
	key := "sk-" + h
	rec := apiKeyRecord{
		Key:       key,
		Name:      strings.TrimSpace(name),
		CreatedAt: time.Now().Unix(),
	}

	ks.mu.Lock()
	ks.gen = append(ks.gen, rec)
	// 一旦生成过 key，鉴权体系即视为"已启用"且**永不回退**（见 initialized 注释）。
	ks.initialized = true
	// 新 key 不可能处于吊销态（key 是随机新值），但同值极端巧合下清一下更稳。
	delete(ks.revoked, key)
	ks.mu.Unlock()

	if err := ks.persist(); err != nil {
		// 落盘失败要回滚内存，否则"内存有效、重启即失效"——用户复制去用，
		// 重启后突然 401，且无从察觉原因。
		ks.mu.Lock()
		for i, g := range ks.gen {
			if g.Key == key {
				ks.gen = append(ks.gen[:i], ks.gen[i+1:]...)
				break
			}
		}
		ks.mu.Unlock()
		return APIKeyView{}, err
	}

	return APIKeyView{
		Key:       key,
		Preview:   keyPreview(key),
		Name:      rec.Name,
		Source:    keySourceGenerated,
		CreatedAt: rec.CreatedAt,
		Enabled:   true,
	}, nil
}

// removeGenerated 删除一个**看板生成**的 key。config 源的 key 不走这里
// （它们由配置文件定义，删了下次启动又回来），应改用 revoke。
func (ks *keyStore) removeGenerated(key string) error {
	ks.mu.Lock()
	idx := -1
	for i, g := range ks.gen {
		if g.Key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		ks.mu.Unlock()
		return fmt.Errorf("该 key 不是看板生成的，无法删除（config 里的 key 请用吊销）")
	}
	removed := ks.gen[idx]
	ks.gen = append(ks.gen[:idx], ks.gen[idx+1:]...)
	ks.mu.Unlock()

	if err := ks.persist(); err != nil {
		// 回滚：删除未落盘会导致重启后 key 复活（用户以为已删除，实际仍可用）。
		ks.mu.Lock()
		ks.gen = append(ks.gen, removed)
		sort.Slice(ks.gen, func(i, j int) bool { return ks.gen[i].CreatedAt < ks.gen[j].CreatedAt })
		ks.mu.Unlock()
		return err
	}
	return nil
}

// setEnabled 启用/吊销一个 key。吊销即写墓碑（两种来源通用）。
//
// 为什么吊销要覆盖 config 源：config 是只读源，删不掉；而"泄露的 key 能否立即失效"
// 是 key 管理的核心能力。墓碑让它不必改配置 + 重启也能立刻失效。
func (ks *keyStore) setEnabled(key string, enabled bool) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("key 不能为空")
	}

	ks.mu.Lock()
	known := false
	for _, c := range ks.cfgKeys {
		if c == key {
			known = true
			break
		}
	}
	for _, g := range ks.gen {
		if g.Key == key {
			known = true
			break
		}
	}
	if !known {
		ks.mu.Unlock()
		return fmt.Errorf("未知的 key")
	}
	prev := ks.revoked[key]
	if enabled {
		delete(ks.revoked, key)
	} else {
		ks.revoked[key] = true
	}
	ks.mu.Unlock()

	if err := ks.persist(); err != nil {
		ks.mu.Lock()
		if prev {
			ks.revoked[key] = true
		} else {
			delete(ks.revoked, key)
		}
		ks.mu.Unlock()
		return err
	}
	return nil
}

// keyStoreDisk 落盘结构。用独立结构而非直接序列化 keyStore：
// 后者含 mutex 与派生字段，且未来加字段时不该污染磁盘格式。
type keyStoreDisk struct {
	Keys    []apiKeyRecord `json:"keys"`
	Revoked []string       `json:"revoked,omitempty"`
	// Initialized 持久化"曾启用过 key 体系"，使"删光所有 key"后重启仍保持鉴权
	// （否则重启会退回不鉴权 = fail-open）。
	Initialized bool `json:"initialized,omitempty"`
}

func (ks *keyStore) persist() error {
	if ks.path == "" {
		return nil // 仅内存模式：不落盘但视为成功
	}
	ks.mu.RLock()
	doc := keyStoreDisk{
		Keys:        append([]apiKeyRecord(nil), ks.gen...),
		Revoked:     make([]string, 0, len(ks.revoked)),
		Initialized: ks.initialized,
	}
	for k := range ks.revoked {
		doc.Revoked = append(doc.Revoked, k)
	}
	ks.mu.RUnlock()
	sort.Strings(doc.Revoked)

	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}
	if dir := filepath.Dir(ks.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	// tmp + rename 原子写（与 state.json / stats.json 同口径），避免半截文件。
	tmp := ks.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, ks.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换 %s 失败: %w", ks.path, err)
	}
	return nil
}

// load 读取既有生成 key 与吊销墓碑。文件不存在属正常（首次启动）。
func (ks *keyStore) load() {
	if ks.path == "" {
		return
	}
	raw, err := os.ReadFile(ks.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("ERR: [keys] 读取 %s 失败: %v", ks.path, err)
		}
		return
	}
	var doc keyStoreDisk
	if err := json.Unmarshal(raw, &doc); err != nil {
		// 解析失败不覆盖文件（保留原文件供人工排查），以空状态继续。
		log.Printf("ERR: [keys] %s 解析失败（已忽略，未覆盖原文件）: %v", ks.path, err)
		return
	}
	ks.gen = doc.Keys
	ks.initialized = doc.Initialized || len(doc.Keys) > 0
	if len(doc.Revoked) > 0 {
		ks.revoked = make(map[string]bool, len(doc.Revoked))
		for _, k := range doc.Revoked {
			ks.revoked[k] = true
		}
	}
	log.Printf("[keys] 已加载 %d 个生成 key（config 源 %d 个，吊销 %d 个）",
		len(ks.gen), len(ks.cfgKeys), len(ks.revoked))
}
