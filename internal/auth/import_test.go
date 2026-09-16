package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadDirAcceptsUserProvidedFormat 验证用户提供的嵌套形 JSON 能被直接加载，
// 并锁定"文件名必须匹配 workbuddy*.json"这一约定（不匹配会被静默跳过）。
func TestLoadDirAcceptsUserProvidedFormat(t *testing.T) {
	dir := t.TempDir()

	// 用户实际粘贴的格式（token 用短串代替，不影响解析）。
	body := `{
  "account": {
    "enterpriseId": "",
    "nickname": "示例昵称甲",
    "uid": "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d"
  },
  "auth": {
    "accessToken": "eyJhbGciOiJSUzI1NiJ9.PAYLOAD.SIG",
    "domain": "www.codebuddy.cn",
    "expiresAt": 1794405600,
    "refreshToken": "eyJhbGciOiJIUzUxMiJ9.PAYLOAD2.SIG2"
  }
}`

	// 文件名用夹具里的 uid 前缀（匹配 LoadDir 的 workbuddy*.json 扫描约定）。
	// 注意：这里必须用**合成 uid**的前缀，不要用任何真实账号的 uid 片段。
	good := filepath.Join(dir, "workbuddy-a1b2c3d4.json")
	bad := filepath.Join(dir, "account.json") // 命名不符：应被跳过
	for _, p := range []string{good, bad} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("loaded %d accounts, want 1（account.json 应因命名不符被跳过）", len(got))
	}
	a := got[0]
	if a.UID != "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d" {
		t.Errorf("UID=%q", a.UID)
	}
	if a.Nickname != "示例昵称甲" {
		t.Errorf("Nickname=%q want 示例昵称甲（UTF-8 昵称应原样保留）", a.Nickname)
	}
	if a.AccessToken == "" || a.RefreshToken == "" {
		t.Errorf("tokens missing: %+v", a)
	}
	if a.Domain != "www.codebuddy.cn" {
		t.Errorf("Domain=%q", a.Domain)
	}
	if a.ExpiresAt != 1794405600 {
		t.Errorf("ExpiresAt=%d", a.ExpiresAt)
	}
	if a.FilePath != good {
		t.Errorf("FilePath=%q want %q", a.FilePath, good)
	}
}
