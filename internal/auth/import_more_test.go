package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 格式 A：嵌套形（单对象）——用户提供的第 2 种格式（token 截断为占位）。
const formatNested = `{
  "account": {
    "enterpriseId": "",
    "nickname": "示例昵称甲",
    "uid": "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
  },
  "auth": {
    "accessToken": "AT-NESTED",
    "domain": "www.codebuddy.cn",
    "expiresAt": 1794405600,
    "refreshToken": "RT-NESTED"
  }
}`

// 格式 B：扁平 snake_case 数组——用户提供的第 3 种格式（保留全部冗余字段）。
const formatArray = `[
  {
    "uid": "d8e01255-0168-464f-b967-f24a3010782a",
    "nickname": "Kingen",
    "access_token": "AT-KINGEN",
    "refresh_token": "RT-KINGEN",
    "expires_in": 5184000,
    "refresh_expires_in": 7776000,
    "domain": "www.codebuddy.cn",
    "enterprise_id": "",
    "source": "platform",
    "credits": 2092,
    "tag": "0913",
    "created_at": 1789229652
  },
  {
    "uid": "7b6c855c-77c4-4f7c-8e60-10000f624283",
    "nickname": "Angel Luis",
    "access_token": "AT-ANGEL",
    "refresh_token": "RT-ANGEL",
    "expires_in": 5184000,
    "domain": "www.codebuddy.cn",
    "credits": 2094,
    "tag": "0913"
  }
]`

// TestParseImportNestedFormat 嵌套形（格式 2）。
func TestParseImportNestedFormat(t *testing.T) {
	ok, results, err := ParseImport([]byte(formatNested))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 1 || len(results) != 1 {
		t.Fatalf("ok=%d results=%d want 1/1", len(ok), len(results))
	}
	a := ok[0]
	if a.UID != "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d" {
		t.Errorf("UID=%q", a.UID)
	}
	if a.Nickname != "示例昵称甲" {
		t.Errorf("Nickname=%q（UTF-8 应原样保留）", a.Nickname)
	}
	if a.AccessToken != "AT-NESTED" || a.RefreshToken != "RT-NESTED" {
		t.Errorf("tokens wrong: %+v", a)
	}
	if a.ExpiresAt != 1794405600 {
		t.Errorf("ExpiresAt=%d want 1794405600（绝对时间直用）", a.ExpiresAt)
	}
	if a.Domain != "www.codebuddy.cn" {
		t.Errorf("Domain=%q", a.Domain)
	}
	if !results[0].OK {
		t.Errorf("result not OK: %+v", results[0])
	}
}

// TestParseImportArraySnakeCase 数组 + snake_case（格式 3），含多余字段与 expires_in 换算。
func TestParseImportArraySnakeCase(t *testing.T) {
	before := time.Now().Unix()
	ok, results, err := ParseImport([]byte(formatArray))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 2 {
		t.Fatalf("imported %d want 2 (results=%+v)", len(ok), results)
	}
	if ok[0].UID != "d8e01255-0168-464f-b967-f24a3010782a" || ok[0].Nickname != "Kingen" {
		t.Errorf("entry0=%+v", ok[0])
	}
	if ok[0].AccessToken != "AT-KINGEN" || ok[0].RefreshToken != "RT-KINGEN" {
		t.Errorf("snake_case token fields not mapped: %+v", ok[0])
	}
	if ok[1].Nickname != "Angel Luis" || ok[1].AccessToken != "AT-ANGEL" {
		t.Errorf("entry1=%+v", ok[1])
	}
	// expires_in 是"剩余秒数"→ 应换算成未来绝对时间（5184000s = 60 天）。
	wantMin := before + 5184000 - 5
	wantMax := before + 5184000 + 5
	if ok[0].ExpiresAt < wantMin || ok[0].ExpiresAt > wantMax {
		t.Errorf("ExpiresAt=%d want ≈now+5184000 (between %d and %d)", ok[0].ExpiresAt, wantMin, wantMax)
	}
	// 冗余字段（credits/tag/source/created_at）被忽略，不应导致失败。
	if !results[0].OK || !results[1].OK {
		t.Errorf("extra fields should not break import: %+v", results)
	}
}

// TestParseImportWrappedAccounts 支持 {"accounts":[...]} 包装。
func TestParseImportWrappedAccounts(t *testing.T) {
	raw := `{"accounts":[` + strings.TrimSuffix(strings.TrimPrefix(formatArray, "["), "]") + `]}`
	ok, _, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 2 {
		t.Errorf("imported %d want 2", len(ok))
	}
}

