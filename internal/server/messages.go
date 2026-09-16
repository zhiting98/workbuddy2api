// messages.go 实现 Anthropic Messages API（POST /v1/messages）→ chat 协议转换。
//
// 背景：Claude Code 只支持 /v1/messages，直连网关的 /v1/chat/completions 会 404。
// 本文件只做「协议转换层」：请求 Anthropic → chat（喂给既有上游管线），
// 响应用 chat SSE → Anthropic 事件序列。
//
// 账号池 / 会话粘性 / 轮转 / 冷却 / 指纹降级全部复用 relay.go 的 relay()。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 请求侧：Anthropic Messages → Chat
// ---------------------------------------------------------------------------

// anthropicRequest Anthropic Messages 请求体（只声明网关关心的字段）。
type anthropicRequest struct {
	Model  string          `json:"model"`
	System json.RawMessage `json:"system"` // string 或 [{type:"text",text}]
	// Messages 的 content 是 string 或 content block 数组，用宽松结构承接。
	Messages []anthropicMessage `json:"messages"`
	// Tools 形状为 [{name, description, input_schema}]，与 chat 的嵌套形状不同。
	Tools []anthropicTool `json:"tools"`
	// ToolChoice: {type:"auto"|"any"|"tool", name}。
	ToolChoice    json.RawMessage `json:"tool_choice"`
	MaxTokens     int             `json:"max_tokens"` // Anthropic 必填
	StopSequences []string        `json:"stop_sequences"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	Stream        bool            `json:"stream"`
	// Metadata / Thinking 忽略或降级（清单 §3）。
	Metadata json.RawMessage `json:"metadata"`
	Thinking json.RawMessage `json:"thinking"`
}

// anthropicMessage 单条消息；content 为 string 或 block 数组。
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicTool Anthropic 工具定义。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicBlock content 数组里的单个 block。
type anthropicBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text"`
	// tool_use block：input 是**对象**（chat 的 arguments 是**字符串**，必须序列化）
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result block
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	// thinking block
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

// messages 处理 POST /v1/messages。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	writeErr := func(status int, msg string) {
		writeAnthropicError(w, status, msg)
	}
	body, ok := h.readBody(w, r, writeErr)
	if !ok {
		return
	}
	req, err := parseAnthropicRequest(body)
	if err != nil {
		writeErr(http.StatusBadRequest, err.Error())
		return
	}
	chatBody, err := buildAnthropicChat(req)
	if err != nil {
		writeErr(http.StatusBadRequest, err.Error())
		return
	}

	h.relay(w, r, chatRequest{Body: chatBody, Model: req.Model, Stream: req.Stream},
		func(rc io.Reader, acct *auth.Auth) (time.Duration, int) {
			if req.Stream {
				return h.messagesStream(w, rc, req.Model, acct)
			}
			return h.messagesSync(w, rc, req.Model, acct)
		},
		writeErr)
}

// parseAnthropicRequest 解析 Anthropic 请求体。
func parseAnthropicRequest(body []byte) (*anthropicRequest, error) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("missing required field: model")
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("missing required field: messages")
	}
	return &req, nil
}

// buildAnthropicChat 把 Anthropic Messages 请求转换为完整 chat 请求 JSON。
//
// 映射规则（清单 §3）：
//   - system（string 或 block 数组）→ 首条 {role:"system", content:<拼接文本>}
//   - messages[].content string      → 直接透传
//   - content [{type:"text"}]        → 拼接为 content 字符串
//   - [{type:"tool_use"}]            → assistant.tool_calls[{id, type, function:{name, arguments:JSON(input)}}]
//   - [{type:"tool_result"}]         → {role:"tool", tool_call_id:tool_use_id, content}
//   - tools [{name, description, input_schema}] → [{type:"function", function:{name, description, parameters:input_schema}}]
//   - tool_choice {type:"auto"|"any"|"tool"} → "auto"|"required"|{type:"function",function:{name}}
//   - stop_sequences → stop
func buildAnthropicChat(req *anthropicRequest) ([]byte, error) {
	system := anthropicSystemText(req.System)

	msgs := make([]any, 0, len(req.Messages)+1)
	if system != "" {
		// Anthropic 的 system 是**顶层字段**，chat 侧落地为 system 消息。
		// prompt.Rewrite 会识别并替换它（custom 模式），从而覆盖 11128 指纹路径。
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	converted, err := anthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, converted...)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("messages is empty: nothing to send upstream")
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	// max_tokens 是 Anthropic 必填；缺省时不写（让上游用默认），避免 0 被上游拒绝。
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				var schema any
				if err := json.Unmarshal(t.InputSchema, &schema); err == nil {
					fn["parameters"] = schema
				}
			} else {
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		if tc := convertAnthropicToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	// Anthropic 的 thinking 字段（{type:"enabled", budget_tokens:N}）与 chat 的
	// thinking（{type:"enabled"}）+ reasoning_effort 语义不同：gateway 的上游
	// thinking.go 已按模型自行注入正确开关，此处不转发，避免语义冲突（清单 §3「忽略或降级」）。

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return raw, nil
}

