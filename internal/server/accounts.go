// accounts.go 看板的账号管理端点：OAuth 添加账号 + 导入/导出 + 复活被禁账号。
//
// 安全边界（重要）：
//   - **OAuth 登录流程**：凭证（accessToken/refreshToken）**全程不经过浏览器**——
//     浏览器只拿 authUrl 与「等待中/成功」状态，token 由网关直接落盘。
//     该流程的响应体中绝不含任何 token 字段。
//   - **导入 / 导出**：这两条通路**有意允许 token 经浏览器流转**（导入是粘贴/拖文件上行，
//     导出是下载落盘）。这不是疏漏，而是功能本质要求：备份无法绕开"文件要落在用户机器上"。
//     代价是导出文件等同于账号本身，故导出额外要求确认短语（见 exportConfirmPhrase）
//     且受 api_key 鉴权；两者都与 status/delete/revive 同一鉴权口径。
//   - 落盘复用 auth.Auth.SaveAtomic()（嵌套形 + tmp/rename + 0600），不另造写文件逻辑。
//   - **先落盘再入池**：写文件失败就不入池，避免内存里有号、重启后消失。
//
// 并发模型：登录会话表由本文件自持（map + mutex + TTL）。这一点是对 CLI 的关键修正——
// cmd/login 把 state 写死在 /tmp/wb2api-login-state.json，单文件、无并发保护，
// 两人同时登录会互相覆盖 state。服务端必须按会话隔离。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/oauth"
)

// loginSessionTTL 单个登录会话的有效期。上游 state 有时效，超时后轮询也无意义；
// 10 分钟足够用户完成一次浏览器授权。
const loginSessionTTL = 10 * time.Minute

// loginSessionGCInterval 过期会话清理周期。
const loginSessionGCInterval = 1 * time.Minute

// LoginStatus 会话状态机取值。
const (
	loginStatusPending = "pending" // 等待用户在浏览器完成授权
	loginStatusDone    = "done"    // 已完成并成功入池
	loginStatusFailed  = "failed"  // 真失败（上游错误/落盘失败）
)

// loginSession 一次登录会话。state 是上游签发的凭据，只存在服务端内存。
type loginSession struct {
	State     string
	AuthURL   string
	CreatedAt time.Time
	Status    string
	Err       string
	// Account 成功后回给前端的信息（**不含任何 token**）。
	Account *loginAccountInfo
}

// loginAccountInfo 添加成功后回给前端的账号信息（脱敏：只有展示字段）。
type loginAccountInfo struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
}

// loginSessions 登录会话表（内存，不落盘：state 是短时凭证，重启丢失可接受）。
type loginSessions struct {
	mu   sync.Mutex
	m    map[string]*loginSession
	stop chan struct{}
}

func newLoginSessions() *loginSessions {
	s := &loginSessions{m: map[string]*loginSession{}, stop: make(chan struct{})}
	go s.gcLoop()
	return s
}

// gcLoop 周期清理过期会话（避免长期运行后 map 无限增长）。
func (s *loginSessions) gcLoop() {
	t := time.NewTicker(loginSessionGCInterval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.gcOnce(time.Now())
		}
	}
}

// Close 停止后台 GC（进程退出时调用）。
func (s *loginSessions) Close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

// gcOnce 清理过期会话，返回清理数。
func (s *loginSessions) gcOnce(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, sess := range s.m {
		if now.Sub(sess.CreatedAt) > loginSessionTTL {
			delete(s.m, id)
			n++
		}
	}
	return n
}

// create 新建一个会话并返回其 id（与上游 state 分离：id 是本地句柄，
// 这样前端轮询时传的是本地 id，上游 state 不出网关）。
func (s *loginSessions) create(state, authURL string) string {
	id := newSessionID()
	s.mu.Lock()
	s.m[id] = &loginSession{
		State:     state,
		AuthURL:   authURL,
		CreatedAt: time.Now(),
		Status:    loginStatusPending,
	}
	s.mu.Unlock()
	return id
}

// get 取会话（不存在或已过期返回 false）。过期即删（惰性过期）。
func (s *loginSessions) get(id string) (*loginSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return nil, false
	}
	if time.Since(sess.CreatedAt) > loginSessionTTL {
		delete(s.m, id)
		return nil, false
	}
	return sess, true
}

// finish 标记会话完成/失败（幂等：已完成的不再改写，避免重复入池）。
// 返回 true 表示本次调用是状态迁移（调用方据此决定是否执行落盘等副作用）。
func (s *loginSessions) finish(id, status, errMsg string, info *loginAccountInfo) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok || sess.Status != loginStatusPending {
		return false
	}
	sess.Status = status
	sess.Err = errMsg
	sess.Account = info
	return true
}

