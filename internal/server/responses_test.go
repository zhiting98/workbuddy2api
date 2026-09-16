package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// sseToolCall 一段"文本 + 两个工具调用"的上游 chat SSE（工具参数分片下发，
// 用于验证 arguments 片段拼接与 index 合并）。
const sseToolCall = `data: {"id":"chatcmpl-t1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant","content":"let me check"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-t1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-t1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"/tmp/a\"}"}}]}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-t1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n" +
	"data: [DONE]\n\n"

// sseReasoning 带 reasoning_content 的上游 chat SSE（验证思维链回放路径）。
const sseReasoning = `data: {"id":"chatcmpl-r1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think step"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-r1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{"content":"answer"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-r1","object":"chat.completion.chunk","created":1753600000,"model":"deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}` + "\n\n" +
	"data: [DONE]\n\n"

// captureUpstream 记录最后一次上游请求体，便于断言"出站 chat 形状"是否正确。
type captureUpstream struct {
	last string
}

// newResponsesHandler 构建带 fake 上游的 handler，并把上游请求体捕获到 cap 中。
//
// 默认用 passthrough 提示词模式：本组测试断言的是「协议字段映射」，
// 需要看到客户端原始 system/instructions 逐字出现在出站体里。
// custom 模式会用网关自有提示词顶替它们（见 TestResponsesCustomPromptReplacesInstructions）。
func newResponsesHandler(t *testing.T, sse string, cap *captureUpstream) *Handler {
	t.Helper()
	return newResponsesHandlerMode(t, sse, cap, "passthrough")
}

// newResponsesHandlerMode 指定提示词模式构建 handler。
func newResponsesHandlerMode(t *testing.T, sse string, cap *captureUpstream, promptMode string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sse, true
	})
	// 包装 RoundTripper 以捕获请求体：newFakeUpstream 的 behavior 只给 authz，
	// 故此处用自定义 transport 覆盖。
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if cap != nil && r.Body != nil {
			buf := make([]byte, 0, 4096)
			tmp := make([]byte, 4096)
			for {
				n, err := r.Body.Read(tmp)
				buf = append(buf, tmp[:n]...)
				if err != nil {
					break
				}
			}
			cap.last = string(buf)
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       newNopCloser(sse),
		}, nil
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// PromptText 只在 custom 模式用到；给出可辨识的网关自有提示词，
	// 便于断言"客户端指纹已被顶替"。
	return NewHandler(Config{
		Pool: p, Upstream: up, PromptMode: promptMode,
		PromptText: "GATEWAY SYSTEM PROMPT",
	})
}

// ---------------------------------------------------------------------------
// 请求侧映射
// ---------------------------------------------------------------------------

// TestResponsesRequestMapping 覆盖 Responses → Chat 的全部字段映射（清单 §2 表格）。
func TestResponsesRequestMapping(t *testing.T) {
	var cap captureUpstream
	h := newResponsesHandler(t, sseOK, &cap)

	body := `{
	  "model":"deepseek-v4-pro",
	  "instructions":"You are a coding agent running in the Codex CLI.",
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"read the file"}]},
	    {"type":"function_call","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"/tmp/a\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"file contents"}
	  ],
	  "tools":[{"type":"function","name":"read_file","description":"read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}],
	  "tool_choice":"auto",
	  "temperature":0.3,
	  "top_p":0.9,
	  "max_output_tokens":128,
	  "stream":false,
	  "store":false
	}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.last), &sent); err != nil {
		t.Fatalf("captured upstream body is not JSON: %v (%s)", err, cap.last)
	}

	// max_output_tokens → max_tokens
	if got := sent["max_tokens"]; got != float64(128) {
		t.Errorf("max_tokens=%v want 128 (from max_output_tokens)", got)
	}
	if got := sent["temperature"]; got != 0.3 {
		t.Errorf("temperature=%v want 0.3", got)
	}
	if got := sent["top_p"]; got != 0.9 {
		t.Errorf("top_p=%v want 0.9", got)
	}
	// 出站恒为流式（上游拒绝非流式）。
	if got := sent["stream"]; got != true {
		t.Errorf("upstream stream=%v want true (PrepareBodyOpt forces it)", got)
	}
	// 出站强制 parallel_tool_calls:false（模型目录 supports_parallel_tool_calls:false）。
	if got := sent["parallel_tool_calls"]; got != false {
		t.Errorf("parallel_tool_calls=%v want false", got)
	}

	// tools 必须重包一层：扁平 {type,name,description,parameters} → 嵌套 {type,function:{...}}。
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if fn == nil {
		t.Fatalf("tool must be re-wrapped with nested function object: %v", tool)
	}
	if fn["name"] != "read_file" {
		t.Errorf("tool.function.name=%v want read_file", fn["name"])
	}
	if tool["name"] != nil {
		t.Errorf("flat name must not leak to top level: %v", tool)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("tool.function.parameters=%v want object schema", fn["parameters"])
	}

	// messages 形状：developer(instructions) + user + assistant(tool_calls) + tool。
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d want 4 (developer + user + assistant(tool_calls) + tool): got %s", len(msgs), cap.last)
	}
	// instructions 成首条消息。协议层落成 developer，出站由 upstream.normalizeRoles
	// 归一回 system（上游 role 白名单不含 developer，命中即 400 code=11128）。
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("messages[0].role=%v want system (developer normalized on the wire)", m0["role"])
	}
	if !strings.Contains(m0["content"].(string), "Codex CLI") {
		t.Errorf("messages[0].content=%v want instructions text", m0["content"])
	}
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "read the file" {
		t.Errorf("messages[1]=%v want user/read the file", m1)
	}
	m2, _ := msgs[2].(map[string]any)
	if m2["role"] != "assistant" {
		t.Fatalf("messages[2].role=%v want assistant", m2["role"])
	}
	calls, _ := m2["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls=%v want 1 entry", m2["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	// call_id 必须原样透传（工具循环匹配依赖它）。
	if call["id"] != "call_1" {
		t.Errorf("tool_call.id=%v want call_1 (call_id must pass through verbatim)", call["id"])
	}
	cfn, _ := call["function"].(map[string]any)
	if cfn["name"] != "read_file" || cfn["arguments"] != `{"path":"/tmp/a"}` {
		t.Errorf("tool_call.function=%v want read_file with verbatim arguments", cfn)
	}
}

// TestResponsesFunctionCallOutputBecomesToolMessage function_call_output → role:"tool" 消息。
func TestResponsesFunctionCallOutputBecomesToolMessage(t *testing.T) {
	var cap captureUpstream
	body := `{"model":"deepseek-v4-pro","input":[
	  {"type":"function_call","call_id":"call_9","name":"do","arguments":"{}"},
	  {"type":"function_call_output","call_id":"call_9","output":"done"}
	],"stream":false}`
	h := newResponsesHandler(t, sseOK, &cap)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d want 2: %v", len(msgs), cap.last)
	}
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "tool" {
		t.Errorf("messages[1].role=%v want tool", m1["role"])
	}
	if m1["tool_call_id"] != "call_9" {
		t.Errorf("tool_call_id=%v want call_9", m1["tool_call_id"])
	}
	if m1["content"] != "done" {
		t.Errorf("tool content=%v want done", m1["content"])
	}
}

