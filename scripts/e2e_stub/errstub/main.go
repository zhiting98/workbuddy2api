// errstub 会按请求内容返回真实错误的假上游，用于验证网关的错误分类与账号处置。
// 与 scripts/e2e_stub 的宽容 stub 不同：这个 stub 会真的回 4xx/5xx 与业务错误码。
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

func main() {
	http.HandleFunc("/v2/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		log.Printf("ERRSTUB got %d bytes", len(raw))

		switch {
		case strings.Contains(body, "E402"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(402)
			fmt.Fprint(w, `{"code":1,"msg":"余额不足"}`)
		case strings.Contains(body, "E429"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"code":6004,"msg":"将在 2026-09-14 23:00:00 UTC+8 重置"}`)
		case strings.Contains(body, "E404"):
			w.WriteHeader(404)
			fmt.Fprint(w, `{"code":404,"msg":"not found"}`)
		case strings.Contains(body, "E500"):
			w.WriteHeader(500)
			fmt.Fprint(w, `{"code":500,"msg":"server error"}`)
		case strings.Contains(body, "EBLOCK"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"code":11128,"msg":"blocked by security policy"}`)
		case strings.Contains(body, "ESESSION"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			fmt.Fprint(w, `{"code":12153,"msg":"Offline user session not found"}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
		}
	})
	log.Printf("errstub listening on 127.0.0.1:18778")
	log.Fatal(http.ListenAndServe("127.0.0.1:18778", nil))
}
