// import.go 账号导入：把**外部来源的多种 JSON 形态**归一成 []*Auth。
//
// 支持的形态（按实际收到的格式归纳）：
//
//  1. 嵌套形（插件 OAuth 输出，单对象）：
//     {"account":{"uid","nickname","enterpriseId"},"auth":{"accessToken","refreshToken","expiresAt","domain"}}
//  2. 扁平 camelCase（单对象）：{"accessToken","refreshToken","expiresAt","domain","uid","nickname"}
//  3. 扁平 snake_case（单个或**数组**，常见于批量导出）：
//     {"uid","nickname","access_token","refresh_token","expires_in","domain","enterprise_id",...}
//
// 之所以单独做一层而不是改 Parse()：Parse 负责"磁盘上我们自己写的文件"
// （格式固定、必须严格）；Import 负责"外面来的东西"（形态多、字段冗余、
// 需要尽量宽容并给出可读的逐条结果）。两者职责不同，混在一起会让 Parse 变松。
package auth

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ImportResult 单条导入结果（供前端逐条展示）。
type ImportResult struct {
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	// OK 为 true 表示已成功写入磁盘。
	OK bool `json:"ok"`
	// Error 为失败原因（OK=true 时为空）。**绝不含 token**。
	Error string `json:"error,omitempty"`
	// File 写入的文件路径（OK=true 时）。
	File string `json:"file,omitempty"`
}

// importEntry 所有支持形态的超集。snake/camel 双字段并存，
// 由 normalize 按"谁非空用谁"择优，避免为每种形态写一份结构体。
type importEntry struct {
	// 嵌套形
	Account *struct {
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterpriseId"`
	} `json:"account"`
	Auth *struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    int64  `json:"expiresAt"`
		Domain       string `json:"domain"`
	} `json:"auth"`

	// 扁平形（camel）
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
	Domain       string `json:"domain"`

	// 扁平形（snake，批量导出常见）
	AccessTokenSnake  string `json:"access_token"`
	RefreshTokenSnake string `json:"refresh_token"`
	// ExpiresIn 是"有效期秒数"而非绝对时间；为 0 时回落到 ExpiresAt。
	ExpiresIn int64 `json:"expires_in"`
	// ExpiresAtSnake 是 snake 形态的**绝对到期时间**，Web 控制台导出为**毫秒**。
	// 与 ExpiresIn（相对秒数）语义不同：前者是时间点，后者是时长。
	ExpiresAtSnake    int64  `json:"expires_at"`
	EnterpriseIDSnake string `json:"enterprise_id"`
}

// ParseImport 把任意支持形态的 JSON 归一成 []*Auth。
//
// 返回的每个 Auth **未设置 FilePath**（由调用方决定落盘位置），
// 且已做最低限度的校验（accessToken 与 uid 必须存在）——校验不过的条目
// 也会出现在结果的"错误条目"里，让调用方逐条汇报而不是整批失败。
func ParseImport(raw []byte) (ok []*Auth, results []ImportResult, err error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil, fmt.Errorf("导入内容为空")
	}

	var entries []importEntry
	switch trimmed[0] {
	case '[':
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, nil, fmt.Errorf("JSON 数组解析失败: %w", err)
		}
	case '{':
		// 单对象。三种常见形态：
		//   a) {"accounts":[...]}                  数组内自带 token（批量导出）
		//   b) {"account":{...},"auth":{...}}      嵌套形（插件 OAuth 输出）
		//   c) 顶层 token + accounts[] 仅放账号元信息（Web 控制台导出）
		// 形态 c 与 a 的区别：accounts[] 里的条目**没有 token**，token 在顶层。
		// 所以先整体落一次顶层字段，再把 accounts[] 逐条与顶层合并。
		var top importEntry
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, nil, fmt.Errorf("JSON 对象解析失败: %w", err)
		}
		var wrapped struct {
			Accounts json.RawMessage `json:"accounts"`
		}
		_ = json.Unmarshal(raw, &wrapped)

		if len(wrapped.Accounts) > 0 {
			var arr []importEntry
			if err := json.Unmarshal(wrapped.Accounts, &arr); err != nil {
				return nil, nil, fmt.Errorf("accounts 数组解析失败: %w", err)
			}
			entries = make([]importEntry, 0, len(arr))
			for _, e := range arr {
				entries = append(entries, mergeEntry(e, top))
			}
		} else {
			entries = []importEntry{top}
		}
	default:
		return nil, nil, fmt.Errorf("无法识别的内容：应为 JSON 对象或数组")
	}

	if len(entries) == 0 {
		return nil, nil, fmt.Errorf("没有可导入的账号条目")
	}

	for i, e := range entries {
		a, r := e.normalize(i)
		results = append(results, r)
		if r.OK {
			ok = append(ok, a)
		}
	}
	return ok, results, nil
}