// TestResponsesCustomPromptReplacesInstructions custom 模式（默认）下网关自有提示词
// 顶替 Codex 的 instructions——issue #36 的 11128 指纹脱敏路径必须覆盖本端点。
func TestResponsesCustomPromptReplacesInstructions(t *testing.T) {
	var cap captureUpstream
	h := newResponsesHandlerMode(t, sseOK, &cap, "custom")
	body := `{"model":"glm-5.2","instructions":"You are a coding agent running in the Codex CLI.","input":"hi","stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(cap.last, "running in the Codex CLI") {
		t.Errorf("custom 模式必须顶替 Codex instructions 指纹，出站体仍含原文: %s", cap.last)
	}
	if !strings.Contains(cap.last, "GATEWAY SYSTEM PROMPT") {
		t.Errorf("custom 模式应注入网关自有提示词: %s", cap.last)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("messages[0].role=%v want system", m0["role"])
	}
}

// TestResponsesReasoningItemIDConsistency reasoning item 的 id 必须在
// added / delta / done 三处**完全一致**：Codex 按 item_id 把增量归集到对应 item，
// 三处不一致会让思维链增量成为孤儿（静默丢弃，不报错）。
func TestResponsesReasoningItemIDConsistency(t *testing.T) {
	h := newResponsesHandler(t, sseReasoning, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"deepseek-v4-pro","input":"think","stream":true}`)))
	raw := rec.Body.String()

	// 从 added 事件取 reasoning item id。
	addedID := reasoningItemIDFromEvent(t, raw, "response.output_item.added")
	// delta 事件的 item_id 必须与之一致。
	deltaID := eventField(t, raw, "response.reasoning_summary_text.delta", "item_id")
	if deltaID != addedID {
		t.Errorf("reasoning delta item_id=%q want %q (added id)", deltaID, addedID)
	}
	// done 事件的 item.id 必须与之一致。
	doneID := reasoningItemIDFromEvent(t, raw, "response.output_item.done")
	if doneID != addedID {
		t.Errorf("reasoning done item.id=%q want %q (added id)", doneID, addedID)
	}
	// 且 reasoning item 的 id 不得与 message item 的 id 相同。
	msgs := lastEventData(t, raw, "response.completed")
	resp, _ := msgs["response"].(map[string]any)
	for _, oi := range resp["output"].([]any) {
		item, _ := oi.(map[string]any)
		if item["type"] == "message" {
			if item["id"] == addedID {
				t.Errorf("message item.id must differ from reasoning item.id (%v)", addedID)
			}
		}
	}
}