// newSessionID 生成不可猜测的会话 id（16 字节随机）。
// 必须不可猜测：id 是轮询凭据，若可枚举，同网段他人可窃取他人的登录结果状态。
func newSessionID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败极罕见；退化为时间戳（仍唯一，只是不可猜测性下降）。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// HTTP 端点
// ---------------------------------------------------------------------------

// accountsLoginStart 发起一次 OAuth 登录，返回会话 id 与授权 URL。
//
// POST /accounts/login/start
// 响应: {"id":"...","auth_url":"https://..."}
//
// 注意：响应**不含 state**——前端不需要它，留在服务端可减少泄露面。
func (h *Handler) accountsLoginStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_dir_unset",
			"网关未配置 auth_dir，无法添加账号")
		return
	}
	state, authURL, err := oauth.StartLogin()
	if err != nil {
		log.Printf("ERR: [accounts] start login: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, "oauth_start_failed", err.Error())
		return
	}
	id := h.logins.create(state, authURL)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"auth_url": authURL,
	})
}

// accountsLoginPoll 轮询一次登录结果；完成时落盘并立即入池。
//
// GET /accounts/login/poll?id=<会话id>
// 响应: {"status":"pending"}
//
//	{"status":"done","account":{"uid":"...","nickname":"..."}}
//	{"status":"failed","error":"..."}
//
// **响应体绝不含 token**：凭证只在服务端流转并写入 auths/。
func (h *Handler) accountsLoginPoll(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_id", "缺少 id 参数")
		return
	}
	sess, ok := h.logins.get(id)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "session_expired",
			"登录会话不存在或已过期，请重新发起")
		return
	}
	// 已终结的会话直接回报，不重复调上游（幂等）。
	if sess.Status != loginStatusPending {
		writeLoginStatus(w, sess)
		return
	}

	bundle, err := oauth.PollLogin(sess.State)
	if errors.Is(err, oauth.ErrPending) {
		writeJSON(w, http.StatusOK, map[string]any{"status": loginStatusPending})
		return
	}
	if err != nil {
		log.Printf("WARN: [accounts] poll login: %v", err)
		h.logins.finish(id, loginStatusFailed, err.Error(), nil)
		writeLoginStatus(w, mustSession(h.logins, id))
		return
	}

	info, err := h.saveAndAdopt(bundle)
	if err != nil {
		log.Printf("ERR: [accounts] save account uid=%s: %v", logUID(bundle.UID), err)
		h.logins.finish(id, loginStatusFailed, err.Error(), nil)
		writeLoginStatus(w, mustSession(h.logins, id))
		return
	}
	log.Printf("[accounts] 新增账号 uid=%s nickname=%q（已入池）", logUID(info.UID), info.Nickname)
	h.logins.finish(id, loginStatusDone, "", info)
	writeLoginStatus(w, mustSession(h.logins, id))
}

// mustSession 取会话（调用方刚写过，必然存在）；仅用于构造响应。
func mustSession(s *loginSessions, id string) *loginSession {
	sess, _ := s.get(id)
	if sess == nil {
		return &loginSession{Status: loginStatusFailed, Err: "session vanished"}
	}
	return sess
}

// writeLoginStatus 输出会话终态（脱敏：只有 uid/nickname）。
func writeLoginStatus(w http.ResponseWriter, sess *loginSession) {
	out := map[string]any{"status": sess.Status}
	if sess.Err != "" {
		out["error"] = sess.Err
	}
	if sess.Account != nil {
		out["account"] = sess.Account
	}
	writeJSON(w, http.StatusOK, out)
}

