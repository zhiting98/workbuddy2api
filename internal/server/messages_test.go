package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// newMessagesHandler 构建带 fake 上游的 handler，并把上游请求体捕获到 cap。
//
// 默认用 passthrough 提示词模式：本组测试断言的是「协议字段映射」，
// 需要看到客户端原始 system 逐字出现在出站体里。
// custom 模式会用网关自有提示词顶替它（见 TestMessagesCustomPromptReplacesSystem）。
func newMessagesHandler(t *testing.T, sse string, cap *captureUpstream) *Handler {
	t.Helper()
	return newMessagesHandlerMode(t, sse, cap, "passthrough")
}

// newMessagesHandlerMode 指定提示词模式构建 handler。
func newMessagesHandlerMode(t *testing.T, sse string, cap *captureUpstream, promptMode string) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sse, true
	})
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

// TestMessagesRequestMapping 覆盖 Anthropic → Chat 的全部字段映射（清单 §3 表格）。
func TestMessagesRequestMapping(t *testing.T) {
	var cap captureUpstream
	h := newMessagesHandler(t, sseOK, &cap)

	body := `{
	  "model":"deepseek-v4-pro",
	  "max_tokens":1024,
	  "system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude"}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"read /tmp/a"}]},
	    {"role":"assistant","content":[
	       {"type":"text","text":"sure"},
	       {"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"/tmp/a"}}
	    ]},
	    {"role":"user","content":[
	       {"type":"tool_result","tool_use_id":"toolu_1","content":"file body"}
	    ]}
	  ],
	  "tools":[{"name":"read_file","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}],
	  "tool_choice":{"type":"auto"},
	  "stop_sequences":["STOP"],
	  "temperature":0.5,
	  "stream":false
	}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	var sent map[string]any
	if err := json.Unmarshal([]byte(cap.last), &sent); err != nil {
		t.Fatalf("captured upstream body not JSON: %v (%s)", err, cap.last)
	}
	if sent["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens=%v want 1024", sent["max_tokens"])
	}
	// stop_sequences → stop
	stops, _ := sent["stop"].([]any)
	if len(stops) != 1 || stops[0] != "STOP" {
		t.Errorf("stop=%v want [STOP]", sent["stop"])
	}
	if sent["temperature"] != 0.5 {
		t.Errorf("temperature=%v want 0.5", sent["temperature"])
	}
	if sent["stream"] != true {
		t.Errorf("upstream stream=%v want true", sent["stream"])
	}
	// tool_choice {type:"auto"} → "auto"
	if sent["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v want auto", sent["tool_choice"])
	}

	// tools: input_schema → function.parameters
	tools, _ := sent["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v want 1", sent["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if fn == nil {
		t.Fatalf("tool must be wrapped: %v", tool)
	}
	if fn["name"] != "read_file" || fn["description"] != "read a file" {
		t.Errorf("tool.function=%v", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("parameters=%v want object (from input_schema)", fn["parameters"])
	}

	// messages: system 置顶 + user + assistant + tool。
	msgs, _ := sent["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%v want 4 (system + user + assistant + tool)", cap.last)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("messages[0].role=%v want system", m0["role"])
	}
	if !strings.Contains(m0["content"].(string), "Claude Code") {
		t.Errorf("system content=%v", m0["content"])
	}
	// user 消息：text block 拍平为 content 字符串。
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "read /tmp/a" {
		t.Fatalf("messages[1]=%v want user/read /tmp/a", m1)
	}
	// assistant 消息：text + tool_calls 共存。
	m2, _ := msgs[2].(map[string]any)
	if m2["role"] != "assistant" || m2["content"] != "sure" {
		t.Fatalf("messages[2]=%v want assistant/sure", m2)
	}
	calls, _ := m2["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v want 1", m2["tool_calls"])
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("tool_call.id=%v want toolu_1", call["id"])
	}
	cfn, _ := call["function"].(map[string]any)
	// input 是对象 → arguments 必须是**字符串**（已序列化）。
	args, ok := cfn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments=%v want string (input object must be serialized)", cfn["arguments"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments is not valid JSON: %q", args)
	}
	if parsed["path"] != "/tmp/a" {
		t.Errorf("arguments round-trip=%v want path=/tmp/a", parsed)
	}
	// tool_result → role:"tool" 独立消息。
	m3, _ := msgs[3].(map[string]any)
	if m3["role"] != "tool" {
		t.Errorf("messages[3].role=%v want tool", m3["role"])
	}
	if m3["tool_call_id"] != "toolu_1" {
		t.Errorf("tool_call_id=%v want toolu_1", m3["tool_call_id"])
	}
	if m3["content"] != "file body" {
		t.Errorf("tool content=%v want file body", m3["content"])
	}
}

// TestMessagesCustomPromptReplacesSystem custom 模式（默认）下网关自有提示词顶替
// 客户端 system——这正是 issue #36 的 11128 指纹脱敏路径，必须对本端点生效。
func TestMessagesCustomPromptReplacesSystem(t *testing.T) {
	var cap captureUpstream
	h := newMessagesHandlerMode(t, sseOK, &cap, "custom")
	body := `{"model":"glm-5.2","max_tokens":16,"system":"You are Claude Code, Anthropic's official CLI for Claude","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 客户端原始 system 指纹不得出现在出站体里。
	if strings.Contains(cap.last, "official CLI for Claude") {
		t.Errorf("custom 模式必须顶替客户端 system 指纹，出站体仍含原文: %s", cap.last)
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

// TestMessagesSystemAsString system 为字符串形式。
func TestMessagesSystemAsString(t *testing.T) {
	var cap captureUpstream
	h := newMessagesHandler(t, sseOK, &cap)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"system":"be brief","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "be brief" {
		t.Errorf("messages[0]=%v want system/be brief", m0)
	}
}

// TestMessagesContentStringPassthrough messages[].content 为 string 时直接透传。
func TestMessagesContentStringPassthrough(t *testing.T) {
	var cap captureUpstream
	h := newMessagesHandler(t, sseOK, &cap)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"plain text"}],"stream":false}`)))
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if m0["content"] != "plain text" {
		t.Errorf("content=%v want plain text", m0["content"])
	}
}

// TestMessagesToolChoiceMapping tool_choice 的 any/auto/none 映射。
//
// none 的语义是"禁止调用工具"：upstream.normalizeToolChoice 会**删除** tool_choice
// 字段（并连带删除 tools）——上游该字段是 string 类型，收到 "none" 会 400 code=11101。
// 故这里断言的是"字段被删除"，而不是留下字符串 "none"。
func TestMessagesToolChoiceMapping(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any // nil = 期望字段不存在
	}{
		{"any → required", `{"type":"any"}`, "required"},
		{"auto → auto", `{"type":"auto"}`, "auto"},
		{"none → 字段删除", `{"type":"none"}`, nil},
		{"tool → function 名", `{"type":"tool","name":"read_file"}`, "read_file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap captureUpstream
			h := newMessagesHandler(t, sseOK, &cap)
			b := `{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"x"}],"tool_choice":` + tc.in + `,"stream":false}`
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(b)))
			var sent map[string]any
			_ = json.Unmarshal([]byte(cap.last), &sent)
			if tc.want == nil {
				if _, present := sent["tool_choice"]; present {
					t.Errorf("tool_choice=%v want field removed", sent["tool_choice"])
				}
				return
			}
			if sent["tool_choice"] != tc.want {
				t.Errorf("tool_choice=%v want %v", sent["tool_choice"], tc.want)
			}
		})
	}
}

// TestMessagesThinkingBlockReplay thinking block 回放为 reasoning_content。
func TestMessagesThinkingBlockReplay(t *testing.T) {
	var cap captureUpstream
	h := newMessagesHandler(t, sseOK, &cap)
	body := `{"model":"deepseek-v4-pro","max_tokens":32,"messages":[
	  {"role":"assistant","content":[{"type":"thinking","thinking":"prior thought"},{"type":"text","text":"answer"}]},
	  {"role":"user","content":"next"}
	],"stream":false}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(cap.last), &sent)
	msgs, _ := sent["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if m0["reasoning_content"] != "prior thought" {
		t.Errorf("reasoning_content=%v want 'prior thought'", m0["reasoning_content"])
	}
}