// anthropicSystemText 把 system 字段（string 或 block 数组）拍平成文本。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text", "":
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// convertAnthropicToolChoice 把 Anthropic 的 tool_choice 转成 chat 形状。
//
//	{type:"auto"}          → "auto"
//	{type:"any"}           → "required"
//	{type:"tool", name}    → {type:"function", function:{name}}
//	{type:"none"}          → "none"
//
// 对象形式随后由 upstream.normalizeToolChoice 继续归一为上游要求的字符串。
func convertAnthropicToolChoice(raw json.RawMessage) any {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		// 已是字符串（非 Anthropic 规范，容错处理）。
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return nil
	}
	typ, _ := obj["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name, _ := obj["name"].(string)
		if strings.TrimSpace(name) == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	default:
		return nil
	}
}

// anthropicMessages 把 Anthropic 消息数组转换为 chat messages。
//
// 一个 Anthropic message 的 content 数组可能**同时**含 text 与 tool_use
// （assistant 边说话边调工具），这对应 chat 的一条 assistant 消息
// （content 为文本 + tool_calls 并存）；tool_result block 则必须升级为
// 独立的 role:"tool" 消息（chat 规范要求 tool 结果自成一条）。
func anthropicMessages(in []anthropicMessage) ([]any, error) {
	out := make([]any, 0, len(in))
	for _, m := range in {
		role := m.Role
		if role == "" {
			role = "user"
		}
		// content 为 string → 直接透传。
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			out = append(out, map[string]any{"role": role, "content": s})
			continue
		}
		var blocks []anthropicBlock
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			return nil, fmt.Errorf("invalid message content for role %q: expected string or block array", role)
		}

		var (
			textParts  strings.Builder
			toolCalls  []any
			toolResult []map[string]any
			thinking   strings.Builder
		)
		for _, blk := range blocks {
			switch blk.Type {
			case "text", "":
				textParts.WriteString(blk.Text)
			case "tool_use":
				// input 是对象 → arguments 必须是字符串（序列化）。
				args := "{}"
				if len(blk.Input) > 0 {
					if raw, err := compactJSON(blk.Input); err == nil {
						args = raw
					}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   blk.ID,
					"type": "function",
					"function": map[string]any{
						"name":      blk.Name,
						"arguments": args,
					},
				})
			case "tool_result":
				// tool_result → 独立 role:"tool" 消息（chat 规范要求）。
				toolResult = append(toolResult, map[string]any{
					"role":         "tool",
					"tool_call_id": blk.ToolUseID,
					"content":      anthropicBlockText(blk.Content),
				})
			case "thinking", "redacted_thinking":
				// Anthropic 的 thinking block：多轮回放时 DeepSeek 要求 assistant
				// 消息带 reasoning_content（见 internal/upstream/thinking.go）。
				if blk.Thinking != "" {
					thinking.WriteString(blk.Thinking)
				}
			default:
				// image / document 等：上游 chat 侧无对应能力，忽略该 block。
				continue
			}
		}

		// tool_result 先于本轮消息插入（它们是上一轮 tool_use 的结果，
		// 在 Anthropic 里恰好位于下一个 user message 的 content 中）。
		out = append(out, anyMessages(toolResult)...)

		// 本轮消息：有 tool_calls 或 thinking 时必须保留 assistant 消息本体。
		text := textParts.String()
		if toolCalls != nil || thinking.Len() > 0 || text != "" {
			msg := map[string]any{"role": role}
			if text == "" && toolCalls != nil {
				msg["content"] = nil // chat 规范：纯工具调用轮 content 为 null
			} else {
				msg["content"] = text
			}
			if toolCalls != nil {
				msg["tool_calls"] = toolCalls
			}
			if thinking.Len() > 0 {
				msg["reasoning_content"] = thinking.String()
			}
			out = append(out, msg)
		}
	}
	return out, nil
}