// saveAndAdopt 把登录结果落盘为 auths/workbuddy-<uid>.json 并加入池。
//
// 顺序刻意固定为「先落盘、后入池」：写文件失败则不入池，避免内存中有账号
// 但重启后消失（那种不一致比直接失败更难排查）。
func (h *Handler) saveAndAdopt(b *oauth.Bundle) (*loginAccountInfo, error) {
	if strings.TrimSpace(b.UID) == "" {
		return nil, fmt.Errorf("上游未返回 uid，无法确定账号文件名")
	}
	if strings.TrimSpace(b.AccessToken) == "" {
		return nil, fmt.Errorf("上游未返回 accessToken")
	}

	expiresAt := int64(0)
	if b.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(b.ExpiresIn) * time.Second).Unix()
	}

	a := &auth.Auth{
		AccessToken:  b.AccessToken,
		RefreshToken: b.RefreshToken,
		ExpiresAt:    expiresAt,
		Domain:       b.Domain,
		UID:          b.UID,
		EnterpriseID: b.EnterpriseID,
		Nickname:     b.Nickname,
	}
	// 复用 auth.Save：文件名走 FileNameFor（保证匹配 LoadDir 的 workbuddy*.json 扫描），
	// 写入走 SaveAtomic（tmp + rename + 0600，含空 accessToken 保护）。
	if _, err := a.Save(h.cfg.AuthDir); err != nil {
		return nil, fmt.Errorf("写 auth 文件失败: %w", err)
	}
	// 入池（新号加入；已存在的同 uid 会被 upsert，保留其积分/冷却状态）。
	h.cfg.Pool.Add(a)
	return &loginAccountInfo{UID: b.UID, Nickname: b.Nickname}, nil
}

// accountsImport 批量导入账号：接受多种 JSON 形态，逐条落盘 + 入池。
//
// POST /accounts/import
// 请求体：单个 JSON 对象、或 JSON 数组、或 {"accounts":[...]} 包装。
// 响应：{"imported":N,"failed":M,"total":T,"results":[{uid,nickname,ok,error,file}...]}
//
// 设计要点：
//   - **部分成功**：单条失败不影响其他条目，逐条回报结果（批量导入时这一点很重要，
//     否则一条脏数据会让整批白干）。
//   - **响应与日志都不含 token**：只回 uid/nickname/成败/原因。
//   - 落盘复用 auth.Save（文件名匹配 LoadDir 扫描约定，0600 原子写）。
//   - 已存在的同 uid 会被覆盖更新（upsert），池内该账号的积分/冷却状态保留。
func (h *Handler) accountsImport(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_dir_unset",
			"网关未配置 auth_dir，无法导入账号")
		return
	}
	body, ok := h.readBody(w, r, func(status int, msg string) {
		writeOpenAIError(w, status, "invalid_request", msg)
	})
	if !ok {
		return
	}

	accounts, results, err := auth.ParseImport(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "import_parse_failed", err.Error())
		return
	}

	imported := 0
	for i, a := range accounts {
		path, err := a.Save(h.cfg.AuthDir)
		if err != nil {
			// 找到对应结果条目回填失败原因（ok 列表与 results 顺序一致，
			// 但 results 里含失败条目，故按 uid 匹配更稳）。
			markImportFailed(results, a.UID, "写文件失败: "+err.Error())
			log.Printf("ERR: [accounts] import uid=%s: %v", logUID(a.UID), err)
			continue
		}
		h.cfg.Pool.Add(a) // 落盘成功才入池（与单账号添加同序）
		markImportSaved(results, a.UID, path)
		imported++
		_ = i
	}

	failed := 0
	for _, res := range results {
		if !res.OK {
			failed++
		}
	}
	log.Printf("[accounts] 导入完成：成功 %d / 失败 %d（共 %d 条）", imported, failed, len(results))
	writeJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"failed":   failed,
		"total":    len(results),
		"results":  results,
	})
}

// markImportSaved 把写盘路径回填到对应结果条目。
func markImportSaved(results []auth.ImportResult, uid, path string) {
	for i := range results {
		if results[i].UID == uid && results[i].OK {
			results[i].File = path
			return
		}
	}
}

// markImportFailed 把某条标记为失败并附原因（写盘阶段出错）。
func markImportFailed(results []auth.ImportResult, uid, msg string) {
	for i := range results {
		if results[i].UID == uid && results[i].OK {
			results[i].OK = false
			results[i].Error = msg
			results[i].File = ""
			return
		}
	}
}

// accountsRevive 复活一个被禁用的账号（清 disabled/reason/12153 计数）。
// 不修改凭证，也不影响其他账号；账号回到池中由既有健康检查接管。
//
// POST /accounts/{uid}/revive
func (h *Handler) accountsRevive(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_uid", "缺少 uid")
		return
	}
	st, ok := h.cfg.Pool.Status(uid)
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "unknown_uid", "账号不存在")
		return
	}
	if !st.Disabled {
		// 幂等：未禁用的账号不需要复活，直接回报当前状态。
		writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": false, "changed": false})
		return
	}
	h.cfg.Pool.ReviveDisabled(uid)
	log.Printf("[accounts] 复活账号 uid=%s（原禁用原因：%s）", logUID(uid), st.DisabledReason)
	writeJSON(w, http.StatusOK, map[string]any{"uid": uid, "disabled": false, "changed": true})
}

