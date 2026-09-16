// responses.go 实现 OpenAI Responses API（POST /v1/responses）→ chat 协议转换。
//
// 背景：Codex v0.154 起只支持 /v1/responses，直连网关的 /v1/chat/completions 会 404。
// 本文件只做「协议转换层」：请求 Responses → chat（喂给既有上游管线），
// 响应用 chat SSE → Responses SSE 事件序列。
//
// 账号池 / 会话粘性 / 轮转 / 冷却 / 指纹降级全部复用 relay.go 的 relay()。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 请求侧：Responses → Chat
// ---------------------------------------------------------------------------

// responsesRequest Responses API 请求体（只声明网关关心的字段）。
type responsesRequest struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions"`
	// Input 是 string 或 item 数组；用 json.RawMessage 手动分流（两种形状差异极大）。
	Input json.RawMessage `json:"input"`
	// Tools 为**扁平**形状 [{type:"function", name, description, parameters}]，
	// 与 chat 的嵌套形状 [{type:"function", function:{...}}] 不同，必须重包一层。
	Tools       []responsesTool `json:"tools"`
	ToolChoice  json.RawMessage `json:"tool_choice"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	// MaxOutputTokens → chat 的 max_tokens。
	MaxOutputTokens *int `json:"max_output_tokens"`
	Stream          bool `json:"stream"`
	// Store / PreviousResponseID 无状态实现下忽略：
	// disable_response_storage=true 时 Codex 恰好发 store:false，与本实现天然匹配。
	Store              *bool  `json:"store"`
	PreviousResponseID string `json:"previous_response_id"`
	// Reasoning 承载 effort 等；Codex 会带 reasoning:{effort:"..."}。
	Reasoning json.RawMessage `json:"reasoning"`
	// ParallelToolCalls 模型目录声明 supports_parallel_tool_calls:false，
	// 出站强制 false（见 buildResponsesChat 注释）。
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Metadata          json.RawMessage `json:"metadata"`
	// 其余未知字段一律忽略（向前兼容）。
}

// responsesTool Responses 的扁平工具定义。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Strict 为 OpenAI 结构化输出开关，chat 侧无对应字段，忽略。
	Strict *bool `json:"strict"`
}

// responsesItem Responses input 数组里的单个 item。
// 用宽松结构承接全部 item 类型，按 type 分流。
type responsesItem struct {
	Type string `json:"type"`
	Role string `json:"role"`
	// Content 为 string 或 [{type:"input_text"/"output_text", text}]。
	Content json.RawMessage `json:"content"`
	// function_call 字段
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	// function_call_output 字段
	Output json.RawMessage `json:"output"`
	// reasoning item 字段：加密内容/摘要/明文 reasoning。
	Summary          json.RawMessage `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
	// ID 兼容部分客户端回传 item id。
	ID string `json:"id"`
}

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	writeErr := func(status int, msg string) {
		writeResponsesError(w, status, msg)
	}
	body, ok := h.readBody(w, r, writeErr)
	if !ok {
		return
	}
	req, err := parseResponsesRequest(body)
	if err != nil {
		writeErr(http.StatusBadRequest, err.Error())
		return
	}
	chatBody, err := buildResponsesChat(req)
	if err != nil {
		writeErr(http.StatusBadRequest, err.Error())
		return
	}

	// 会话粘性 / 统计用的 model 与实际发上游一致。
	model := req.Model
	h.relay(w, r, chatRequest{Body: chatBody, Model: model, Stream: req.Stream},
		func(rc io.Reader, acct *auth.Auth) (time.Duration, int) {
			if req.Stream {
				return h.responsesStream(w, rc, model, acct)
			}
			return h.responsesSync(w, rc, model, acct)
		},
		writeErr)
}

// parseResponsesRequest 解析 Responses 请求体。
func parseResponsesRequest(body []byte) (*responsesRequest, error) {
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if req.Model == "" {
		return nil, fmt.Errorf("missing required field: model")
	}
	return &req, nil
}