// ---------------------------------------------------------------------------
// 流式响应：事件序列
// ---------------------------------------------------------------------------

// TestMessagesStreamEventOrder 校验 Claude Code 依赖的事件顺序（清单 §3）。
func TestMessagesStreamEventOrder(t *testing.T) {
	h := newMessagesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}
	got := eventNames(rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("event order:\n got %v\nwant %v\nraw=%s", got, want, rec.Body.String())
	}
	// message_start 必须带 message 骨架。
	start := lastEventData(t, rec.Body.String(), "message_start")
	msg, _ := start["message"].(map[string]any)
	if msg == nil || msg["role"] != "assistant" || msg["type"] != "message" {
		t.Fatalf("message_start.message=%v", start["message"])
	}
	id, _ := msg["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("message.id=%q want msg_ prefix", id)
	}
	// message_delta 的 stop_reason / usage。
	delta := lastEventData(t, rec.Body.String(), "message_delta")
	d, _ := delta["delta"].(map[string]any)
	if d["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v want end_turn (finish_reason=stop)", d["stop_reason"])
	}
	if _, ok := d["stop_sequence"]; !ok {
		t.Errorf("delta must carry stop_sequence (null): %v", d)
	}
	usage, _ := delta["usage"].(map[string]any)
	if usage["output_tokens"] == nil {
		t.Errorf("usage=%v want output_tokens", usage)
	}
}

