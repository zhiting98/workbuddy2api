# e2e_stub —— 离线端到端验证

在没有真实账号 / 不联网打到上游的前提下，验证 `/v1/responses` 与 `/v1/messages`
两个协议端点的**完整 SSE 事件序列**（清单 §4 的端到端验证手段）。

## 组成

| 文件 | 作用 |
|---|---|
| `stub_upstream.go` | 假上游：监听 `127.0.0.1:18777`，模拟 CodeBuddy `/v2/chat/completions` 的 SSE 行为 |
| `harness/main.go` | 接线程序：把**真实**的 `server.Handler`（同一套 pool / session / upstream 装配）接到假上游上，监听 `127.0.0.1:17863` |
| `errstub/main.go` | **错误注入**假上游：监听 `127.0.0.1:18778`，按请求内容回真实的 4xx/5xx 与业务码，用于验证错误分类与账号处置 |

`stub_upstream.go` 是**宽容**的（任何请求都回成功流），因此**验不出**错误路径。
要验证冷却 / 熔断 / 禁用等账号处置，用 `errstub`：请求体含下列标记即触发对应错误。

| 请求体含 | errstub 返回 | 期望的网关处置（README「错误分类与账号处置」） |
|---|---|---|
| `E402` | 402 `{"code":1,"msg":"余额不足"}` | 硬冷却至次日 04:00 |
| `E429` | 429 `{"code":6004,"msg":"将在 … 重置"}` | 冷却到重置墙钟（封顶 `soft_rate_max`） |
| `E404` | 404 | 固定 60s 软冷却（不随 soft_rate 退避） |
| `E500` | 500 | 喂 `fails`，达阈值熔断；本次不冷却 |
| `EBLOCK` | 400 `{"code":11128,"msg":"blocked by security policy"}` | **不罚账号**，`passthrough` 下走降级重试 |
| `ESESSION` | 401 `{"code":12153,"msg":"Offline user session not found"}` | **连续 3 次**才禁用（一次多为抖动） |

用法（`E2E_STUB_BASE` 指向 errstub，其余同上）：

```bash
go run ./scripts/e2e_stub/errstub &
E2E_AUTH_DIR=/tmp/wb2a-e2e/auths E2E_STATE_FILE=/tmp/wb2a-e2e/data/state.json \
E2E_STUB_BASE=http://127.0.0.1:18778 E2E_LISTEN=127.0.0.1:17864 \
go run ./scripts/e2e_stub/harness &

# 触发 12153：连发 3 次，第 1/2 次 /status 的 disabled 应为 false，第 3 次才 true
for i in 1 2 3; do
  curl -s http://127.0.0.1:17864/v1/chat/completions -H 'Authorization: Bearer sk-e2e-test' \
    -H 'Content-Type: application/json' \
    -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"ESESSION"}]}' >/dev/null
  curl -s http://127.0.0.1:17864/status -H 'Authorization: Bearer sk-e2e-test' | grep -o '"disabled":[a-z]*'
done
```

假上游按请求体内容分流，便于分别触发各条路径：

| 请求体含 | 假上游行为 |
|---|---|
| `TOOLPROBE` | 回 文本 + 一次 `tool_call`（`arguments` 分两片下发，id 为 `call_stub_1`） |
| `REASONPROBE` | 回 `reasoning_content` + `content` |
| 其他 | 回纯文本 |

## 用法

```bash
# 1) 准备账号目录（假 token 即可，必须匹配 workbuddy*.json 命名）
mkdir -p /tmp/wb2a-e2e/auths /tmp/wb2a-e2e/data
cat > /tmp/wb2a-e2e/auths/workbuddy-stub.json <<'JSON'
{"auth":{"accessToken":"stub-token","refreshToken":"stub","expiresAt":4102444800,"domain":"stub"},
 "account":{"uid":"stub-uid-0001","enterpriseId":"stub-ent","nickname":"stub"}}
JSON

# 2) 起假上游 + harness
go run ./scripts/e2e_stub &
E2E_AUTH_DIR=/tmp/wb2a-e2e/auths \
E2E_STATE_FILE=/tmp/wb2a-e2e/data/state.json \
E2E_STUB_BASE=http://127.0.0.1:18777 \
E2E_LISTEN=127.0.0.1:17863 \
E2E_PROMPT_MODE=passthrough \
go run ./scripts/e2e_stub/harness &

# 3) 验证 Responses 事件序列（预期 response.created → output_text.delta → response.completed）
curl -sN http://127.0.0.1:17863/v1/responses \
  -H "Authorization: Bearer sk-e2e-test" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","input":"hello","stream":true}'

# 4) 验证 Messages 事件序列（预期 message_start → content_block_* → message_delta → message_stop）
#    注意 Claude Code 用 x-api-key 而非 Bearer
curl -sN http://127.0.0.1:17863/v1/messages \
  -H "x-api-key: sk-e2e-test" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}'
```

harness 的 `api_key` 固定为 `sk-e2e-test`；`E2E_PROMPT_MODE` 可切 `custom` 验证
网关自有提示词对两个端点的顶替效果（此时需同时给 `E2E_PROMPT_TEXT`）。

## 注意事项

- 这些文件**不参与生产镜像**（Dockerfile 只 `COPY` 指定文件），也不被 `go build ./...` 之外引用；
- harness 30 秒后自动退出，避免忘记清理进程；
- 假上游会打印每次收到的**出站请求体**，是核对协议映射（`stream:true` 强制、
  工具重包、`parallel_tool_calls:false`、thinking 注入等）最直接的证据。