// buildResponsesChat 把 Responses 请求转换为完整 chat 请求 JSON。
//
// 映射规则（清单 §2）：
//   - instructions          → 首条 {role:"developer", content:instructions}
//     （developer 由 upstream.normalizeRoles 归一回 system，语义不变；
//     保留 developer 是为了让 prompt.Rewrite 的 system/developer 过滤同样命中，
//     从而 custom 模式下网关自有提示词能正常顶替——issue #36 指纹路径。）
//   - input string          → 单条 user 消息
//   - input message item    → {role, content:<拼接 text>}
//   - function_call item    → assistant.tool_calls[{id:call_id, type:"function", function:{name, arguments}}]
//   - function_call_output  → {role:"tool", tool_call_id:call_id, content:output}
//   - reasoning item        → assistant 消息的 reasoning_content（复用 thinking.go 出站回填逻辑）
//   - tools 扁平 → 嵌套
//   - max_output_tokens     → max_tokens
//   - store/previous_response_id → 忽略（无状态）
func buildResponsesChat(req *responsesRequest) ([]byte, error) {
	msgs, err := responsesInputToMessages(req.Input)
	if err != nil {
		return nil, err
	}
	// instructions 置顶为 developer 消息（OpenAI 新规范里 developer 即 system 别名）。
	if s := strings.TrimSpace(req.Instructions); s != "" {
		msgs = append([]any{map[string]any{"role": "developer", "content": s}}, msgs...)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input is empty: nothing to send upstream")
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	// 上游恒为流式（PrepareBodyOpt 强制 stream:true）；这里按客户端意图声明，
	// 保持请求体语义自洽（网关本地再做聚合或事件转换）。
	out["stream"] = req.Stream

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			// 只处理 function 类型；web_search 等内置工具上游不支持，跳过。
			if t.Type != "" && t.Type != "function" {
				continue
			}
			fn := map[string]any{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				var params any
				if err := json.Unmarshal(t.Parameters, &params); err == nil {
					fn["parameters"] = params
				}
			} else {
				// chat 工具定义要求 parameters 存在，缺省给空对象 schema。
				fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if len(req.ToolChoice) > 0 {
		if tc := convertResponsesToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	// reasoning.effort → reasoning_effort（复用既有 effort 降级管线）。
	if eff := responsesReasoningEffort(req.Reasoning); eff != "" {
		out["reasoning_effort"] = eff
	}
	// 模型目录声明 supports_parallel_tool_calls:false：出站强制 false。
	// 与 normalizeToolChoice 不冲突——后者只处理 tool_choice 字段（且会把对象形式
	// 归一为字符串），不触碰 parallel_tool_calls；两者作用字段互不相交。
	out["parallel_tool_calls"] = false
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return raw, nil
}

// responsesReasoningEffort 从 reasoning 对象取 effort（支持 reasoning:{effort:"high"}）。
func responsesReasoningEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	return strings.TrimSpace(r.Effort)
}

// convertResponsesToolChoice 把 Responses 的 tool_choice 转成 chat 形状。
//
// Responses/chat 二者的 tool_choice 形状本就一致（字符串或 {type:"function",function:{name}}），
// 但 Responses 也允许扁平 {type:"function", name:"x"}，故在此归一，剩余交给
// upstream.normalizeToolChoice 做上游要求的字符串化。
func convertResponsesToolChoice(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	typ, _ := obj["type"].(string)
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "function":
		// 嵌套形式原样；扁平形式补一层 function。
		if fn, ok := obj["function"].(map[string]any); ok && fn != nil {
			return obj
		}
		name, _ := obj["name"].(string)
		if strings.TrimSpace(name) == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	case "auto", "required", "none":
		return strings.ToLower(strings.TrimSpace(typ))
	default:
		return nil
	}
}

// responsesInputToMessages 把 Responses 的 input 数组转换为 chat messages。
//
// 关键点：**连续的同角色消息不合并**（上游能接受多条同角色消息，合并会引入新变量），
// 但 function_call 必须与它所属的 assistant 消息合并成一条——chat 规范要求
// assistant 消息的 content 为 null 且 tool_calls 挂在同一条消息上。
func responsesInputToMessages(raw json.RawMessage) ([]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// 形状一：input 直接是字符串。
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": "user", "content": s}}, nil
	}
	var items []responsesItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("invalid input: expected string or item array")
	}

	msgs := make([]any, 0, len(items))
	for _, it := range items {
		switch it.Type {
		case "function_call":
			// 连续的 function_call 合并进一条 assistant 消息的 tool_calls。
			call := map[string]any{
				"id":   it.CallID,
				"type": "function",
				"function": map[string]any{
					"name":      it.Name,
					"arguments": it.Arguments,
				},
			}
			// 回看上一条：若已是带 tool_calls 的 assistant 消息则追加，
			// 避免产生多条 assistant 消息（部分上游对连续 assistant 容忍度低）。
			if n := len(msgs); n > 0 {
				if last, ok := msgs[n-1].(map[string]any); ok {
					if last["role"] == "assistant" {
						if tcs, ok := last["tool_calls"].([]any); ok {
							last["tool_calls"] = append(tcs, call)
							continue
						}
					}
				}
			}
			msgs = append(msgs, map[string]any{
				"role":       "assistant",
				"content":    nil,
				"tool_calls": []any{call},
			})
		case "function_call_output":
			out := rawText(it.Output)
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      out,
			})
		case "reasoning":
			// Codex 会回传 reasoning item。DeepSeek 要求多轮请求里 assistant 消息
			// 带 reasoning_content（requiresReasoningContentOnAssistantMessages）。
			// 这里把 reasoning 的明文/摘要落成 assistant 消息的 reasoning_content，
			// 出站由 upstream.backfillReasoningContent 统一补齐其余 assistant 消息。
			text := responsesReasoningText(it)
			if text == "" {
				// 只有 encrypted_content（无明文）时无法还原，跳过该 item
				// （不产生空 assistant 消息，避免污染对话）。
				continue
			}
			msgs = append(msgs, map[string]any{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": text,
			})
		case "message", "":
			// type 缺省按 message 处理（部分客户端省略该字段）。
			role := it.Role
			if role == "" {
				role = "user"
			}
			text := responsesContentText(it.Content)
			msgs = append(msgs, map[string]any{
				"role":    role,
				"content": text,
			})
		default:
			// 未知 item 类型（如 local_shell_call / web_search_call）：
			// 上游无对应能力，跳过而不是让整单失败。
			continue
		}
	}
	return msgs, nil
}