// deletedDirName 回收站目录名（位于 auth_dir 下）。
// 放 auths/ 内部而非外部的原因：删除是"把凭据移出可用集合"，
// 回收站与凭据同生命周期，备份 auths/ 时一并带走，不会漏。
const deletedDirName = ".deleted"

// accountsDelete 删除账号：软删除（凭据移入回收站）+ 从池中移除 + 清池状态。
//
// DELETE /accounts/{uid}
// 响应：{"uid":..., "deleted":true, "file":"<回收站路径>"}
//
// 为什么是软删除：账号凭证是不可再生的（丢了要重新走 OAuth 登录），
// 而删除按钮就在列表里、误点成本极高。移入 auth_dir/.deleted/ 后：
//   - 立即从池中消失、不再被选号（满足"删除"的语义）；
//   - 文件还在，误删可手动拷回 auths/ 再重启恢复。
//
// 安全要点：
//   - **只操作 auth_dir 下的文件**：路径由 FileNameFor 生成（已净化 uid），
//     绝不接受调用方传来的路径，杜绝路径穿越删任意文件；
//   - 只删**我们自己管理**的命名（workbuddy-*.json）；即便 uid 合法，
//     若目标文件名不符该模式也拒绝——避免删掉用户放进 auths/ 的其他东西；
//   - 账号正在被请求占用（in_flight > 0）时删除仍允许，但会记日志：
//     删除后该请求自然失败，不会破坏状态一致性。
func (h *Handler) accountsDelete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_dir_unset",
			"网关未配置 auth_dir，无法删除账号")
		return
	}
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_uid", "缺少 uid")
		return
	}

	// 池内不存在时**不直接 404**：文件可能存在但未被加载（例如刚放进去还没重启）。
	// 仍按 uid 推导文件名尝试软删，这样"删掉一个没加载的账号"也能工作；
	// 文件也不存在才报 404。
	inPool := false
	if st, ok := h.cfg.Pool.Status(uid); ok {
		inPool = true
		if st.InFlight > 0 {
			log.Printf("WARN: [accounts] 删除账号 uid=%s 时有 %d 个在途请求（该请求将失败）",
				logUID(uid), st.InFlight)
		}
	}

	// 文件名由 FileNameFor 按 uid 推导——不接受外部路径输入。
	name := auth.FileNameFor(uid)
	src := filepath.Join(h.cfg.AuthDir, name)
	if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
		// 双保险：FileNameFor 的产物必须匹配 LoadDir 扫描模式。
		writeOpenAIError(w, http.StatusBadRequest, "bad_uid", "uid 非法")
		return
	}

	dst, err := h.moveToRecycleBin(src, uid)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && !inPool {
			writeOpenAIError(w, http.StatusNotFound, "unknown_uid", "账号不存在")
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			// 池里有、文件不在：可能是同名文件被手工改名。仍从池中移除。
			h.cfg.Pool.RemoveUID(uid)
			writeJSON(w, http.StatusOK, map[string]any{
				"uid": uid, "deleted": true, "file": "",
				"warning": "凭证文件不存在，仅从池中移除（重启后不会恢复）",
			})
			return
		}
		log.Printf("ERR: [accounts] 删除账号 uid=%s: %v", logUID(uid), err)
		writeOpenAIError(w, http.StatusInternalServerError, "delete_failed", err.Error())
		return
	}

	// 文件已移走 → 从池中移除（内部会立即落盘，避免重启复活）。
	removed := h.cfg.Pool.RemoveUID(uid)
	log.Printf("[accounts] 删除账号 uid=%s（文件已移入回收站，池内移除=%v）", logUID(uid), removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":     uid,
		"deleted": true,
		"file":    dst,
	})
}

// moveToRecycleBin 把 src 移入 <auth_dir>/.deleted/<时间戳>-<原名>。
// 返回回收站内的路径。src 不存在时返回 os.ErrNotExist。
//
// 用 rename 而非"读+写+删"：同一文件系统内 rename 是原子的，
// 且不会出现"复制成功但删除失败"导致账号同时存在两份的中间态。
func (h *Handler) moveToRecycleBin(src, uid string) (string, error) {
	if _, err := os.Stat(src); err != nil {
		return "", err
	}
	dir := filepath.Join(h.cfg.AuthDir, deletedDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建回收站目录失败: %w", err)
	}
	// 时间戳前缀避免同名覆盖（重复删除/重新添加同一 uid 时）。
	stamp := time.Now().Format("20060102-150405")
	dst := filepath.Join(dir, stamp+"-"+filepath.Base(src))
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("移入回收站失败: %w", err)
	}
	_ = uid
	return dst, nil
}