// TestMessagesStreamToolUseSequence 工具调用流的 content_block 序列与 input_json_delta。
func TestMessagesStreamToolUseSequence(t *testing.T) {
	h := newMessagesHandler(t, sseToolCall, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"read"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	raw := rec.Body.String()
	events := eventNames(raw)
	// 至少两个 content_block（text + tool_use），每个都有 start/delta/stop。
	if n := countStr(events, "content_block_start"); n < 2 {
		t.Errorf("content_block_start count=%d want >=2 (text + tool_use): %v", n, events)
	}
	if !strings.Contains(raw, `"input_json_delta"`) {
		t.Errorf("tool_use arguments must stream as input_json_delta: %s", raw)
	}
	// tool_use block 的 id/name 与 partial_json 拼接保真。
	if !strings.Contains(raw, `"id":"call_abc"`) {
		t.Errorf("tool_use id must pass through verbatim (call_abc): %s", raw)
	}
	if !strings.Contains(raw, `"name":"read_file"`) {
		t.Errorf("tool_use name missing: %s", raw)
	}
	// stop_reason 必须是 tool_use。
	delta := lastEventData(t, raw, "message_delta")
	d, _ := delta["delta"].(map[string]any)
	if d["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", d["stop_reason"])
	}
}

// TestMessagesStreamThinkingOnly thinking 只回 reasoning_content 时仍给出合法块序列。
func TestMessagesStreamThinkingOnly(t *testing.T) {
	h := newMessagesHandler(t, sseReasoning, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"think"}],"stream":true}`)))
	raw := rec.Body.String()
	got := eventNames(raw)
	if got[0] != "message_start" || got[len(got)-1] != "message_stop" {
		t.Errorf("stream must be bracketed by message_start/message_stop: %v", got)
	}
	if !strings.Contains(raw, "answer") {
		t.Errorf("text content must be streamed: %s", raw)
	}
}

// TestMessagesStreamEmitsThinkingBlock 流式必须把上游 reasoning_content 转成
// Anthropic thinking 块（thinking_delta），且**排在正文之前**。
//
// 历史缺陷：messagesStream 只读 delta.content / delta.tool_calls，漏读
// delta.reasoning_content，思维链被静默丢弃——而非流式路径（chatToAnthropic）
// 一直是正确的，兄弟端点 /v1/responses 也正确处理（reasoning item）。
// Claude Code 只用流式，所以这个漏读等于"思维链对它永远不可见"。
func TestMessagesStreamEmitsThinkingBlock(t *testing.T) {
	h := newMessagesHandler(t, sseReasoning, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"think"}],"stream":true}`)))
	raw := rec.Body.String()

	// 1) 必须出现 thinking 块与 thinking_delta，且内容来自上游 reasoning_content。
	if !strings.Contains(raw, `"type":"thinking"`) {
		t.Errorf("缺少 thinking content_block: %s", raw)
	}
	if !strings.Contains(raw, `"type":"thinking_delta"`) {
		t.Errorf("缺少 thinking_delta: %s", raw)
	}
	if !strings.Contains(raw, "think step") {
		t.Errorf("思维链内容未透出: %s", raw)
	}

	// 2) 顺序：thinking 块必须整体早于 text 块。
	thinkStart := strings.Index(raw, `"type":"thinking"`)
	textDelta := strings.Index(raw, `"type":"text_delta"`)
	if thinkStart < 0 || textDelta < 0 {
		t.Fatalf("块类型缺失: think=%d text=%d\n%s", thinkStart, textDelta, raw)
	}
	if thinkStart > textDelta {
		t.Errorf("thinking 块必须排在正文之前（think@%d text@%d）", thinkStart, textDelta)
	}

	// 3) thinking 块必须被 content_block_stop 关闭（index 0），不能只开不关。
	//    用事件序列断言：thinking start → thinking delta → block stop → text start …
	if !strings.Contains(raw, `"index":0,"type":"content_block_stop"`) {
		t.Errorf("thinking 块未关闭: %s", raw)
	}
}