// anyMessages 把 []map[string]any 转成 []any（JSON 数组元素的通用形状）。
func anyMessages(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, m := range in {
		out[i] = m
	}
	return out
}

// anthropicBlockText 把 tool_result 的 content（string 或 text block 数组）拍平。
func anthropicBlockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text", "":
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// compactJSON 紧凑序列化 RawMessage（去掉空白，保持 arguments 是合法 JSON 字符串）。
func compactJSON(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------------------------------------------------------------------------
// 响应侧：Chat SSE → Anthropic Messages SSE
// ---------------------------------------------------------------------------

// msgIDSeq 进程级 message id 序号（保证同进程内 id 唯一）。
var msgIDSeq counter64

// newAnthropicMessageID 生成 `msg_...` 形式的 id（Claude Code 依赖此前缀）。
func newAnthropicMessageID() string {
	return fmt.Sprintf("msg_%d%06d", time.Now().UnixNano(), msgIDSeq.Add(1)%1000000)
}

// messagesStream 消费上游 chat SSE，边转边发 Anthropic 事件序列。
//
// 事件顺序（Claude Code 严格依赖，清单 §3）：
//
//	message_start            （message 骨架 + usage）
//	content_block_start      （index + content_block: text | tool_use）
//	content_block_delta      （text_delta | input_json_delta，可多次）
//	content_block_stop       （index）
//	message_delta            （{delta:{stop_reason, stop_sequence:null}, usage:{output_tokens}}）
//	message_stop
//
// 一个输出可含多个 content_block（先 text 后 tool_use）；tool_use 参数以
// input_json_delta 增量下发。
func (h *Handler) messagesStream(w http.ResponseWriter, rc io.Reader, model string, acct *auth.Auth) (time.Duration, int) {
	sw := newSSEWriter(w)
	id := newAnthropicMessageID()
	start := time.Now()

	// message_start 必须先发（骨架里的 usage.input_tokens 此刻未知，先置 0，
	// 最终值由 message_delta 的 output_tokens 与 message_start 的 input_tokens 组合）。
	if err := sw.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return 0, -1
	}

	var (
		textBuf   strings.Builder
		thinkBuf  strings.Builder
		toolCalls = map[int]map[string]any{}
		toolOrder []int

		thinkStarted bool
		thinkIndex   int
		textStarted  bool
		textIndex    int
		nextIndex    int

		usage     map[string]any
		finish    = "stop"
		ttfb      time.Duration
		seenFrame bool
	)

	// openThinking 惰性开 thinking content_block。
	//
	// Anthropic 规范要求 thinking 块**在正文之前**，故必须在开 text 块前先开它；
	// 关闭顺序由 finishThinking 保证（见收尾逻辑）。
	openThinking := func() error {
		if thinkStarted {
			return nil
		}
		if textStarted {
			// thinking 迟到（上游先吐正文再吐思维链）：此时正文块已开，
			// 不能再插到它前面。保持规范优先——晚到的思维链丢弃，避免产出
			// 非法的事件序列（Claude Code 会因 index 乱序解析失败）。
			return nil
		}
		thinkStarted = true
		thinkIndex = nextIndex
		nextIndex++
		return sw.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": thinkIndex,
			"content_block": map[string]any{
				"type":     "thinking",
				"thinking": "",
			},
		})
	}

	// finishThinking 关闭 thinking 块（开正文块前调用；未开则空操作）。
	finishThinking := func() error {
		if !thinkStarted {
			return nil
		}
		thinkStarted = false // 只关一次
		return sw.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": thinkIndex,
		})
	}

	// openText 惰性开 text content_block。开正文前必须先收掉 thinking 块，
	// 否则两个块会交错（Anthropic 要求块内事件连续、index 单调）。
	openText := func() error {
		if textStarted {
			return nil
		}
		if e := finishThinking(); e != nil {
			return e
		}
		textStarted = true
		textIndex = nextIndex
		nextIndex++
		return sw.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": textIndex,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		})
	}

	br := bufio.NewReaderSize(rc, 64*1024)
	readErr := error(nil)