// logUID 日志用 uid 前缀（避免整串 uid 进日志）。
func logUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "-"
	}
	return uid
}

// exportConfirmPhrase 导出凭证所需的确认短语。
//
// 为什么需要它：导出文件含**明文 accessToken/refreshToken**，落地到下载目录后
// 可能被索引、同步到网盘、或残留很久。加一道显式确认，避免误点或被诱导点击
// （导出是不可逆的信息泄露，而删除还有回收站兜底）。
const exportConfirmPhrase = "EXPORT"

// accountsExport 导出全部账号的**完整凭证**（含明文 token），用于备份 / 迁移。
//
// POST /accounts/export
// 请求体：{"confirm":"EXPORT"}  ← 必须命中确认短语，否则 400
// 响应：JSON 数组，每项为**嵌套形**（与 auth.SaveAtomic 写出的完全一致）
//
//	[{"account":{"uid":...,"nickname":...,"enterpriseId":...},
//	  "auth":{"accessToken":...,"refreshToken":...,"expiresAt":...,"domain":...}}, ...]
//
// ── 安全边界（重要，与其他账号端点的取舍不同）──────────────────────────
// 本项目对 OAuth 登录流程有一条硬边界：**token 全程不经过浏览器**
// （见本文件开头注释；那是为了让「网页授权登录」不必把凭证交给前端）。
// 本端点**有意突破该边界**：导出必须让 token 经过浏览器才能落到用户的下载目录，
// 这是备份功能的本质要求，无法规避。之所以可接受：
//   - 反方向的「导入 JSON」早已允许 token 经浏览器上行（粘贴/拖文件），
//     本端点只是把同一条数据通路补齐为双向，未引入新的信任假设；
//   - 访问需通过看板 Basic Auth（与 import/delete/revive 同一鉴权口径），
//     且额外要求确认短语；
//   - 日志只记条数，**绝不记 token**。
//
// 代价需明确知悉：导出文件是**不可再生的凭证**，泄露等同于交出账号。
//
// ── 为什么用池而非扫盘 ────────────────────────────────────────────────
// 池内存的是**已加载且 token 最新**的账号（refresh 成功后内存与磁盘同步更新），
// 且与看板账号列表（/status）同源——"导出你看到的这些账号"最不意外。
// auths/.deleted/ 回收站里的账号不属于当前池，**不会**被导出。
func (h *Handler) accountsExport(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "auth_dir_unset",
			"网关未配置 auth_dir，无法导出账号")
		return
	}

	// 读取确认短语。空 body / 非法 JSON 都视为未确认（不静默放行）。
	var req struct {
		Confirm string `json:"confirm"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
		if err == nil && len(raw) > 0 {
			_ = json.Unmarshal(raw, &req)
		}
	}
	if strings.TrimSpace(req.Confirm) != exportConfirmPhrase {
		writeOpenAIError(w, http.StatusBadRequest, "confirm_required",
			"导出凭证需显式确认：请求体传 {\"confirm\":\""+exportConfirmPhrase+"\"}。该文件含明文 token，请确认下载目录安全")
		return
	}

	states := h.cfg.Pool.List()
	docs := make([]any, 0, len(states))
	skipped := 0
	for _, st := range states {
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			skipped++
			continue
		}
		// 持锁读取，避免与并发 refresh 写出半更新字段（token 与 expiresAt 不配对）。
		a.Lock()
		if strings.TrimSpace(a.AccessToken) == "" {
			// 空 token 不导出：导出去也导不回来（ParseImport 会判缺 access_token），
			// 反而让备份文件看起来"有这条账号"却恢复失败。
			a.Unlock()
			skipped++
			continue
		}
		doc := map[string]any{
			"account": map[string]any{
				"uid":          a.UID,
				"nickname":     a.Nickname,
				"enterpriseId": a.EnterpriseID,
			},
			"auth": map[string]any{
				"accessToken":  a.AccessToken,
				"refreshToken": a.RefreshToken,
				"expiresAt":    a.ExpiresAt,
				"domain":       a.Domain,
			},
		}
		a.Unlock()
		docs = append(docs, doc)
	}

	// 日志只记条数，绝不记 token。
	log.Printf("[accounts] 导出凭证：%d 个账号（跳过 %d）", len(docs), skipped)

	name := "workbuddy-accounts-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// 凭证内容绝不能被缓存（共享机器/代理上残留）。
	w.Header().Set("Cache-Control", "no-store")
	out, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "export_failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
