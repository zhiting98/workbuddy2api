// relay.go 三个协议端点（/v1/chat/completions、/v1/responses、/v1/messages）共用的
// 「账号池挑号 + 会话粘性 + 轮转 + 上游 SSE 管线」。
//
// 分工：本文件只负责"把一份**已经是 chat 形状**的请求体可靠地送到上游并拿回 SSE 流"，
// 协议转换（Responses/Anthropic ↔ Chat）完全由 responses.go / messages.go 承担。
// 这样账号池、粘性、冷却、熔断、指纹降级等既有语义在三条路径上完全一致，
// 不会因为新增端点而出现"某个端点不粘性/不冷却"的漂移。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// chatRequest 上游请求的中转描述：协议层解析出的 chat 请求体 + 少量元信息。
//
// Body 必须是**完整合法的 chat 请求 JSON**（messages/model/stream 等），
// 协议层负责保证这一点；relay 不改写业务字段（只做出站前的通用改写）。
type chatRequest struct {
	Body  []byte
	Model string // 供模型感知选号（PickExcludingForModel）与 6004 模型级冷却记录
	// Stream 客户端是否要求流式。上游恒为流式（PrepareBodyOpt 强制 stream:true），
	// 此字段只决定"上游 SSE 如何回给客户端"：直通 or 聚合。
	Stream bool
}

// relayResult 一次 relay 的终态，仅用于请求级表格日志（chatStat）与调用流水。
type relayResult struct {
	// Status 最终回给客户端的 HTTP 状态码。
	Status int
	// UID/Nick 最终尝试（成功时即成功）的账号，供日志与调用流水。
	UID  string
	Nick string
}

// relay 把 chatRequest 送到上游，成功时调用 emit 消费 SSE 流。
//
// emit 语义：**已被成功调用 = 响应已开始（或已完整写出）**，relay 不再写任何
// HTTP 错误。emit 返回 error 表示写客户端失败（对端断开等），relay 直接返回。
// 非 2xx / 网络失败 / 全池耗尽时 emit 不被调用，由 relay 写协议层错误
// （writeErr 由调用方注入，保证错误体是客户端协议的形状）。
//
// 参数：
//   - w/r：原始请求（粘性 key 从 Body 提取，与端点无关）。
//   - req：协议层已转换好的 chat 请求。
//   - emit：消费上游 SSE 流的回调（流式=边转边发，非流式=Aggregate 后整包回）。
//     acct 为最终成功的账号，供 emit 记录分账号统计。
//     返回 (ttfb, completionTokens)：ttfb 为首个上游数据帧耗时（无则 0），
//     completionTokens 为上游 usage.completion_tokens（缺失则 -1，日志显示 "-"）。
//   - writeErr：协议层错误写出（Responses/Anthropic 各自的错误 envelope）。
//
// 复用既有全部语义：粘性绑定/解绑、在途租约、token 预刷新、错误分类与冷却策略、
// 内容拦截误报的降级重试（Degraded 提示词）。
func (h *Handler) relay(w http.ResponseWriter, r *http.Request,
	req chatRequest, emit func(rc io.Reader, acct *auth.Auth) (time.Duration, int),
	writeErr func(status int, msg string)) {

	res := &relayResult{Status: http.StatusOK}

	st := newChatStat(time.Now(), req.Body, req.Stream)
	defer func() {
		st.uid = res.UID
		st.nick = res.Nick
		st.status = res.Status
		st.done()
		if h.cfg.CallTrack != nil {
			h.cfg.CallTrack.Record(st.uid, st.nick, st.model, st.mode, st.status)
		}
	}()

	body := req.Body
	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	sessKey := ""
	stickyUID := ""
	if h.cfg.Session != nil {
		sessKey = session.ExtractKey(body)
		if sessKey != "" {
			if uid, ok := h.cfg.Session.Resolve(sessKey); ok {
				stickyUID = uid
			}
		}
	}

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//     协议层已把 Responses 的 instructions / Anthropic 的 system 落成 system 消息，
	//     故此处的替换规则天然覆盖这两个新端点（issue #36 的 11128 指纹路径）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough 非降级期：透传客户端原始 system（不改写）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "passthrough" && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUID 已校验 health + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUID(stickyUID)
			if acct == nil {
				// 粘性号当前不可用（冷却/占满）→ 解绑，本次回落普通轮换。
				unbindSticky()
			}
		}
		if acct == nil {
			// 模型感知选号：请求携带 model 时启用 6004 模型级冷却豁免
			// （PickExcludingForModel 内部当 model 为空时即退化为 PickExcluding）。
			acct = h.cfg.Pool.PickExcludingForModel(tried, req.Model)
		}
		if acct == nil {
			res.Status = http.StatusServiceUnavailable
			break
		}
		res.UID = acct.UID
		res.Nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 走连续计数阈值（同 applyErrorPolicy）：单次 refresh 12153 多为竞态，
					// 不应立即禁用账号。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] refresh uid=%s: save auth failed: %v", logfmt.UID8(acct.UID), err)
			}
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body)
		if terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 上游 client 已打 transport error 日志。
			res.Status = http.StatusServiceUnavailable
			lastErr = terr
			fail(acct.UID)
			continue
		}
		if status >= 400 {
			res.Status = status
			kind := upstream.Classify(status, string(respBody))
			// 内容拦截误报（passthrough 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试。
			// 第二次仍被拦（用户内容本身触发审核）→ 走既有错误路径返回客户端。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && h.cfg.PromptMode == "passthrough" && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), req.Model)
			fail(acct.UID)
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}

		res.Status = http.StatusOK
		// emit 内部完成"读上游 SSE → 写客户端"的全部工作。
		// 写失败（对端断开）时状态码已发出，无法再改，故忽略返回值。
		ttfb, toks := emit(rc, acct)
		rc.Close()
		st.ttfb = ttfb
		if toks >= 0 {
			st.toks = toks
		}
		return
	}

	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeErr(http.StatusServiceUnavailable, msg)
	res.Status = http.StatusServiceUnavailable
}

