// setup.go 首次安装向导（Web）。
//
// 背景：网关首次启动时 `dashboard.user/pass` 为空 → 看板不启用 → 访问 `/` 返回 404。
// 用户只能靠改配置文件或环境变量手工配置，容易卡住（本仓库历史上已多次出现
// "打包后没有前端页面"的困惑，根因就是这个静默 404）。
//
// 方案：**未配置凭据时，自动进入 setup 模式**——`/` 变成安装向导，
// 引导用户填看板账号密码，并顺带生成一个强随机 `api_key`。
// 安装完成后（凭据落盘 + 内存生效）向导自动关闭，`/` 恢复为受保护的看板。
//
// 安全模型（与 WordPress / Gitea 等安装器一致）：
//   - 向导本身**不鉴权**——此时系统尚无任何凭据，无从校验；这是设计而非疏漏。
//   - 因此**安装完成即关闭**：一旦凭据生效，`/setup*` 全部返回 404，
//     不存在"装完还能被人重设密码"的窗口。
//   - 向导不泄露任何已有信息：只做"设置"，不回显现有 api_key / 账号 / 统计。
//   - 写入 config.json 用 tmp+rename 原子替换，权限 0600。
//   - 若部署在公网，建议只在首次安装时暴露端口；装完即刻生效关闭。
package server

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

//go:embed setup_page.html
var setupHTML []byte

// setupState setup 模式的运行态。
type setupState struct {
	mu sync.Mutex
	// configPath 要写回的配置文件路径（空 = 无法持久化，只改内存）。
	configPath string
	// onInstall 安装成功后的回调：把新凭据应用到这个 handler（免重启）。
	onInstall func(user, pass, apiKey string)
}

// SetupDeps 供 NewHandler 注入向导依赖。
type SetupDeps struct {
	// ConfigPath config.json 路径；空表示只改内存、不落盘（会提示用户）。
	ConfigPath string
}

// Generator 生成强随机密钥（hex，n 字节 → 2n 字符）。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// setupEnabled 报告是否处于 setup 模式。
// 判定条件：看板凭据未配置。**注意**：只认凭据，不看 api_key——
// api_key 可能由环境变量提供，而看板凭据才是"是否装好"的标志。
func (h *Handler) setupEnabled() bool {
	return !h.dashboardEnabled()
}

// registerSetup 注册安装向导的**提交**路由。
//
// 根路径 `/` 由 registerRoot 按运行态分发（见 dashboard.go），此处只注册
// `/setup/install`。装好后该路由不存在（404），而不是"存在但拒绝"，
// 减少攻击面与误判。
func (h *Handler) registerSetup() {
	if !h.setupEnabled() {
		return
	}
	h.mux.HandleFunc("POST /setup/install", h.setupInstall)
}

// setupPage 渲染安装向导页面。
func (h *Handler) setupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(setupHTML)
}

// setupInstallReq 向导提交体。
type setupInstallReq struct {
	// User/Pass 看板登录凭据。留空则用默认 admin + 随机密码。
	User string `json:"user"`
	Pass string `json:"pass"`
	// APIKey 留空则自动生成；也允许用户自带（例如要与旧客户端保持一致）。
	APIKey string `json:"api_key"`
}

// setupInstall 处理安装提交：生成/校验凭据 → 落盘 → 立即生效。
//
// 幂等与安全：若此刻已不是 setup 模式（并发提交/已被安装），直接 404，
// 不覆盖已有配置——否则任何人在装好后重放该请求就能改掉密码。
func (h *Handler) setupInstall(w http.ResponseWriter, r *http.Request) {
	if !h.setupEnabled() {
		writeOpenAIError(w, http.StatusNotFound, "already_installed", "已完成安装，向导已关闭")
		return
	}

	body, ok := h.readBody(w, r, func(status int, msg string) {
		writeOpenAIError(w, status, "invalid_request", msg)
	})
	if !ok {
		return
	}
	var req setupInstallReq
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "bad_json", "请求体不是合法 JSON")
			return
		}
	}

	user := strings.TrimSpace(req.User)
	if user == "" {
		user = "admin"
	}
	pass := req.Pass
	generated := false
	if strings.TrimSpace(pass) == "" {
		p, err := randHex(12) // 24 字符，约 96 bit 熵
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", "生成密码失败: "+err.Error())
			return
		}
		pass = p
		generated = true
	}
	if err := validateCreds(user, pass); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "weak_creds", err.Error())
		return
	}

	apiKey := strings.TrimSpace(req.APIKey)
	apiKeyGenerated := false
	if apiKey == "" {
		k, err := randHex(24) // 48 字符
		if err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "rand_failed", "生成 API Key 失败: "+err.Error())
			return
		}
		apiKey = "sk-" + k
		apiKeyGenerated = true
	}

	// 落盘（若可写）。写失败**不阻断**内存配置生效，但要如实告知用户
	// ——否则重启后凭据丢失，用户会以为"又被重置了"。
	persisted := false
	var persistErr string
	if h.setup.configPath != "" {
		if err := writeConfigCreds(h.setup.configPath, user, pass, apiKey); err != nil {
			persistErr = err.Error()
		} else {
			persisted = true
		}
	} else {
		persistErr = "未提供 config 路径（可能以默认配置启动），配置仅存在于内存，重启后需重新安装"
	}

	// 立即生效（免重启）。
	h.cfg.DashboardUser = user
	h.cfg.DashboardPass = pass
	h.cfg.APIKey = apiKey
	// 把向导生成的 key 也纳入多 key 存储：否则安装后 h.keys 里没有这个 key，
	// 而 cfg.APIKey 已不再被 authOK 直接读取（authOK 只问 keys）。
	// 这是"单值字段迁到多 key 存储"的关键接线点，漏了会导致安装后立即 401。
	h.keys.addConfigKey(apiKey)
	// 注册看板数据入口 /api/*（根路径已由 registerRoot 动态分发，无需再注册，
	// 也不会与 /setup/install 冲突）。重复调用是安全的：dashboardEnabled()
	// 此时已为 true，但 ServeMux 不允许重复注册同一模式——故用 sync.Once 保护。
	h.dashboardOnce.Do(h.registerDashboard)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"user":             user,
		"pass":             pass,
		"api_key":          apiKey,
		"pass_generated":   generated,
		"apikey_generated": apiKeyGenerated,
		"persisted":        persisted,
		"persist_error":    persistErr,
	})
}

