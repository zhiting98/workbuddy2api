// Package oauth 实现 WorkBuddy 设备授权登录（CN realm）的上游调用。
//
// 由 cmd/login（CLI，配合 login.sh）与 internal/server（看板「添加账号」）共用，
// 保证两条路径走完全相同的协议语义——上游 token 结构与错误判定只此一份实现。
//
// 流程（无 PKCE，state 由服务端签发）：
//
//	StartLogin()  → POST /v2/plugin/auth/state?platform=CLI  拿 state + authUrl
//	  用户在浏览器完成登录
//	PollLogin()   → GET  /v2/plugin/auth/token?state=         一次，拿 token bundle
//	              → GET  /v2/plugin/login/account?state=       拿 uid/nickname
//
// 设计约束：
//   - **无状态**：包内不保存 state 与会话，并发由调用方（server 的会话表）负责。
//     这样 CLI 与服务端可各自选择合适的存储，不会互相干扰。
//   - 每次调用独立 cookie jar（多账号登录互不串会话）。
//   - **绝不打印 token**：任何日志/错误信息都不含凭证。
package oauth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

// 上游常量（CN only）。
const (
	BaseCN        = "https://copilot.tencent.com"
	clientUA      = "CLI/2.63.2 CodeBuddy/2.63.2"
	originReferer = "https://www.codebuddy.cn"
)

// baseURL 上游 base，可被测试覆盖（生产恒为 BaseCN，不对外暴露 setter）。
// 与 upstream.Client.ChatBaseCN 同思路：留一个可注入点，避免测试打真实上游。
var baseURL = BaseCN

// SetBaseURLForTest 覆盖上游 base 并返回旧值，仅供测试使用。
// 生产代码不得调用——它是包级可变状态，会串到并发用例上。
func SetBaseURLForTest(u string) string {
	old := baseURL
	baseURL = u
	return old
}

// 端点路径（相对 baseURL）。
const (
	pathAuthState = "/v2/plugin/auth/state?platform=CLI"
	pathLoginAcct = "/v2/plugin/login/account?state="
	pathAuthToken = "/v2/plugin/auth/token?state="
)

func endpointAuthState() string { return baseURL + pathAuthState }
func endpointLoginAcct() string { return baseURL + pathLoginAcct }
func endpointAuthToken() string { return baseURL + pathAuthToken }

// timeout 单次上游调用总时长上限（登录是交互流程，给足余量）。
const timeout = 30 * time.Second

// ErrPending 表示用户尚未在浏览器完成授权（上游 code!=0 或 token 为空）。
// 调用方据此区分「还在等」与「真失败」——前者应继续轮询，后者应停止。
var ErrPending = fmt.Errorf("登录未完成（waiting for login）")

// Bundle 一次成功登录的完整凭证。
type Bundle struct {
	AccessToken  string
	RefreshToken string
	Domain       string
	ExpiresIn    int64
	UID          string
	EnterpriseID string
	Nickname     string
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// newClient 每个流程独立 cookie jar（多账号登录互不串会话）。
func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: timeout, Jar: jar}
}

// commonHeaders 通用请求头（与网关出站口径一致）。
func commonHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originReferer)
	req.Header.Set("Referer", originReferer+"/")
	req.Header.Set("User-Agent", clientUA)
}

// doJSON 发请求并解信封。返回 (data, httpStatus, err)。
// code!=0 时返回错误（错误信息只含上游 code/msg，不含任何凭证）。
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		commonHeaders(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// StartLogin 发起一次设备授权：返回 state 与供用户打开的 authUrl。
// state 是后续 PollLogin 的唯一凭据，**由调用方负责保存与过期清理**。
func StartLogin() (state, authURL string, err error) {
	client := newClient()
	data, _, err := doJSON(client, http.MethodPost, endpointAuthState(), nil, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	return st.State, st.AuthURL, nil
}

// PollLogin 查询一次登录结果。
//
// 返回：
//   - (*Bundle, nil)      —— 登录完成
//   - (nil, ErrPending)   —— 尚未完成，调用方应继续轮询
//   - (nil, 其他 error)   —— 真失败（网络/上游 5xx），调用方应停止并报错
//
// 语义依据：上游 auth/token 是权威登录状态端点，pending 时业务 code 非 0
// （"login ing"），完成时 code=0 且带 token bundle。故 4xx 视作 pending
// （用户还没登完），5xx/网络错误视作真失败。
func PollLogin(state string) (*Bundle, error) {
	if strings.TrimSpace(state) == "" {
		return nil, fmt.Errorf("empty state")
	}
	client := newClient()
	tokRaw, status, errTok := doJSON(client, http.MethodGet, endpointAuthToken()+state, nil, nil)
	if errTok != nil {
		// status==0（传输层失败）或 5xx → 真失败；其余（4xx，含上游 code!=0）→ 仍在等待。
		if status == 0 || status >= 500 {
			return nil, fmt.Errorf("token endpoint error: %w", errTok)
		}
		return nil, ErrPending
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}

	// login/account 拿 uid/nickname（带 Bearer）。此步失败不阻断登录：
	// token 已拿到即可用，uid 缺失时由调用方兜底（如用 token 哈希占位）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		commonHeaders(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, endpointLoginAcct()+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	return &Bundle{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
		ExpiresIn:    tok.ExpiresIn,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}, nil
}