// TestMessagesStreamThinkingBlockIndexOrder thinking 与 text 的 index 必须单调递增
// 且各自成块（Anthropic 要求块内事件连续）。
func TestMessagesStreamThinkingBlockIndexOrder(t *testing.T) {
	h := newMessagesHandler(t, sseReasoning, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"think"}],"stream":true}`)))
	raw := rec.Body.String()

	// 收集 content_block_start 的 index 序列，应为 0,1（thinking 在前，text 在后）。
	var starts []string
	for _, line := range strings.Split(raw, "\n") {
		if strings.Contains(line, `"type":"content_block_start"`) {
			starts = append(starts, line)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("应有 2 个 content_block_start（thinking+text），实际 %d: %v", len(starts), starts)
	}
	if !strings.Contains(starts[0], `"type":"thinking"`) || !strings.Contains(starts[0], `"index":0`) {
		t.Errorf("第一个块应为 index 0 的 thinking: %s", starts[0])
	}
	if !strings.Contains(starts[1], `"type":"text"`) || !strings.Contains(starts[1], `"index":1`) {
		t.Errorf("第二个块应为 index 1 的 text: %s", starts[1])
	}
}

// TestMessagesStreamEmptyUpstreamStillWellFormed 上游空流也必须给出完整骨架
// （Claude Code 挂在半途比拿到空回复更糟）。
func TestMessagesStreamEmptyUpstreamStillWellFormed(t *testing.T) {
	h := newMessagesHandler(t, "data: [DONE]\n\n", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	got := eventNames(rec.Body.String())
	for _, want := range []string{"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop"} {
		if !containsStr(got, want) {
			t.Errorf("empty upstream stream must still emit %s: %v", want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// 非流式响应
// ---------------------------------------------------------------------------

// TestMessagesSyncAggregates 非流式聚合成 Anthropic message 对象。
func TestMessagesSyncAggregates(t *testing.T) {
	h := newMessagesHandler(t, sseOK, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":32,"messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("out=%v want type=message role=assistant", out)
	}
	id, _ := out["id"].(string)
	if !strings.HasPrefix(id, "msg_") {
		t.Errorf("id=%q want msg_ prefix", id)
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v want end_turn", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v want 1 text block", out["content"])
	}
	b0, _ := content[0].(map[string]any)
	if b0["type"] != "text" {
		t.Errorf("content[0].type=%v want text", b0["type"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Errorf("usage=%v want input_tokens/output_tokens", usage)
	}
}

// TestMessagesSyncToolUseInputIsObject tool_use 的 input 必须是**对象**（arguments 反序列化）。
func TestMessagesSyncToolUseInputIsObject(t *testing.T) {
	h := newMessagesHandler(t, sseToolCall, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"deepseek-v4-pro","max_tokens":64,"messages":[{"role":"user","content":"read"}],"stream":false}`)))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rec.Body)
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	var tu map[string]any
	for _, ci := range content {
		b, _ := ci.(map[string]any)
		if b["type"] == "tool_use" {
			tu = b
		}
	}
	if tu == nil {
		t.Fatalf("no tool_use block: %v", content)
	}
	if tu["id"] != "call_abc" {
		t.Errorf("tool_use.id=%v want call_abc", tu["id"])
	}
	if tu["name"] != "read_file" {
		t.Errorf("tool_use.name=%v want read_file", tu["name"])
	}
	// input 必须是对象（不是字符串）。
	input, ok := tu["input"].(map[string]any)
	if !ok {
		t.Fatalf("tool_use.input=%v want object (arguments must be deserialized)", tu["input"])
	}
	if input["path"] != "/tmp/a" {
		t.Errorf("input=%v want path=/tmp/a", input)
	}
}