readLoop:
	for {
		line, err := br.ReadString('\n')
		_, payload, isData := sseFrame(line)
		if isData {
			if payload == "[DONE]" {
				break readLoop
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				if !seenFrame {
					seenFrame = true
					ttfb = time.Since(start)
				}
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				if chs, ok := chunk["choices"].([]any); ok && len(chs) > 0 {
					c, _ := chs[0].(map[string]any)
					if c != nil {
						if fr, ok := c["finish_reason"].(string); ok && fr != "" {
							finish = fr
						}
						if d, ok := c["delta"].(map[string]any); ok {
							// 思维链增量 → thinking 块（block 内 delta 用 thinking_delta）。
							// 与 /v1/responses 的 reasoning item 同源：上游 chat 的
							// delta.reasoning_content。非流式路径（chatToAnthropic）一直
							// 有这一步，流式此前漏掉，导致 Claude Code 看不到思维链。
							if v, ok := d["reasoning_content"].(string); ok && v != "" {
								if e := openThinking(); e != nil {
									readErr = e
									break readLoop
								}
								if thinkStarted {
									thinkBuf.WriteString(v)
									if e := sw.event("content_block_delta", map[string]any{
										"type":  "content_block_delta",
										"index": thinkIndex,
										"delta": map[string]any{"type": "thinking_delta", "thinking": v},
									}); e != nil {
										readErr = e
										break readLoop
									}
								}
							}
							// 正文增量 → text_delta
							if v, ok := d["content"].(string); ok && v != "" {
								if e := openText(); e != nil {
									readErr = e
									break readLoop
								}
								textBuf.WriteString(v)
								if e := sw.event("content_block_delta", map[string]any{
									"type":  "content_block_delta",
									"index": textIndex,
									"delta": map[string]any{"type": "text_delta", "text": v},
								}); e != nil {
									readErr = e
									break readLoop
								}
							}
							// 工具调用增量（index 合并，arguments 是片段拼接）
							if tcs, ok := d["tool_calls"].([]any); ok {
								for _, tci := range tcs {
									call, ok := tci.(map[string]any)
									if !ok {
										continue
									}
									idx := 0
									if v, ok := call["index"].(float64); ok {
										idx = int(v)
									}
									merged, seen := toolCalls[idx]
									if !seen {
										merged = map[string]any{"index": idx}
										toolCalls[idx] = merged
										toolOrder = append(toolOrder, idx)
									}
									upstream.MergeToolCallDelta(merged, call)
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			readErr = err
			break
		}
	}
	if readErr != nil {
		return ttfb, -1
	}

	// ── 收尾 ──
	// 0) 关闭 thinking block（若开了且尚未关）：
	//    正常路径下 openText 已收掉它；但"只有思维链、没有正文"的回复会走到这里，
	//    此时必须补发 content_block_stop，否则 Claude Code 收到未闭合的块。
	if e := finishThinking(); e != nil {
		return ttfb, -1
	}
	// 1) 关闭 text block（有则发 stop；无 text 也无工具时补一个空 text block，
	//    保证 Claude Code 至少拿到一个 content_block——空回复也是合法 message）。
	if textStarted {
		if e := sw.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": textIndex,
		}); e != nil {
			return ttfb, -1
		}
	}
	// 2) 工具调用 block：tool_use 需在 text 之后（Claude Code 按 index 顺序渲染）。
	upstream.SortInts(toolOrder)
	for _, idx := range toolOrder {
		tc := toolCalls[idx]
		callID, name, args := "", "", ""
		callID, _ = tc["id"].(string)
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		blockIndex := nextIndex
		nextIndex++
		// content_block_start：tool_use 的 input 先给空对象，参数随后以增量下发。
		if e := sw.event("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": blockIndex,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    callID, // 原样透传上游 tool_call.id，Claude Code 回传 tool_result 依赖它
				"name":  name,
				"input": map[string]any{},
			},
		}); e != nil {
			return ttfb, -1
		}
		if args != "" {
			if e := sw.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			}); e != nil {
				return ttfb, -1
			}
		}
		if e := sw.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		}); e != nil {
			return ttfb, -1
		}
	}
	// 无任何 content block：补一个空 text block，避免客户端解析空 content 数组。
	if !textStarted && len(toolOrder) == 0 {
		_ = sw.event("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		_ = sw.event("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		})
	}

	_, completion := usageInts(usage)
	// 3) message_delta：stop_reason 映射 + output_tokens。
	if e := sw.event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(finish, len(toolOrder) > 0),
			"stop_sequence": nil,
		},
		"usage": map[string]any{"output_tokens": completion},
	}); e != nil {
		return ttfb, -1
	}
	// 4) message_stop。
	_ = sw.event("message_stop", map[string]any{"type": "message_stop"})

	if h.cfg.Stats != nil {
		p, c := usageInts(usage)
		h.cfg.Stats.Record(model, acct.UID, p, c, p+c, http.StatusOK)
	}
	return ttfb, completion
}

