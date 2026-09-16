// dashboard.go 看板页面托管：把 dashboard/html/index.html 嵌进二进制，
// 与网关同容器提供（不再需要独立 nginx）。
//
// 路径设计：页面里的请求都是相对的 `api/...`（→ `/api/...`），
// 因此这里把 `/api/` 前缀剥掉后交给既有 mux 处理：
//
//	浏览器            → 本容器 Go 服务
//	GET  /            → 看板 HTML（embed）
//	GET  /api/status  → 内部 /status
//	POST /api/accounts/login/start → 内部 /accounts/login/start
//
// 鉴权：合并后 nginx 不在了，改由 Go 直接做 Basic Auth（见 withBasicAuth）。
// 浏览器凭据只在用户输入时存在，**网关 api_key 不下发到页面**。
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"net/http"
	"strings"
)

// dashboardHTML 看板单页。用 embed 打进二进制：镜像只多一个文件（其实是 0 个，
// 全在二进制里），部署时不需要额外挂载或拷贝静态资源。
//
//go:embed dashboard/index.html
var dashboardHTML []byte

// dashboardEnabled 报告是否启用了看板托管。
// 未配置 dashboard.pass 时关闭：与旧行为一致（纯 API 网关），且避免
// 「误开一个无鉴权的管理页面」。
func (h *Handler) dashboardEnabled() bool {
	return h.cfg.DashboardUser != "" && h.cfg.DashboardPass != ""
}

// registerRoot 注册根路径 `/` 的**唯一**处理器。
//
// 为什么必须唯一：http.ServeMux 对同一模式重复注册会 panic，而 `/` 既是
// 看板入口、又是安装向导入口（二选一）。因此不在两个 register* 里各注册一次，
// 而是注册一个按运行态分发的处理器：
//
//	未安装（无看板凭据） → 安装向导（无需鉴权）
//	已安装               → 看板页面（Basic Auth）
//
// 这样安装完成时**不需要重新注册路由**（也就不会 panic），状态切换靠
// dashboardEnabled() 实时判定，天然幂等。
func (h *Handler) registerRoot() {
	h.mux.HandleFunc("GET /{$}", h.rootHandler)
}

// rootHandler 按当前是否已安装分发根路径。
func (h *Handler) rootHandler(w http.ResponseWriter, r *http.Request) {
	if h.dashboardEnabled() {
		h.withBasicAuth(h.dashboardIndex)(w, r)
		return
	}
	h.setupPage(w, r)
}

// registerDashboard 注册看板的数据入口 `/api/*`（Basic Auth 保护）。
//
// 根路径 `/` 由 registerRoot 统一处理，见其注释。
func (h *Handler) registerDashboard() {
	if !h.dashboardEnabled() {
		return
	}
	// 前端请求的 /api/* → 内部路由（剥前缀）。这是唯一给页面用的入口，
	// 同样走 Basic Auth，避免"页面受保护但数据接口裸奔"。
	h.mux.HandleFunc("/api/", h.withBasicAuth(h.dashboardAPI))
}

// dashboardIndex 返回看板 HTML。
func (h *Handler) dashboardIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // 页面随版本变化，别让浏览器留旧副本
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

// dashboardAPI 把 /api/<rest> 转发给内部路由处理。
//
// 实现方式：改写 r.URL.Path 后交给 h.mux。这样 /api/accounts/... 自然命中
// 已注册的 /accounts/... 处理器，不需要为每个端点写一份重复注册
// （少一处"新增端点忘了加 /api 前缀"的漂移点）。
//
// 鉴权衔接：内部路由都包了 withAuth（校验 api_key），而浏览器手里只有
// Basic 凭据。这里在转发前打上 "已通过看板鉴权" 的标记，withAuth 见到标记即放行
// ——避免把 api_key 硬编码进页面（那会让任何能打开页面的人拿到网关密钥）。
// 安全性由外层的 withBasicAuth 保证：能走到这里的一定已经过凭据校验。
func (h *Handler) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api")
	if rest == "" || rest == "/" {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown api path")
		return
	}
	r2 := r.Clone(context.WithValue(r.Context(), dashAuthedKey{}, true))
	r2.URL.Path = rest
	h.mux.ServeHTTP(w, r2)
}

// dashAuthedKey 请求上下文里标记"已经过看板 Basic 鉴权"的私有 key 类型。
// 用空结构体而非字符串：避免与其他包的 context key 撞名。
type dashAuthedKey struct{}

// dashAuthed 报告该请求是否已通过看板鉴权。
func dashAuthed(r *http.Request) bool {
	v, _ := r.Context().Value(dashAuthedKey{}).(bool)
	return v
}

// withBasicAuth 对看板相关路径做 HTTP Basic 鉴权。
//
// 为什么是自己实现而不是复用 withAuth：withAuth 校验的是 `api_key`
// （给 API 客户端用的），而看板登录是**另一个凭据**（dashboard_user/pass）。
// 两者分离的意义：把看板密码给运维同事，不必交出 API 密钥。
func (h *Handler) withBasicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := parseBasicAuth(r.Header.Get("Authorization"))
		if !ok || !h.checkDashboardCreds(user, pass) {
			// WWW-Authenticate 让浏览器弹原生登录框。
			w.Header().Set("WWW-Authenticate", `Basic realm="workbuddy dashboard", charset="UTF-8"`)
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// checkDashboardCreds 常量时间比较用户名与密码（防时序侧信道）。
func (h *Handler) checkDashboardCreds(user, pass string) bool {
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.DashboardUser)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.DashboardPass)) == 1
	return uOK && pOK
}

// parseBasicAuth 解析 Basic 头；格式非法返回 ok=false。
// 用标准库 base64 而非 r.BasicAuth()，是为了显式控制解析失败的行为
// （BasicAuth 在密码含冒号时会截断，这里按第一个冒号切分，语义一致且可读）。
func parseBasicAuth(header string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	s := string(raw)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