// TestMessagesSyncStopReasonLength finish_reason=length → max_tokens。
func TestMessagesSyncStopReasonLength(t *testing.T) {
	sse := `data: {"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" + "data: [DONE]\n\n"
	h := newMessagesHandler(t, sse, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":1,"messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason=%v want max_tokens", out["stop_reason"])
	}
}

// TestAnthropicStopReasonMapping 映射表全覆盖。
func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := []struct {
		finish  string
		hasTool bool
		want    string
	}{
		{"stop", false, "end_turn"},
		{"length", false, "max_tokens"},
		{"tool_calls", false, "tool_use"},
		{"stop", true, "tool_use"}, // 有工具调用时优先 tool_use
		{"content_filter", false, "end_turn"},
		{"", false, "end_turn"},
		{"weird_unknown", false, "end_turn"},
	}
	for _, tc := range cases {
		if got := anthropicStopReason(tc.finish, tc.hasTool); got != tc.want {
			t.Errorf("anthropicStopReason(%q,%v)=%q want %q", tc.finish, tc.hasTool, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 错误与鉴权
// ---------------------------------------------------------------------------

// TestMessagesMissingFields400 缺 model/messages → 400 且为 Anthropic 错误形状。
func TestMessagesMissingFields400(t *testing.T) {
	h := newMessagesHandler(t, sseOK, nil)
	for _, body := range []string{
		`{"max_tokens":10,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"glm-5.2","max_tokens":10,"messages":[]}`,
		`{"model":"glm-5.2"`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s code=%d want 400", body, rec.Code)
			continue
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Errorf("error body not JSON: %s", rec.Body)
			continue
		}
		// Anthropic 客户端的错误形状：{"type":"error","error":{...}}
		if out["type"] != "error" {
			t.Errorf("error envelope type=%v want error (%s)", out["type"], rec.Body)
		}
	}
}

// TestAPIKeyAuthAcceptsXAPIKey Claude Code 只发 x-api-key 头，必须能通过鉴权。
func TestAPIKeyAuthAcceptsXAPIKey(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-secret"})

	// x-api-key 正确 → 放行。
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("x-api-key", "sk-secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("x-api-key auth rejected: code=%d body=%s", rec.Code, rec.Body)
	}

	// Bearer 同样放行（OpenAI 系客户端）。
	req2 := httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":false}`))
	req2.Header.Set("Authorization", "Bearer sk-secret")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Errorf("Bearer auth rejected on /v1/responses: code=%d body=%s", rec2.Code, rec2.Body)
	}

	// 错误的 x-api-key → 401。
	req3 := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`))
	req3.Header.Set("x-api-key", "wrong")
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Errorf("wrong x-api-key code=%d want 401", rec3.Code)
	}
}

// countStr 统计 ss 中等于 s 的元素数。
func countStr(ss []string, s string) int {
	n := 0
	for _, v := range ss {
		if v == s {
			n++
		}
	}
	return n
}