// messagesSync 聚合上游 chat SSE 为完整 Anthropic message（非流式）。
func (h *Handler) messagesSync(w http.ResponseWriter, rc io.Reader, model string, acct *auth.Auth) (time.Duration, int) {
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, err.Error())
		return 0, -1
	}
	out := chatToAnthropic(resp, model)
	writeJSON(w, http.StatusOK, out)
	if h.cfg.Stats != nil {
		if p, c, t, ok := usageTriple(resp); ok {
			h.cfg.Stats.Record(model, acct.UID, p, c, t, http.StatusOK)
		}
	}
	return 0, completionTokens(resp)
}

// chatToAnthropic 把聚合后的 chat.completion 转成 Anthropic message 对象。
//
// 形状：
//
//	{id, type:"message", role:"assistant", model,
//	 content:[{type:"text",text} | {type:"tool_use",id,name,input}],
//	 stop_reason, stop_sequence, usage:{input_tokens, output_tokens}}
//
// tool_use 的 input 需把 chat 的 arguments **字符串反序列化回对象**。
func chatToAnthropic(resp map[string]any, model string) map[string]any {
	msg := firstChoiceMessage(resp)
	content := make([]any, 0, 2)

	if r, ok := msg["reasoning_content"].(string); ok && r != "" {
		// Anthropic 有 thinking block，回放给 Claude Code 保持思维链可见。
		content = append(content, map[string]any{"type": "thinking", "thinking": r})
	}
	if txt, ok := msg["content"].(string); ok && txt != "" {
		content = append(content, map[string]any{"type": "text", "text": txt})
	}
	hasToolUse := false
	for _, tc := range toolCallsOf(msg) {
		callID, name, args := splitToolCall(tc)
		// arguments（字符串）→ input（对象）。
		input := any(map[string]any{})
		if strings.TrimSpace(args) != "" {
			var v any
			if json.Unmarshal([]byte(args), &v) == nil {
				input = v
			} else {
				// 上游给出畸形 arguments：保留原文，避免整单失败。
				input = map[string]any{"_raw": args}
			}
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    callID, // 原样透传，tool_result 匹配依赖它
			"name":  name,
			"input": input,
		})
		hasToolUse = true
	}

	id, _ := resp["id"].(string)
	if id == "" {
		id = newAnthropicMessageID()
	} else {
		id = "msg_" + strings.TrimPrefix(id, "chatcmpl-")
	}
	finish, _ := firstChoiceFinishReason(resp)
	usage, _ := resp["usage"].(map[string]any)
	p, c := usageInts(usage)

	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStopReason(finish, hasToolUse),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  p,
			"output_tokens": c,
		},
	}
}

// firstChoiceFinishReason 取 choices[0].finish_reason。
func firstChoiceFinishReason(resp map[string]any) (string, bool) {
	chs, ok := resp["choices"].([]any)
	if !ok || len(chs) == 0 {
		return "", false
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return "", false
	}
	fr, ok := c["finish_reason"].(string)
	return fr, ok
}

// anthropicStopReason 把 chat 的 finish_reason 映射为 Anthropic 的 stop_reason。
//
//	tool_calls → tool_use（有工具调用时优先，与 hasToolUse 一致）
//	stop       → end_turn
//	length     → max_tokens
//	其余        → end_turn（兜底，Claude Code 只认白名单值）
func anthropicStopReason(finish string, hasToolUse bool) string {
	if hasToolUse {
		return "tool_use"
	}
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length", "max_tokens":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	case "content_filter":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// writeAnthropicError 写 Anthropic 形状的错误体。
//
// Claude Code 解析 {"type":"error","error":{"type":...,"message":...}}，
// 与 OpenAI 的 envelope 不同，必须分开写。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    anthropicErrorType(status),
			"message": msg,
		},
	})
}

// anthropicErrorType 把 HTTP 状态码映射为 Anthropic 错误类型字符串。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		if status >= 500 {
			return "api_error"
		}
		return "invalid_request_error"
	}
}