// responsesContentText 把 Responses 的 content 拍平成纯文本。
// 支持 string 与 [{type:"input_text"/"output_text", text}] 两种形状；
// 非文本 part（input_image 等）忽略——上游 chat 侧无视觉输入能力。
func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text", "summary_text", "":
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// responsesReasoningText 提取 reasoning item 的可回放文本。
// 优先明文 content（非标准但部分客户端会带），其次 summary[].text。
func responsesReasoningText(it responsesItem) string {
	if s := responsesContentText(it.Content); s != "" {
		return s
	}
	if len(it.Summary) == 0 {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(it.Summary, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// rawText 把 function_call_output.output 拍平成字符串。
// output 可以是 string，也可以是 [{type:"output_text", text}] 数组。
func rawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return responsesContentText(raw)
}

// ---------------------------------------------------------------------------
// 响应侧：Chat SSE → Responses SSE
// ---------------------------------------------------------------------------

// respIDSeq 进程级 response id 序号（保证同进程内 id 唯一）。
var respIDSeq counter64

// newResponseID 生成 `resp_...` 形式的 id（Codex 依赖此前缀）。
func newResponseID() string {
	return fmt.Sprintf("resp_%d%06d", time.Now().UnixNano(), respIDSeq.Add(1)%1000000)
}

// newFunctionCallID 生成 `fc_...` 形式的 function_call item id。
// 注意：item.id 是响应内部标识，工具循环的断链风险在 **call_id**（必须与请求原样对应），
// 两者不可混用——call_id 一律透传客户端的值，绝不重造。
func newFunctionCallID() string {
	return fmt.Sprintf("fc_%d%06d", time.Now().UnixNano(), respIDSeq.Add(1)%1000000)
}

// newMessageItemID 生成 `msg_...` 形式的 message item id。
func newMessageItemID() string {
	return fmt.Sprintf("msg_%d%06d", time.Now().UnixNano(), respIDSeq.Add(1)%1000000)
}

// responsesStream 消费上游 chat SSE，边转边发 Responses 事件序列。
//
// 事件顺序（Codex 严格依赖，清单 §2）：
//
//	response.created
//	response.in_progress              （可选）
//	── 每个输出项 ──
//	response.output_item.added
//	response.output_text.delta / response.function_call_arguments.delta
//	response.reasoning_summary_text.delta
//	response.output_item.done
//	── 结束 ──
//	response.completed                （携带完整 output 数组 + usage）
//
// 输出项顺序固定为 reasoning → message → function_call：上游 chat SSE 的 delta 里
// reasoning_content 与 content 可能交错到达，但 Responses 规范要求 output 数组自洽
// （reasoning 在最前、工具调用在文本之后），故本地按类型分桶累积、在结束时定序提交。
func (h *Handler) responsesStream(w http.ResponseWriter, rc io.Reader, model string, acct *auth.Auth) (time.Duration, int) {
	sw := newSSEWriter(w)
	id := newResponseID()
	start := time.Now()

	// response.created：先发骨架（status=in_progress），Codex 据此建立 response 上下文。
	skeleton := func(status string) map[string]any {
		return map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": start.Unix(),
			"status":     status,
			"model":      model,
			"output":     []any{},
			"usage":      nil,
		}
	}
	if err := sw.event("response.created", map[string]any{
		"type":     "response.created",
		"response": skeleton("in_progress"),
	}); err != nil {
		return 0, -1
	}
	_ = sw.event("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": skeleton("in_progress"),
	})

	// 输出项累积器：文本、思维链、工具调用各自成 item。
	var (
		textBuf   strings.Builder
		reasonBuf strings.Builder
		toolCalls = map[int]map[string]any{}
		toolOrder []int

		messageStarted bool
		reasonStarted  bool
		outputIndex    int

		usage       map[string]any
		ttfb        time.Duration
		seenFrame   bool
		msgItemID   = newMessageItemID()
		rsnItemID   = newMessageItemID()
		msgOutputAt = -1
		rsnOutputAt = -1
	)

	// openReasoning 惰性开 reasoning item（只发一次 added）。
	// rsnItemID 预先分配并贯穿 added/delta/done 三处：三者的 id 必须一致，
	// 否则客户端无法把 delta 关联到对应 item（Codex 按 item_id 归集增量）。
	openReasoning := func() error {
		if reasonStarted {
			return nil
		}
		reasonStarted = true
		rsnOutputAt = outputIndex
		outputIndex++
		return sw.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": rsnOutputAt,
			"item": map[string]any{
				"id":      rsnItemID,
				"type":    "reasoning",
				"summary": []any{},
			},
		})
	}
	// openMessage 惰性开 message item（只发一次 added）。
	openMessage := func() error {
		if messageStarted {
			return nil
		}
		messageStarted = true
		msgOutputAt = outputIndex
		outputIndex++
		return sw.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": msgOutputAt,
			"item": map[string]any{
				"id":      msgItemID,
				"type":    "message",
				"status":  "in_progress",
				"role":    "assistant",
				"content": []any{},
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
						// Responses 的终态是 status:"completed"（不携带 finish_reason），
						// 上游的 finish_reason 在此无需转换，故不读取。
						if d, ok := c["delta"].(map[string]any); ok {
							// 思维链增量 → reasoning item
							if v, ok := d["reasoning_content"].(string); ok && v != "" {
								if e := openReasoning(); e != nil {
									readErr = e
									break readLoop
								}
								reasonBuf.WriteString(v)
								if e := sw.event("response.reasoning_summary_text.delta", map[string]any{
									"type":          "response.reasoning_summary_text.delta",
									"item_id":       rsnItemID,
									"output_index":  rsnOutputAt,
									"summary_index": 0,
									"delta":         v,
								}); e != nil {
									readErr = e
									break readLoop
								}
							}
							// 正文增量 → message item
							if v, ok := d["content"].(string); ok && v != "" {
								if e := openMessage(); e != nil {
									readErr = e
									break readLoop
								}
								textBuf.WriteString(v)
								if e := sw.event("response.output_text.delta", map[string]any{
									"type":          "response.output_text.delta",
									"item_id":       msgItemID,
									"output_index":  msgOutputAt,
									"content_index": 0,
									"delta":         v,
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

	// ── 收尾：按 reasoning → message → function_call 顺序提交 output_item.done ──
	if reasonStarted {
		if e := sw.event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": rsnOutputAt,
			"item": map[string]any{
				"id":      rsnItemID,
				"type":    "reasoning",
				"summary": []any{map[string]any{"type": "summary_text", "text": reasonBuf.String()}},
			},
		}); e != nil {
			return ttfb, -1
		}
	}
	if messageStarted {
		if e := sw.event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": msgOutputAt,
			"item": map[string]any{
				"id":     msgItemID,
				"type":   "message",
				"status": "completed",
				"role":   "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": textBuf.String(), "annotations": []any{}},
				},
			},
		}); e != nil {
			return ttfb, -1
		}
	}

	// 工具调用项（按 index 升序，与上游声明顺序一致）。
	upstream.SortInts(toolOrder)
	output := make([]any, 0, len(toolOrder)+2)
	if reasonStarted {
		output = append(output, map[string]any{
			"id":      rsnItemID,
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": reasonBuf.String()}},
		})
	}
	if messageStarted {
		output = append(output, map[string]any{
			"id":     msgItemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": textBuf.String(), "annotations": []any{}},
			},
		})
	}
	for _, idx := range toolOrder {
		tc := toolCalls[idx]
		itemID := newFunctionCallID()
		callID, _ := tc["id"].(string)
		name, args := "", ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		callIndex := outputIndex
		outputIndex++
		// function_call item：added → arguments.delta（整段一次下发）→ done。
		if e := sw.event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": callIndex,
			"item": map[string]any{
				"id":        itemID,
				"type":      "function_call",
				"status":    "in_progress",
				"call_id":   callID,
				"name":      name,
				"arguments": "",
			},
		}); e != nil {
			return ttfb, -1
		}
		if args != "" {
			if e := sw.event("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      itemID,
				"output_index": callIndex,
				"delta":        args,
			}); e != nil {
				return ttfb, -1
			}
		}
		// 注意：**call_id 原样透传**（来自上游 chat 的 tool_call.id），
		// 绝不能用本地生成的 itemID 顶替，否则 Codex 工具循环断链。
		item := map[string]any{
			"id":        itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   callID,
			"name":      name,
			"arguments": args,
		}
		if e := sw.event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": callIndex,
			"item":         item,
		}); e != nil {
			return ttfb, -1
		}
		output = append(output, item)
	}

	// response.completed：携带完整 output 数组 + usage（字段名与 chat 不同）。
	completed := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": start.Unix(),
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage":      responsesUsage(usage),
	}
	if len(toolOrder) > 0 {
		completed["tools"] = []any{}
		completed["parallel_tool_calls"] = false
	}
	_ = sw.event("response.completed", map[string]any{
		"type":     "response.completed",
		"response": completed,
	})

	p, c := usageInts(usage)
	if h.cfg.Stats != nil {
		h.cfg.Stats.Record(model, acct.UID, p, c, p+c, http.StatusOK)
	}
	if !seenFrame {
		// 上游 0 有效帧：已写出 created/in_progress，只能以 completed 收尾并记录 0 token。
		// （客户端可据此判定"空回复"，不会挂在半途。）
	}
	if c > 0 {
		return ttfb, c
	}
	return ttfb, 0
}