// reasoningItemIDFromEvent 取指定事件里 type=="reasoning" 的 item.id。
// 注意同名事件可能出现多次（reasoning 与 message 各一个 added），
// 故遍历全部同名事件而不是只看最后一个。
func reasoningItemIDFromEvent(t *testing.T, raw, event string) string {
	t.Helper()
	for _, data := range allEventData(t, raw, event) {
		item, _ := data["item"].(map[string]any)
		if item == nil || item["type"] != "reasoning" {
			continue
		}
		id, _ := item["id"].(string)
		if id == "" {
			t.Fatalf("event %s reasoning item has empty id", event)
		}
		return id
	}
	t.Fatalf("event %s has no reasoning item in %s", event, raw)
	return ""
}

// eventField 取指定事件 data 里的某个字符串字段。
func eventField(t *testing.T, raw, event, field string) string {
	t.Helper()
	datas := allEventData(t, raw, event)
	if len(datas) == 0 {
		t.Fatalf("event %s not found in %s", event, raw)
	}
	s, _ := datas[len(datas)-1][field].(string)
	if s == "" {
		t.Fatalf("event %s missing field %s: %v", event, field, datas[len(datas)-1])
	}
	return s
}

// allEventData 解析同名事件的**全部** data 负载（按出现顺序）。
func allEventData(t *testing.T, raw, name string) []map[string]any {
	t.Helper()
	lines := strings.Split(raw, "\n")
	var out []map[string]any
	for i, line := range lines {
		if line != "event: "+name || i+1 >= len(lines) {
			continue
		}
		payload := strings.TrimPrefix(lines[i+1], "data: ")
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("event %s payload not JSON: %v (%s)", name, err, payload)
		}
		out = append(out, m)
	}
	return out
}

// TestResponsesInputStringForm input 直接是字符串（非数组）时按单条 user 消息处理。
func TestResponsesInputStringForm(t *testing.T) {
	var cap captureUpstream
	h := newResponsesHandler(t, sseOK, &cap)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi there","stream":false}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v want 1 user message", cap.last)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "hi there" {
		t.Errorf("messages[0]=%v want user/hi there", m0)
	}
}

// TestResponsesReasoningItemReplay reasoning item 回放为 assistant 的 reasoning_content。
func TestResponsesReasoningItemReplay(t *testing.T) {
	var cap captureUpstream
	body := `{"model":"deepseek-v4-pro","input":[
	  {"type":"reasoning","summary":[{"type":"summary_text","text":"previous thoughts"}]},
	  {"type":"message","role":"user","content":"next"}
	],"stream":false}`
	h := newResponsesHandler(t, sseOK, &cap)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v want 2 (reasoning replay + user)", cap.last)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["reasoning_content"] != "previous thoughts" {
		t.Errorf("reasoning_content=%v want 'previous thoughts'", m0["reasoning_content"])
	}
}

// TestResponsesEncryptedOnlyReasoningSkipped 只有 encrypted_content 的 reasoning item 被跳过
// （不产生空 assistant 消息污染对话）。
func TestResponsesEncryptedOnlyReasoningSkipped(t *testing.T) {
	var cap captureUpstream
	body := `{"model":"deepseek-v4-pro","input":[
	  {"type":"reasoning","encrypted_content":"gAAAA..."},
	  {"type":"message","role":"user","content":"hello"}
	],"stream":false}`
	h := newResponsesHandler(t, sseOK, &cap)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages=%v want only the user message", cap.last)
	}
}

// ---------------------------------------------------------------------------
// 流式响应：事件序列
// ---------------------------------------------------------------------------