// readBody 读取请求体并施加网关侧大小上限。
// 返回 ok=false 表示已写出错误响应（调用方直接 return）。
//
// 与 chatCompletions 同口径：LimitReader 读 limit+1 探测"超限"，超限直接 413，
// 不把截断的半截 JSON 喂给上游（issue #41）。413 是客户端问题，不打上游、不罚账号。
//
// Claude Code 启动即发送超长 system prompt + 大量工具定义，端点属于大请求路径，
// 上限同样受 server.max_body_mb 约束（默认 8MB 一般够用，可按需调大）。
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, writeErr func(status int, msg string)) ([]byte, bool) {
	limit := h.cfg.MaxBodyBytes
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeErr(http.StatusBadRequest, "read body: "+err.Error())
		return nil, false
	}
	if int64(len(body)) > limit {
		writeErr(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("请求体超过 %d MB 上限：请压缩内容或调大 server.max_body_mb 配置后重试", limit>>20))
		return nil, false
	}
	return body, true
}

// sseWriter 负责把协议层生成的事件按 SSE 帧写回客户端并即时 flush。
// Claude Code / Codex 都依赖逐事件增量到达，故每次写帧后必须 Flush。
type sseWriter struct {
	w  http.ResponseWriter
	fl http.Flusher
}

// newSSEWriter 设置 SSE 响应头并返回帧写入器。
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	return &sseWriter{w: w, fl: fl}
}

// event 写一个具名事件（Responses API 与 Anthropic Messages 都用 `event:` 行）。
// payload 为任意可 JSON 序列化结构。
func (s *sseWriter) event(name string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		return err
	}
	s.flush()
	return nil
}

// rawEvent 写出已序列化好的 data（用于需要精确控制 JSON 形状的场景）。
func (s *sseWriter) rawEvent(name string, data []byte) error {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return err
	}
	s.flush()
	return nil
}

// flush 立即把已写帧推给客户端（http.Flusher 缺失时静默跳过，便于测试用 recorder）。
func (s *sseWriter) flush() {
	if s.fl != nil {
		s.fl.Flush()
	}
}

// counter64 进程级单调递增计数器（生成 resp_/msg_/fc_ 等 id 的序号后缀）。
type counter64 struct{ v atomic.Int64 }

// Add 原子自增并返回新值。
func (c *counter64) Add(delta int64) int64 { return c.v.Add(delta) }

// sseFrame 解析一行上游 SSE：返回事件名与 data 负载；非 data 行返回 ok=false。
//
// 上游（CodeBuddy chat completions）只发 `data: {...}`（无 event 行），事件名恒为空，
// 但保留 name 解析以便兼容带 event 行的上游变体。
func sseFrame(line string) (name string, payload string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "data: "):
		return "", strings.TrimPrefix(line, "data: "), true
	case line == "data:":
		return "", "", true
	case strings.HasPrefix(line, "event: "):
		return strings.TrimPrefix(line, "event: "), "", false
	default:
		return "", "", false
	}
}