// validateCreds 基本强度校验——只挡明显不可用的输入，不做过度的密码策略
// （这是自托管内网工具，强制复杂度反而会让人写便签）。
func validateCreds(user, pass string) error {
	if len(pass) < 6 {
		return fmt.Errorf("密码至少 6 位")
	}
	if len(user) < 2 {
		return fmt.Errorf("用户名至少 2 位")
	}
	if user == pass {
		return fmt.Errorf("密码不能与用户名相同")
	}
	// 用户名不得含空白/控制字符（会进 Basic Auth 头与日志）。
	for _, r := range user {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("用户名不能包含空白或控制字符")
		}
	}
	return nil
}

// writeConfigCreds 把新凭据写回 config.json（读取现有配置 → 改三个字段 → 写回）。
//
// 只改这三个字段、**保留其余全部内容**：用户可能已经调过超时、冷却、排程等，
// 整体重写会把它们抹掉。也不重新序列化未知字段（用 map 保留原始结构）。
//
// 写入策略（两步，针对容器场景的必要妥协）：
//  1. 首选 tmp+rename —— 原子，不会出现半截配置。**但**当 config.json 是
//     docker 的**单文件绑定挂载**时，挂载点绑定在 inode 上，rename 会失败
//     （EBUSY: device or resource busy）。这是本项目部署方式（compose 挂载）
//     下的常态，所以必须有退路。
//  2. 退路：直接原地写。牺牲原子性，但能真正写入。为避免"写一半崩溃留半截
//     JSON"，先做一次备份（.bak），失败时用户仍有原文件可救。
func writeConfigCreds(path, user, pass, apiKey string) error {
	doc := map[string]any{}
	orig, readErr := os.ReadFile(path)
	if readErr == nil && len(orig) > 0 {
		if err := json.Unmarshal(orig, &doc); err != nil {
			return fmt.Errorf("现有 config 解析失败（不敢覆盖，以免丢失内容）: %w", err)
		}
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("读取现有 config 失败: %w", readErr)
	}
	doc["api_key"] = apiKey
	dash, _ := doc["dashboard"].(map[string]any)
	if dash == nil {
		dash = map[string]any{}
	}
	dash["user"] = user
	dash["pass"] = pass
	doc["dashboard"] = dash

	return writeConfigDoc(path, doc, orig)
}

// WriteScheduleSwitch 是 writeScheduleSwitch 的导出包装，供 cmd/server 注入
// server.Config.PersistScheduleSwitch 使用（跨包只能调导出符号）。
func WriteScheduleSwitch(path, key string, enabled bool) error {
	return writeScheduleSwitch(path, key, enabled)
}

// writeScheduleSwitch 把某个定时任务开关写回 config.json 的 schedule.<key>。
//
// 与安装向导共用 writeConfigDoc 的读-改-写与原子落盘策略（含绑定挂载退路），
// 不另起一套写文件逻辑——否则两处的"写失败该如何退化"迟早会漂移。
//
// key 取 *_enabled 字段名（checkin_enabled 等），未知 key 直接报错而不是静默写进去
// （静默会往配置里塞一个无人读的字段，用户以为生效了）。
func writeScheduleSwitch(path, key string, enabled bool) error {
	switch key {
	case "checkin", "travel", "activity", "keepalive":
	default:
		return fmt.Errorf("未知任务标识 %q", key)
	}
	doc := map[string]any{}
	orig, readErr := os.ReadFile(path)
	if readErr == nil && len(orig) > 0 {
		if err := json.Unmarshal(orig, &doc); err != nil {
			return fmt.Errorf("现有 config 解析失败（不敢覆盖，以免丢失内容）: %w", err)
		}
	} else if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("读取现有 config 失败: %w", readErr)
	}
	sched, _ := doc["schedule"].(map[string]any)
	if sched == nil {
		sched = map[string]any{}
	}
	sched[key+"_enabled"] = enabled
	doc["schedule"] = sched
	return writeConfigDoc(path, doc, orig)
}

// writeConfigDoc 把 doc 序列化后原子写回 path（tmp+rename，失败退化原地写）。
//
// orig 是原始文件内容（用于退路时留 .bak 备份）；调用方已负责读入并改好 doc。
func writeConfigDoc(path string, doc map[string]any, orig []byte) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}

	// 步 1：原子替换。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err == nil {
		if err := os.Rename(tmp, path); err == nil {
			return nil
		}
		_ = os.Remove(tmp) // rename 失败：清掉临时文件，走退路
	}

	// 步 2：原地写（绑定挂载场景）。先备份，便于人工恢复。
	if len(orig) > 0 {
		_ = os.WriteFile(path+".bak", orig, 0o600)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return nil
}