// mergeEntry 用顶层字段补全数组条目的空缺：条目自身非空字段优先，空缺处取 top。
// 用途：Web 控制台导出的形态是「顶层放 token + accounts[] 只放账号元信息」，
// 直接按数组条目解析会因缺 token 而整条失败（见 §形态 c）。
// 只补空，不覆盖——数组条目若自带 token 则以自身为准。
func mergeEntry(e, top importEntry) importEntry {
	fillStr := func(dst *string, src string) {
		if *dst == "" {
			*dst = src
		}
	}
	// 顶层 token 可能在 camel/snake/嵌套 auth 三处，按 normalize 的优先级取值。
	if top.AccessToken == "" && top.AccessTokenSnake != "" {
		top.AccessToken = top.AccessTokenSnake
	}
	if top.RefreshToken == "" && top.RefreshTokenSnake != "" {
		top.RefreshToken = top.RefreshTokenSnake
	}
	if top.ExpiresAt == 0 && top.ExpiresIn > 0 {
		top.ExpiresAt = top.ExpiresIn
	}

	// token 判空须把条目自身的三种形态都算上：snake 形态的条目若只看 camel 字段，
	// 会被误判为空而被顶层覆盖——而 normalize 最终会读 snake，导致条目自己的 token 丢失。
	if e.AccessToken == "" && e.AccessTokenSnake == "" && e.Auth == nil {
		e.AccessToken = top.AccessToken
		e.AccessTokenSnake = top.AccessTokenSnake
	}
	if e.RefreshToken == "" && e.RefreshTokenSnake == "" && e.Auth == nil {
		e.RefreshToken = top.RefreshToken
		e.RefreshTokenSnake = top.RefreshTokenSnake
	}

	fillStr(&e.Domain, top.Domain)
	fillStr(&e.UID, top.UID)
	fillStr(&e.Nickname, top.Nickname)
	fillStr(&e.EnterpriseID, top.EnterpriseID)
	fillStr(&e.EnterpriseIDSnake, top.EnterpriseIDSnake)
	if e.ExpiresAt == 0 {
		e.ExpiresAt = top.ExpiresAt
	}
	if e.ExpiresAtSnake == 0 {
		e.ExpiresAtSnake = top.ExpiresAtSnake
	}
	if e.ExpiresIn == 0 {
		e.ExpiresIn = top.ExpiresIn
	}
	// 数组条目若带嵌套 auth（罕见），也一并补空。
	if e.Auth == nil && top.Auth != nil {
		e.Auth = top.Auth
	}
	return e
}

// normalize 归一单条，并给出该条的结果（含可读错误）。
func (e importEntry) normalize(idx int) (*Auth, ImportResult) {
	res := ImportResult{}

	// ── token：嵌套形优先，其次 camel，最后 snake ──
	access, refresh, domain := e.AccessToken, e.RefreshToken, e.Domain
	expiresAt := e.ExpiresAt
	uid, nick, ent := e.UID, e.Nickname, e.EnterpriseID
	if e.Auth != nil {
		if e.Auth.AccessToken != "" {
			access = e.Auth.AccessToken
		}
		if e.Auth.RefreshToken != "" {
			refresh = e.Auth.RefreshToken
		}
		if e.Auth.Domain != "" {
			domain = e.Auth.Domain
		}
		if e.Auth.ExpiresAt != 0 {
			expiresAt = e.Auth.ExpiresAt
		}
	}
	if e.Account != nil {
		if e.Account.UID != "" {
			uid = e.Account.UID
		}
		if e.Account.Nickname != "" {
			nick = e.Account.Nickname
		}
		if e.Account.EnterpriseID != "" {
			ent = e.Account.EnterpriseID
		}
	}
	if access == "" {
		access = e.AccessTokenSnake
	}
	if refresh == "" {
		refresh = e.RefreshTokenSnake
	}
	if ent == "" {
		ent = e.EnterpriseIDSnake
	}
	// expires_at（snake，绝对时间）优先级低于 camel/嵌套（同为空时才用）。
	if expiresAt == 0 && e.ExpiresAtSnake != 0 {
		expiresAt = e.ExpiresAtSnake
	}
	// expires_in 是"剩余秒数"，换算成绝对时间；仅在没有绝对时间时用。
	if expiresAt == 0 && e.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(e.ExpiresIn) * time.Second).Unix()
	}
	// 单位归一：Auth.ExpiresAt 约定为 Unix 秒，而 Web 控制台导出的 expires_at 是毫秒。
	// 判据用绝对值（毫秒时间戳 ≫ 秒时间戳），避免对"秒级但很大"的正常值误判：
	// 1e11 秒 ≈ 公元 5138 年，任何现实时间戳都不会超过，故 > 1e11 判为毫秒。
	if expiresAt > 1e11 {
		expiresAt /= 1000
	}

	res.UID = uid
	res.Nickname = nick

	if strings.TrimSpace(access) == "" {
		res.Error = fmt.Sprintf("第 %d 条缺少 access_token", idx+1)
		return nil, res
	}
	if strings.TrimSpace(uid) == "" {
		// uid 不能缺：文件名与池内索引都以它为准，缺了会与其他账号撞名。
		res.Error = fmt.Sprintf("第 %d 条缺少 uid", idx+1)
		return nil, res
	}

	a := &Auth{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		Domain:       domain,
		UID:          uid,
		EnterpriseID: ent,
		Nickname:     nick,
	}
	res.OK = true
	return a, res
}

// FileNameFor 返回账号在 authDir 下的标准文件名。
//
// 命名必须匹配 LoadDir 的扫描模式 `workbuddy*.json`，否则下次启动
// 不会被加载（且**静默跳过**，是这个项目最容易踩的坑之一）。
func FileNameFor(uid string) string {
	// uid 来自外部输入，需防路径穿越：只保留安全字符。
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, uid)
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "unknown"
	}
	return "workbuddy-" + safe + ".json"
}

// Save 把账号原子写入 dir（文件名按 FileNameFor），返回写入路径。
// 复用 SaveAtomic 的 tmp+rename 与空 token 保护。
func (a *Auth) Save(dir string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("auth_dir 未配置")
	}
	a.FilePath = filepath.Join(dir, FileNameFor(a.UID))
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}