// TestResponsesStreamEventOrder 校验 Codex 依赖的事件顺序（清单 §2）。
func TestResponsesStreamEventOrder(t *testing.T) {
	h := newResponsesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
	events := eventNames(rec.Body.String())
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v\nraw=%s", events, want, rec.Body.String())
	}
	// response.completed 必须带完整 output + usage。
	completed := lastEventData(t, rec.Body.String(), "response.completed")
	resp, _ := completed["response"].(map[string]any)
	if resp == nil || resp["status"] != "completed" {
		t.Fatalf("completed.response=%v", completed["response"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v want 1 message item", resp["output"])
	}
	usage, _ := resp["usage"].(map[string]any)
	if usage["input_tokens"] == nil || usage["output_tokens"] == nil || usage["total_tokens"] == nil {
		t.Errorf("usage=%v want input_tokens/output_tokens/total_tokens", usage)
	}
	// 字段名必须是 responses 口径（不是 chat 的 prompt_tokens/completion_tokens）。
	if _, bad := usage["prompt_tokens"]; bad {
		t.Errorf("usage must not use chat field names: %v", usage)
	}
}

// TestResponsesStreamToolCallSequence 工具调用流的 item 序列与 arguments 拼接。
func TestResponsesStreamToolCallSequence(t *testing.T) {
	h := newResponsesHandler(t, sseToolCall, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"deepseek-v4-pro","input":"read it","stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	events := eventNames(raw)

	// 必须含 function_call 的 added/delta/done 三段。
	for _, want := range []string{
		"response.output_item.added",
		"response.output_text.delta",
		"response.function_call_arguments.delta",
		"response.output_item.done",
		"response.completed",
	} {
		if !containsStr(events, want) {
			t.Errorf("missing event %s in %v", want, events)
		}
	}

	// arguments 片段必须拼接完整。
	completed := lastEventData(t, raw, "response.completed")
	resp, _ := completed["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	var fc map[string]any
	for _, oi := range output {
		item, _ := oi.(map[string]any)
		if item["type"] == "function_call" {
			fc = item
		}
	}
	if fc == nil {
		t.Fatalf("no function_call item in output: %v", resp["output"])
	}
	if fc["arguments"] != `{"path":"/tmp/a"}` {
		t.Errorf("arguments=%v want joined fragments {\"path\":\"/tmp/a\"}", fc["arguments"])
	}
	if fc["name"] != "read_file" {
		t.Errorf("name=%v want read_file", fc["name"])
	}
	// call_id 必须原样来自上游 tool_call.id（Codex 用它回传 function_call_output）。
	if fc["call_id"] != "call_abc" {
		t.Errorf("call_id=%v want call_abc (verbatim passthrough)", fc["call_id"])
	}
	if fc["status"] != "completed" {
		t.Errorf("status=%v want completed", fc["status"])
	}
}

// TestResponsesStreamReasoningSequence reasoning_content → reasoning item 事件。
func TestResponsesStreamReasoningSequence(t *testing.T) {
	h := newResponsesHandler(t, sseReasoning, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"deepseek-v4-pro","input":"think","stream":true}`)))
	raw := rec.Body.String()
	if !containsStr(eventNames(raw), "response.reasoning_summary_text.delta") {
		t.Fatalf("missing reasoning delta event: %s", raw)
	}
	completed := lastEventData(t, raw, "response.completed")
	resp, _ := completed["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%v want reasoning + message", resp["output"])
	}
	first, _ := output[0].(map[string]any)
	if first["type"] != "reasoning" {
		t.Errorf("output[0].type=%v want reasoning", first["type"])
	}
	second, _ := output[1].(map[string]any)
	if second["type"] != "message" {
		t.Errorf("output[1].type=%v want message", second["type"])
	}
}

// TestResponsesIDsHaveRequiredPrefixes id 前缀规范（resp_ / msg_ / fc_）。
func TestResponsesIDsHaveRequiredPrefixes(t *testing.T) {
	h := newResponsesHandler(t, sseToolCall, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"deepseek-v4-pro","input":"x","stream":true}`)))
	raw := rec.Body.String()
	created := lastEventData(t, raw, "response.created")
	resp, _ := created["response"].(map[string]any)
	id, _ := resp["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Errorf("response.id=%q want resp_ prefix", id)
	}
	completed := lastEventData(t, raw, "response.completed")
	cresp, _ := completed["response"].(map[string]any)
	for _, oi := range cresp["output"].([]any) {
		item, _ := oi.(map[string]any)
		switch item["type"] {
		case "message", "reasoning":
			if s, _ := item["id"].(string); !strings.HasPrefix(s, "msg_") {
				t.Errorf("item.id=%q want msg_ prefix for type %v", s, item["type"])
			}
		case "function_call":
			if s, _ := item["id"].(string); !strings.HasPrefix(s, "fc_") {
				t.Errorf("item.id=%q want fc_ prefix", s)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 非流式响应
// ---------------------------------------------------------------------------

// TestResponsesSyncAggregates 非流式聚合成 Responses 对象（含 usage 字段换名）。
func TestResponsesSyncAggregates(t *testing.T) {
	h := newResponsesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if out["object"] != "response" {
		t.Errorf("object=%v want response", out["object"])
	}
	if out["status"] != "completed" {
		t.Errorf("status=%v want completed", out["status"])
	}
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "resp_") {
		t.Errorf("id=%q want resp_ prefix", id)
	}
	output, _ := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v want 1 item", out["output"])
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("output[0]=%v want assistant message", item)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] == nil {
		t.Errorf("usage=%v want input_tokens", usage)
	}
}

// TestResponsesSyncToolCalls 非流式工具调用转换（call_id 与 arguments 保真）。
func TestResponsesSyncToolCalls(t *testing.T) {
	h := newResponsesHandler(t, sseToolCall, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"deepseek-v4-pro","input":"read","stream":false}`)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	output, _ := out["output"].([]any)
	var types []string
	var fc map[string]any
	for _, oi := range output {
		item, _ := oi.(map[string]any)
		types = append(types, item["type"].(string))
		if item["type"] == "function_call" {
			fc = item
		}
	}
	if fc == nil {
		t.Fatalf("no function_call in output %v", types)
	}
	if fc["arguments"] != `{"path":"/tmp/a"}` {
		t.Errorf("arguments=%v want joined fragments", fc["arguments"])
	}
	if fc["call_id"] != "call_abc" {
		t.Errorf("call_id=%v want call_abc", fc["call_id"])
	}
}

// ---------------------------------------------------------------------------
// 错误与边界
// ---------------------------------------------------------------------------

// TestResponsesMissingModel400 缺 model 直接 400，不打上游。
func TestResponsesMissingModel400(t *testing.T) {
	h := newResponsesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"input":"hi"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (body=%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model") {
		t.Errorf("error should name the missing field: %s", rec.Body)
	}
}

// TestResponsesEmptyInput400 空 input 直接 400（含只有 encrypted reasoning 的情况）。
func TestResponsesEmptyInput400(t *testing.T) {
	h := newResponsesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (body=%s)", rec.Code, rec.Body)
	}
}

// TestResponsesInvalidJSON400 畸形 JSON → 400。
func TestResponsesInvalidJSON400(t *testing.T) {
	h := newResponsesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (body=%s)", rec.Code, rec.Body)
	}
}

// TestResponsesOversizedBody413 超限复用同一上限语义（413，不打上游）。
func TestResponsesOversizedBody413(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 100})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"`+strings.Repeat("a", 200)+`"}`)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 (body=%s)", rec.Code, rec.Body)
	}
}