// responsesSync 聚合上游 chat SSE 为完整 Responses 对象（非流式）。
func (h *Handler) responsesSync(w http.ResponseWriter, rc io.Reader, model string, acct *auth.Auth) (time.Duration, int) {
	resp, err := upstream.Aggregate(rc)
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, err.Error())
		return 0, -1
	}
	out := chatToResponses(resp, model)
	writeJSON(w, http.StatusOK, out)
	if h.cfg.Stats != nil {
		if p, c, t, ok := usageTriple(resp); ok {
			h.cfg.Stats.Record(model, acct.UID, p, c, t, http.StatusOK)
		}
	}
	return 0, completionTokens(resp)
}

// chatToResponses 把聚合后的 chat.completion 转成 Responses 对象。
func chatToResponses(resp map[string]any, model string) map[string]any {
	msg := firstChoiceMessage(resp)
	var output []any
	if r, ok := msg["reasoning_content"].(string); ok && r != "" {
		output = append(output, map[string]any{
			"id":      newMessageItemID(),
			"type":    "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}
	content, _ := msg["content"].(string)
	if content != "" {
		output = append(output, map[string]any{
			"id":     newMessageItemID(),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{
				map[string]any{"type": "output_text", "text": content, "annotations": []any{}},
			},
		})
	}
	if tcs := toolCallsOf(msg); len(tcs) > 0 {
		for _, tc := range tcs {
			callID, name, args := splitToolCall(tc)
			output = append(output, map[string]any{
				"id":        newFunctionCallID(),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   callID, // 原样透传，工具循环依赖它匹配
				"name":      name,
				"arguments": args,
			})
		}
	}
	if output == nil {
		output = []any{}
	}
	id, _ := resp["id"].(string)
	if id == "" {
		id = newResponseID()
	}
	usage, _ := resp["usage"].(map[string]any)
	return map[string]any{
		"id":         "resp_" + strings.TrimPrefix(id, "chatcmpl-"),
		"object":     "response",
		"created_at": createdOf(resp),
		"status":     "completed",
		"model":      model,
		"output":     output,
		"usage":      responsesUsage(usage),
	}
}

// firstChoiceMessage 取 choices[0].message（缺失返回空 map）。
func firstChoiceMessage(resp map[string]any) map[string]any {
	chs, ok := resp["choices"].([]any)
	if !ok || len(chs) == 0 {
		return map[string]any{}
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return map[string]any{}
	}
	m, _ := c["message"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

// toolCallsOf 从 chat message 取出 tool_calls，统一成 []map[string]any。
//
// 必须容错两种形状：upstream.Aggregate 产出的是 `[]map[string]any`（Go 具体切片），
// 而直接来自 JSON 反序列化的同类数据是 `[]any`。只断言 `[]any` 会静默丢掉全部
// 工具调用——非流式工具循环会因此整条断链，且不报任何错。
func toolCallsOf(msg map[string]any) []map[string]any {
	switch v := msg["tool_calls"].(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, ci := range v {
			if c, ok := ci.(map[string]any); ok {
				out = append(out, c)
			}
		}
		return out
	case []map[string]any:
		return v
	default:
		return nil
	}
}

// splitToolCall 从单个 chat tool_call 取出 id / name / arguments（缺失为空串）。
// call_id 语义：**必须原样透传**给客户端，绝不重造，否则客户端回传的
// tool_result / function_call_output 无法与请求匹配，工具循环断链。
func splitToolCall(tc map[string]any) (callID, name, args string) {
	callID, _ = tc["id"].(string)
	if fn, ok := tc["function"].(map[string]any); ok {
		name, _ = fn["name"].(string)
		args, _ = fn["arguments"].(string)
	}
	return callID, name, args
}

// createdOf 取响应 created（缺失用当前时间）。
func createdOf(resp map[string]any) int64 {
	switch v := resp["created"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return time.Now().Unix()
	}
}

// responsesUsage 把 chat 的 usage 字段名换成 Responses 的字段名。
// chat: prompt_tokens / completion_tokens / total_tokens
// Responses: input_tokens / output_tokens / total_tokens
func responsesUsage(usage map[string]any) map[string]any {
	in, out := usageInts(usage)
	u := map[string]any{
		"input_tokens":  in,
		"output_tokens": out,
		"total_tokens":  in + out,
	}
	if usage != nil {
		if d, ok := usage["prompt_tokens_details"]; ok && d != nil {
			u["input_tokens_details"] = d
		}
		if d, ok := usage["completion_tokens_details"]; ok && d != nil {
			u["output_tokens_details"] = d
		}
	}
	return u
}

// usageInts 从 chat usage 取 prompt/completion token 数（缺失为 0）。
func usageInts(usage map[string]any) (int, int) {
	if usage == nil {
		return 0, 0
	}
	get := func(k string) int {
		if v, ok := usage[k].(float64); ok {
			return int(v)
		}
		return 0
	}
	return get("prompt_tokens"), get("completion_tokens")
}

// writeResponsesError 写 Responses 形状的错误体（OpenAI 错误 envelope，Codex 可解析）。
func writeResponsesError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    strconv.Itoa(status),
		},
	})
}
