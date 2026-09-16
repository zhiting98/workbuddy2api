// keys_api.go API Key 管理的 HTTP 端点：列出 / 生成 / 删除 / 启用吊销。
//
// 路由（均在看板 /api/* 下，走 Basic Auth，与账号管理端点同口径）：
//
//	GET    /keys                列出全部 key（含 config 源，标记来源与启用态）
//	POST   /keys                生成新 key，body {"name":"可选备注"}
//	DELETE /keys/{key}          删除**看板生成**的 key（config 源的不可删，应吊销）
//	POST   /keys/{key}/disable  吊销（立即失效，两种来源通用）
//	POST   /keys/{key}/enable   恢复被吊销的 key
//
// 为什么这些端点不需要 api_key：它们服务于看板 UI，而看板已由 Basic Auth 保护；
// 若强制 api_key，页面就得内嵌密钥（本项目明确避免这一点）。
// 注意与"用 key 调 /v1/*"的区别：那是客户端鉴权，这是管理操作。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// keysList 列出全部 key（含明文，供前端"复制"用）。
//
// 为什么返回明文：看板需要「复制」按钮，否则用户只能看到掩码却无法取用。
// 这与既有安全模型一致——同为本项目提供的「导出凭证」端点也返回明文 token，
// 而 /api/* 本身已在 Basic Auth 之后。
func (h *Handler) keysList(w http.ResponseWriter, r *http.Request) {
	views := h.keys.list()
	enabled := 0
	for _, v := range views {
		if v.Enabled {
			enabled++
		}
	}
	// unconfigured=true 提示前端：当前"不鉴权"是因为从未配置过 key。
	// 这是安全上值得显式提醒的状态（公网暴露会直接裸奔）。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"keys":         views,
		"total":        len(views),
		"enabled":      enabled,
		"unconfigured": !h.keys.configured(),
		"auth_enabled": h.keys.configured(),
	})
}

// keysCreate 生成一个新 key。
func (h *Handler) keysCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	// body 可选：允许空体（直接生成匿名 key）。
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	v, err := h.keys.generate(body.Name)
	if err != nil {
		log.Printf("ERR: [keys] 生成失败: %v", err)
		writeOpenAIError(w, http.StatusInternalServerError, "generate_failed", err.Error())
		return
	}
	// 日志只记掩码与前缀，不记完整 key。
	log.Printf("[keys] 已生成新 key（preview=%s name=%q）", v.Preview, v.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": v})
}

// keysDelete 删除一个看板生成的 key。config 源的 key 拒绝删除（下次启动会回来），
// 提示改用吊销。
func (h *Handler) keysDelete(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing_key", "缺少 key")
		return
	}
	if err := h.keys.removeGenerated(key); err != nil {
		// 区分"不是生成源"（客户端用法问题，400）与"落盘失败"（服务端问题，500）。
		code := http.StatusBadRequest
		if strings.Contains(err.Error(), "失败") {
			code = http.StatusInternalServerError
		}
		writeOpenAIError(w, code, "delete_failed", err.Error())
		return
	}
	log.Printf("[keys] 已删除 key（preview=%s）", keyPreview(key))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
}

// keysSetEnabled 吊销 / 恢复一个 key（两种来源通用）。
func (h *Handler) keysSetEnabled(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.PathValue("key"))
		if key == "" {
			writeOpenAIError(w, http.StatusBadRequest, "missing_key", "缺少 key")
			return
		}
		if err := h.keys.setEnabled(key, enabled); err != nil {
			code := http.StatusBadRequest
			if strings.Contains(err.Error(), "失败") {
				code = http.StatusInternalServerError
			}
			writeOpenAIError(w, code, "toggle_failed", err.Error())
			return
		}
		action := "吊销"
		if enabled {
			action = "恢复"
		}
		log.Printf("[keys] 已%s key（preview=%s）", action, keyPreview(key))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": enabled})
	}
}