// TestParseImportPartialFailure 部分成功：坏条目单独报错，好条目照常导入。
func TestParseImportPartialFailure(t *testing.T) {
	raw := `[
	  {"uid":"good-1","nickname":"Good","access_token":"AT-1","expires_in":100},
	  {"uid":"","access_token":"AT-2"},
	  {"uid":"no-token","access_token":""},
	  {"uid":"good-2","nickname":"Good2","access_token":"AT-3","refresh_token":"RT-3"}
	]`
	ok, results, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 2 {
		t.Errorf("imported %d want 2", len(ok))
	}
	if len(results) != 4 {
		t.Fatalf("results %d want 4", len(results))
	}
	if !results[0].OK || results[1].OK || results[2].OK || !results[3].OK {
		t.Errorf("unexpected ok flags: %+v", results)
	}
	// 失败原因应可读且**不含任何 token**。
	for _, r := range results {
		if r.OK {
			continue
		}
		if r.Error == "" {
			t.Errorf("failed entry has empty error: %+v", r)
		}
		for _, secret := range []string{"AT-2", "AT-1", "RT-"} {
			if strings.Contains(r.Error, secret) {
				t.Errorf("error leaked token material: %q", r.Error)
			}
		}
	}
}

// TestParseImportInvalid 无效输入给出可读错误。
func TestParseImportInvalid(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"空", ""},
		{"空白", "   "},
		{"非 JSON", "hello"},
		{"空数组", "[]"},
		{"坏 JSON", `{"uid":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := ParseImport([]byte(tc.raw)); err == nil {
				t.Errorf("want error for %q", tc.raw)
			}
		})
	}
}

// TestFileNameFor 文件名必须匹配 LoadDir 的 workbuddy*.json 扫描，
// 且要防路径穿越（uid 来自外部输入）。
func TestFileNameFor(t *testing.T) {
	for _, tc := range []struct{ uid, want string }{
		{"a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d", "workbuddy-a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d.json"},
		{"abc", "workbuddy-abc.json"},
		{"../../etc/passwd", "workbuddy-etc-passwd.json"}, // 穿越字符被替换
		{"a/b\\c", "workbuddy-a-b-c.json"},
		{"", "workbuddy-unknown.json"},
		{"...", "workbuddy-unknown.json"},
	} {
		got := FileNameFor(tc.uid)
		if got != tc.want {
			t.Errorf("FileNameFor(%q)=%q want %q", tc.uid, got, tc.want)
		}
		// 必须匹配扫描模式，否则重启后加载不到。
		if !strings.HasPrefix(got, "workbuddy") || !strings.HasSuffix(got, ".json") {
			t.Errorf("FileNameFor(%q)=%q breaks LoadDir glob", tc.uid, got)
		}
		// 不得含路径分隔符。
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("FileNameFor(%q)=%q contains path separator", tc.uid, got)
		}
	}
}

// TestImportThenLoadDirRoundTrip 导入写盘的文件能被 LoadDir 读回（端到端一致性）。
func TestImportThenLoadDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	for _, raw := range []string{formatNested, formatArray} {
		ok, _, err := ParseImport([]byte(raw))
		if err != nil {
			t.Fatalf("ParseImport: %v", err)
		}
		for _, a := range ok {
			if _, err := a.Save(dir); err != nil {
				t.Fatalf("Save uid=%s: %v", a.UID, err)
			}
		}
	}

	loaded, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(loaded) != 3 {
		t.Fatalf("LoadDir returned %d want 3（导入的账号必须能被重启加载）", len(loaded))
	}
	byUID := map[string]*Auth{}
	for _, a := range loaded {
		byUID[a.UID] = a
	}
	if a := byUID["a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"]; a == nil || a.AccessToken != "AT-NESTED" {
		t.Errorf("nested account not round-tripped: %+v", a)
	}
	if a := byUID["d8e01255-0168-464f-b967-f24a3010782a"]; a == nil || a.AccessToken != "AT-KINGEN" {
		t.Errorf("array account not round-tripped: %+v", a)
	}
	if a := byUID["7b6c855c-77c4-4f7c-8e60-10000f624283"]; a == nil || a.Nickname != "Angel Luis" {
		t.Errorf("second array account wrong: %+v", a)
	}
}

// TestImportSavedFileIsNestedForm 导入写盘必须是嵌套形（与既有文件一致，插件可读）。
func TestImportSavedFileIsNestedForm(t *testing.T) {
	dir := t.TempDir()
	ok, _, err := ParseImport([]byte(formatArray))
	if err != nil {
		t.Fatal(err)
	}
	path, err := ok[0].Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("saved file not valid JSON: %v", err)
	}
	if _, hasAuth := doc["auth"]; !hasAuth {
		t.Errorf("saved file missing nested 'auth' object: %s", raw)
	}
	if _, hasAcct := doc["account"]; !hasAcct {
		t.Errorf("saved file missing nested 'account' object: %s", raw)
	}
	// 权限：仅类 Unix 有意义（Windows 不支持 Unix 权限位）。
	if st, err := os.Stat(filepath.Join(dir, FileNameFor(ok[0].UID))); err == nil {
		if perm := st.Mode().Perm(); perm != 0o600 && perm != 0o666 {
			t.Errorf("perm=%o want 0600", perm)
		}
	}
}

// formatTopLevelToken 格式 E：Web 控制台导出。
// 特征：token 在**顶层**（snake 形态，含跨行冗余的 account/auth/accounts/allAccounts），
// 而 accounts[] 数组里**只有账号元信息、没有 token**。
// 回归背景：早期实现只要看到 accounts[] 就只解析数组条目，导致此形态整条报
// 「缺少 access_token」——数据没毛病，是解析器漏了这种布局。
const formatTopLevelToken = `{
  "account": {
    "uid": "f0e1d2c3-b4a5-4968-8778-99aabbccddee",
    "nickname": "示例昵称乙",
    "type": "personal",
    "lastLogin": true
  },
  "auth": {
    "accessToken": "AT-TOPLEVEL",
    "expiresIn": 5184000,
    "refreshToken": "RT-TOPLEVEL",
    "domain": "www.codebuddy.cn",
    "expiresAt": 1794536854500
  },
  "accounts": [
    {
      "uid": "f0e1d2c3-b4a5-4968-8778-99aabbccddee",
      "nickname": "示例昵称乙",
      "type": "personal",
      "lastLogin": true,
      "phoneNumber": "",
      "mpOpenId": ""
    }
  ],
  "allAccounts": [
    {"uid": "f0e1d2c3-b4a5-4968-8778-99aabbccddee", "nickname": "示例昵称乙"}
  ],
  "access_token": "AT-TOPLEVEL",
  "refresh_token": "RT-TOPLEVEL",
  "uid": "f0e1d2c3-b4a5-4968-8778-99aabbccddee",
  "nickname": "示例昵称乙",
  "domain": "www.codebuddy.cn",
  "expires_at": 1794536854500
}`

// TestParseImportTopLevelTokenWithAccountsArray 格式 E：顶层 token + accounts[] 元信息。
// 期望：数组条目从顶层补齐 token/domain，成功导入且不报「缺少 access_token」。
func TestParseImportTopLevelTokenWithAccountsArray(t *testing.T) {
	ok, results, err := ParseImport([]byte(formatTopLevelToken))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 1 {
		t.Fatalf("imported %d want 1 (results=%+v)", len(ok), results)
	}
	a := ok[0]
	if a.AccessToken != "AT-TOPLEVEL" {
		t.Errorf("AccessToken = %q, want AT-TOPLEVEL（应从顶层补齐）", a.AccessToken)
	}
	if a.RefreshToken != "RT-TOPLEVEL" {
		t.Errorf("RefreshToken = %q, want RT-TOPLEVEL", a.RefreshToken)
	}
	if a.UID != "f0e1d2c3-b4a5-4968-8778-99aabbccddee" {
		t.Errorf("UID = %q", a.UID)
	}
	if a.Domain != "www.codebuddy.cn" {
		t.Errorf("Domain = %q, want www.codebuddy.cn", a.Domain)
	}
	// expires_at 是毫秒，须归一为 Unix 秒：1794536854500 / 1000 = 1794536854。
	if a.ExpiresAt != 1794536854 {
		t.Errorf("ExpiresAt = %d, want 1794536854（毫秒应换算为秒）", a.ExpiresAt)
	}
}

// TestParseImportArrayEntryWinsOverTopLevel 数组条目自带 token 时以条目为准，
// 不被顶层覆盖——保证格式 A（accounts[] 内含 token）的既有语义不被新逻辑破坏。
func TestParseImportArrayEntryWinsOverTopLevel(t *testing.T) {
	raw := `{
  "access_token": "AT-TOP",
  "refresh_token": "RT-TOP",
  "accounts": [
    {"uid": "u-1", "nickname": "n1", "access_token": "AT-ENTRY", "refresh_token": "RT-ENTRY", "domain": "d1"}
  ]
}`
	ok, _, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 1 {
		t.Fatalf("imported %d want 1", len(ok))
	}
	if ok[0].AccessToken != "AT-ENTRY" {
		t.Errorf("AccessToken = %q, want AT-ENTRY（条目自身优先）", ok[0].AccessToken)
	}
	if ok[0].Domain != "d1" {
		t.Errorf("Domain = %q, want d1", ok[0].Domain)
	}
}

// TestParseImportSecondResolutionUnchanged 秒级 expires_at 不被误当作毫秒。
// 判据用绝对值阈值（>1e11），公元 5138 年前的秒级时间戳都不受影响。
func TestParseImportSecondResolutionUnchanged(t *testing.T) {
	raw := `[{"uid":"u-2","access_token":"AT","refresh_token":"RT","expires_at": 1794536854}]`
	ok, _, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(ok) != 1 {
		t.Fatalf("imported %d want 1", len(ok))
	}
	if ok[0].ExpiresAt != 1794536854 {
		t.Errorf("ExpiresAt = %d, want 1794536854（秒级不应被除以 1000）", ok[0].ExpiresAt)
	}
}
