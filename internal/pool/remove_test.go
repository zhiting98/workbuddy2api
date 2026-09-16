package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// TestRemoveUIDDropsAccountAndPersists 移除后池中不存在，且状态立即落盘。
func TestRemoveUIDDropsAccountAndPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999})
	p.Flush()

	if !p.RemoveUID("u1") {
		t.Fatal("RemoveUID(u1) = false want true")
	}
	if _, ok := p.Status("u1"); ok {
		t.Error("u1 still in pool")
	}
	if _, ok := p.Status("u2"); !ok {
		t.Error("u2 should be untouched")
	}

	// 立即落盘：读回 state.json 不应含 u1。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(raw), "u1") {
		t.Errorf("state.json still has u1: %s", raw)
	}
	if !strings.Contains(string(raw), "u2") {
		t.Errorf("state.json lost u2: %s", raw)
	}
}

// TestRemoveUIDUnknown 移除不存在的 uid 返回 false（幂等，不 panic）。
func TestRemoveUIDUnknown(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	if p.RemoveUID("nope") {
		t.Error("RemoveUID(unknown) = true want false")
	}
}

// TestRemoveUIDThenReload 移除并重新加载后账号不会复活
// （防止"文件删了但 state.json 残留 → 幽灵条目"）。
func TestRemoveUIDThenReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "keep", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "gone", AccessToken: "at", ExpiresAt: 9999999999})
	p.RemoveUID("gone")

	p2 := New(fp)
	if _, ok := p2.Status("gone"); ok {
		t.Error("removed account resurrected after reload")
	}
	if _, ok := p2.Status("keep"); !ok {
		t.Error("kept account vanished after reload")
	}
}

// TestRemoveUIDDuringCooldown 冷却中的账号也能被移除（删除不应受状态影响）。
func TestRemoveUIDDuringCooldown(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "cool", AccessToken: "at", ExpiresAt: 9999999999})
	p.Cooldown("cool", CoolSoft, time.Hour, "test")
	if st, _ := p.Status("cool"); !st.Cooling {
		t.Fatal("setup: account should be cooling")
	}
	if !p.RemoveUID("cool") {
		t.Error("should remove a cooling account")
	}
	if _, ok := p.Status("cool"); ok {
		t.Error("cooling account still present")
	}
}

// TestRemoveUIDDuringBreak 熔断中的账号也能被移除。
func TestRemoveUIDDuringBreak(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	p.Add(&auth.Auth{UID: "brk", AccessToken: "at", ExpiresAt: 9999999999})
	for i := 0; i < 5; i++ {
		p.NoteError("brk")
	}
	if !p.RemoveUID("brk") {
		t.Error("should remove a broken account")
	}
	if _, ok := p.Status("brk"); ok {
		t.Error("broken account still present")
	}
}
