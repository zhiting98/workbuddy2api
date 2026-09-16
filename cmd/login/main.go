// login.go — WorkBuddy CN OAuth 登录 CLI（设备授权流程，CN realm only）。
//
// 两个子命令，由 login.sh 顺序驱动：
//
//	login url   → 调 oauth.StartLogin 拿 state+authUrl，
//	              state 落 /tmp/wb2api-login-state.json，stdout 打印授权 URL
//	login poll  → 读 state，调 oauth.PollLogin 一次，
//	              stdout 打印完整 token+account JSON
//
// 协议实现已抽到 internal/oauth，与网关看板的「添加账号」共用同一份逻辑。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"workbuddy2api/internal/oauth"
)

// stateFile CLI 专用的 state 暂存（服务端走内存会话表，不用此文件）。
const stateFile = "/tmp/wb2api-login-state.json"

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	os.Exit(1)
}

type loginState struct {
	State string `json:"state"`
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: login <url|poll>")
	}

	switch os.Args[1] {
	case "url":
		state, authURL, err := oauth.StartLogin()
		if err != nil {
			fatal("auth state failed: %v", err)
		}
		raw, _ := json.Marshal(loginState{State: state})
		if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
			fatal("write state: %v", err)
		}
		fmt.Println(authURL)

	case "poll":
		raw, err := os.ReadFile(stateFile)
		if err != nil {
			fatal("read state: %v (先跑 login url)", err)
		}
		var ls loginState
		if err := json.Unmarshal(raw, &ls); err != nil {
			fatal("parse state: %v", err)
		}
		b, err := oauth.PollLogin(ls.State)
		if errors.Is(err, oauth.ErrPending) {
			fatal("登录未完成（waiting for login）。请确认已在浏览器完成登录再按 y")
		}
		if err != nil {
			fatal("token endpoint error: %v", err)
		}
		out := map[string]any{
			"access_token":  b.AccessToken,
			"refresh_token": b.RefreshToken,
			"expires_in":    b.ExpiresIn,
			"domain":        b.Domain,
			"uid":           b.UID,
			"enterprise_id": b.EnterpriseID,
			"nickname":      b.Nickname,
		}
		oraw, _ := json.Marshal(out)
		fmt.Println(string(oraw))
		os.Remove(stateFile)

	default:
		fatal("unknown subcommand %q (want url|poll)", os.Args[1])
	}
}
