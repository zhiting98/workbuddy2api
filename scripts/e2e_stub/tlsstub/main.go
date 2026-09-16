// tls_stub.go 容器验证专用的 TLS 假上游：监听 HTTPS，模拟 CodeBuddy 上游。
//
// 为什么需要它：网关出站写死 `https://copilot.tencent.com`，在容器里验证必须
// 把该域名解析到本机、并在 443 上提供 HTTPS 服务（含自签证书 + 容器内信任）。
//
// 用法：
//
//	TLS_STUB_ADDR=0.0.0.0:443 TLS_STUB_CERT=cert.pem TLS_STUB_KEY=key.pem go run ./scripts/e2e_stub/tlsstub
//
// 行为与 stub_upstream.go 一致（按请求体关键词分流），外加 443 端口与 TLS。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

func main() {
	addr := os.Getenv("TLS_STUB_ADDR")
	if addr == "" {
		addr = "0.0.0.0:443"
	}
	cert := os.Getenv("TLS_STUB_CERT")
	key := os.Getenv("TLS_STUB_KEY")
	if cert == "" || key == "" {
		log.Fatal("TLS_STUB_CERT / TLS_STUB_KEY required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/chat/completions", chatHandler)
	// 网关会在这些路径上打上游；给出 200 空信封即可避免干扰主流程。
	mux.HandleFunc("/console/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"data":{"models":[{"id":"deepseek-v4-pro","name":"pro","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"supportedEfforts":["low","medium","high"]}}],"agents":[{"name":"cli","models":["deepseek-v4-pro"]}]}}`)
	})
	// OAuth 设备授权流（供「添加账号」端到端验证）。
	// POLL_UNTIL_DONE：前 N 次 token 查询返回 pending，之后成功——模拟用户尚未登完。
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"state":"stub-state-1","authUrl":"https://login.example/authorize?state=stub-state-1"}}`)
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := pendingPolls.Add(1)
		until := int64(2)
		if v := os.Getenv("TLS_STUB_PENDING_POLLS"); v != "" {
			if p, err := strconv.ParseInt(v, 10, 64); err == nil {
				until = p
			}
		}
		if n <= until {
			log.Printf("TLS STUB oauth token poll #%d -> pending", n)
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":4001,"msg":"login ing"}`)
			return
		}
		log.Printf("TLS STUB oauth token poll #%d -> done", n)
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"accessToken":"AT-E2E","refreshToken":"RT-E2E","expiresIn":3600,"domain":"www.codebuddy.cn"}}`)
	})
	mux.HandleFunc("/v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"msg":"","data":{"uid":"e2e-uid-0001","enterpriseId":"ent-e2e","nickname":"E2E新账号"}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"code":0,"msg":"ok","data":{}}`)
	})

	log.Printf("TLS stub listening on https://%s", addr)
	log.Fatal(http.ListenAndServeTLS(addr, cert, key, mux))
}

// pendingPolls token 端点被调用次数（原子），用于模拟"前几次仍 pending"。
var pendingPolls atomic.Int64

func chatHandler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	log.Printf("TLS STUB chat request (%d bytes): %s", len(raw), body)

	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)

	frame := func(delta map[string]any, finish any, usage any) {
		ch := map[string]any{"index": 0, "delta": delta}
		if finish != nil {
			ch["finish_reason"] = finish
		}
		m := map[string]any{
			"id": "chatcmpl-tls", "object": "chat.completion.chunk",
			"created": 1753600000, "model": "deepseek-v4-pro",
			"choices": []any{ch},
		}
		if usage != nil {
			m["usage"] = usage
		}
		b, _ := json.Marshal(m)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
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
		frame(map[string]any{"role": "assistant", "content": "hello from TLS stub"}, nil, nil)
		frame(map[string]any{}, "stop",
			map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7})
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}
