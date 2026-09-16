// stub_upstream.go 端到端验证用的假上游：模拟 CodeBuddy /v2/chat/completions。
//
// 行为：按请求体里的 messages 内容决定回什么——
//   - 含 "TOOLPROBE" → 回文本 + 一次 tool_call（验证工具循环与 call_id 透传）
//   - 含 "REASONPROBE" → 回 reasoning_content + content（验证思维链回放）
//   - 其他 → 回纯文本（验证基础流）
//
// 始终以 SSE 形式返回（含末尾 usage 帧与 [DONE]），与真实上游一致。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

func main() {
	http.HandleFunc("/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		log.Printf("STUB got chat request (%d bytes), stream-forced=%v", len(raw), strings.Contains(body, `"stream":true`))
		log.Printf("STUB outbound body: %s", body)

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		write := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		frame := func(delta map[string]any, finish any, usage any) {
			ch := map[string]any{"index": 0, "delta": delta}
			if finish != nil {
				ch["finish_reason"] = finish
			}
			m := map[string]any{
				"id": "chatcmpl-stub", "object": "chat.completion.chunk",
				"created": 1753600000, "model": "deepseek-v4-pro",
				"choices": []any{ch},
			}
			if usage != nil {
				m["usage"] = usage
			}
			write(m)
		}

		switch {
		case strings.Contains(body, "TOOLPROBE"):
			frame(map[string]any{"role": "assistant", "content": "checking the file"}, nil, nil)
			frame(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_stub_1", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"pa`},
			}}}, nil, nil)
			frame(map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "function": map[string]any{"arguments": `th":"/tmp/x"}`},
			}}}, nil, nil)
			frame(map[string]any{}, "tool_calls",
				map[string]any{"prompt_tokens": 21, "completion_tokens": 9, "total_tokens": 30})
		case strings.Contains(body, "REASONPROBE"):
			frame(map[string]any{"role": "assistant", "reasoning_content": "weighing options"}, nil, nil)
			frame(map[string]any{"content": "the answer"}, nil, nil)
			frame(map[string]any{}, "stop",
				map[string]any{"prompt_tokens": 5, "completion_tokens": 6, "total_tokens": 11})
		default:
			frame(map[string]any{"role": "assistant", "content": "hello from stub"}, nil, nil)
			frame(map[string]any{}, "stop",
				map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})

	// 模型列表接口（供网关动态模型拉取，失败也不影响本次验证）。
	http.HandleFunc("/console/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"models":[{"id":"deepseek-v4-pro","name":"pro","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"supportedEfforts":["low","medium","high"]}}],"agents":[{"name":"cli","models":["deepseek-v4-pro"]}]}}`)
	})

	// 监听地址可用 E2E_STUB_LISTEN 覆盖：容器内跑网关时需绑 0.0.0.0
	// （默认 127.0.0.1 只对本机可见，容器无法经 host.docker.internal 访问）。
	addr := os.Getenv("E2E_STUB_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:18777"
	}
	log.Printf("stub upstream listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