// TestResponsesUpstreamErrorPropagates 上游 4xx 错误按 OpenAI envelope 回给客户端。
func TestResponsesUpstreamErrorPropagates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"error":{"message":"bad params"}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`)))
	if rec.Code == 200 {
		t.Fatalf("want non-200, got 200 body=%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("body should be an error envelope: %s", rec.Body)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// eventNames 按出现顺序提取 SSE 的 event: 名（忽略 data-only 帧）。
func eventNames(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if name, ok := strings.CutPrefix(line, "event: "); ok {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

// lastEventData 解析指定事件的最后一个 data 负载为 map。
func lastEventData(t *testing.T, raw, name string) map[string]any {
	t.Helper()
	lines := strings.Split(raw, "\n")
	var payload string
	for i, line := range lines {
		if line == "event: "+name && i+1 < len(lines) {
			payload = strings.TrimPrefix(lines[i+1], "data: ")
		}
	}
	if payload == "" {
		t.Fatalf("event %s not found in %s", name, raw)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("event %s payload not JSON: %v (%s)", name, err, payload)
	}
	return out
}

// containsStr 报告 ss 是否含 s。
func containsStr(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// newNopCloser 把字符串包成 io.ReadCloser（测试响应体）。
func newNopCloser(s string) nopCloser { return nopCloser{strings.NewReader(s)} }

type nopCloser struct{ *strings.Reader }

func (nopCloser) Close() error { return nil }
