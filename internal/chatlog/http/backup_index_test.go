package http

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func mkDirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
	}
}

func TestBackupIndex_EmptyRoot(t *testing.T) {
	idx := NewBackupIndex("", nil)
	if err := idx.Scan(); err != nil {
		t.Errorf("Scan with empty root should be non-fatal, got %v", err)
	}
	chat, hex, unk := idx.Stats()
	if chat != 0 || hex != 0 || unk != 0 {
		t.Errorf("expected empty stats, got chat=%d hex=%d unk=%d", chat, hex, unk)
	}
}

func TestBackupIndex_MissingRoot(t *testing.T) {
	idx := NewBackupIndex(filepath.Join(t.TempDir(), "does-not-exist"), nil)
	if err := idx.Scan(); err != nil {
		t.Errorf("Scan with missing root should be non-fatal, got %v", err)
	}
}

func TestBackupIndex_ChatroomMode(t *testing.T) {
	root := t.TempDir()
	mkDirs(t, root, "麦悠电台(52854121751@chatroom)")

	idx := NewBackupIndex(root, nil)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}

	dir, via, ok := idx.Resolve("52854121751@chatroom")
	if !ok {
		t.Fatal("expected talker to resolve")
	}
	if via != "chatroom" {
		t.Errorf("expected via=chatroom, got %q", via)
	}
	if !strings.HasSuffix(dir, "麦悠电台(52854121751@chatroom)") {
		t.Errorf("unexpected resolved dir: %q", dir)
	}

	chat, _, _ := idx.Stats()
	if chat != 1 {
		t.Errorf("expected 1 chatroom dir, got %d", chat)
	}
}

func TestBackupIndex_WxidMode(t *testing.T) {
	// 个人私聊 wxid_xxx 作为标识
	root := t.TempDir()
	mkDirs(t, root, "张三(wxid_abc123def)")

	idx := NewBackupIndex(root, nil)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}

	if _, _, ok := idx.Resolve("wxid_abc123def"); !ok {
		t.Error("expected wxid to resolve via chatroom/talker table")
	}
}

func TestBackupIndex_HexMode(t *testing.T) {
	root := t.TempDir()
	mkDirs(t, root, "拼车群(C606ACFA)")

	// 用户需要通过 folderMap 配置 talker → hex 关系
	folderMap := map[string]string{
		"27580424670@chatroom": "C606ACFA",
	}
	idx := NewBackupIndex(root, folderMap)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}

	dir, via, ok := idx.Resolve("27580424670@chatroom")
	if !ok {
		t.Fatal("expected map-configured talker to resolve")
	}
	if via != "map" {
		t.Errorf("expected via=map, got %q", via)
	}
	if !strings.HasSuffix(dir, "拼车群(C606ACFA)") {
		t.Errorf("unexpected dir: %q", dir)
	}
}

func TestBackupIndex_HexMode_LowercaseMap(t *testing.T) {
	// folderMap 里写的是小写 hex，也要能命中（normalize 后比对）
	root := t.TempDir()
	mkDirs(t, root, "群(C606ACFA)")

	idx := NewBackupIndex(root, map[string]string{
		"x@chatroom": "c606acfa",
	})
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := idx.Resolve("x@chatroom"); !ok {
		t.Error("lowercase map entry should still resolve")
	}
}

func TestBackupIndex_MixedFormats(t *testing.T) {
	root := t.TempDir()
	mkDirs(t, root,
		"群A(AABBCCDD)",                  // hex
		"群B(52854121751@chatroom)",       // chatroom
		"群C(wxid_somebody)",              // wxid
		"某文件夹",                           // unknown (无括号)
		"乱七八糟(随便写的)",                    // unknown (括号内不符合任何格式)
	)
	idx := NewBackupIndex(root, nil)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}
	chat, hex, unk := idx.Stats()
	if chat != 2 {
		t.Errorf("chatroom+wxid count: expected 2, got %d", chat)
	}
	if hex != 1 {
		t.Errorf("hex count: expected 1, got %d", hex)
	}
	if unk != 2 {
		t.Errorf("unknown count: expected 2, got %d", unk)
	}
}

func TestBackupIndex_MixedModeDirectoryTakesHex(t *testing.T) {
	// 真实数据里见过 "47442020514@chatroom(ECB5DD4C)" 这种混合形式
	// 尾部 (hex) 优先视作 hex 模式
	root := t.TempDir()
	mkDirs(t, root, "47442020514@chatroom(ECB5DD4C)")

	idx := NewBackupIndex(root, map[string]string{
		"47442020514@chatroom": "ECB5DD4C",
	})
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, via, ok := idx.Resolve("47442020514@chatroom"); !ok || via != "map" {
		t.Errorf("expected hex-mode resolve via=map, got via=%q ok=%v", via, ok)
	}
}

func TestBackupIndex_ResolveMiss(t *testing.T) {
	root := t.TempDir()
	mkDirs(t, root, "无关的群(AABBCCDD)")
	idx := NewBackupIndex(root, nil)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := idx.Resolve("未配置的群@chatroom"); ok {
		t.Error("unmapped talker should not resolve")
	}
}

func TestBackupIndex_ChatroomTakesPrecedenceOverMap(t *testing.T) {
	// 同一个 talker 既命中 chatroomToDir 又命中 hexToDir 时, 优先 chatroom
	root := t.TempDir()
	mkDirs(t, root,
		"ChatMode(x@chatroom)",
		"HexMode(DEADBEEF)",
	)
	idx := NewBackupIndex(root, map[string]string{
		"x@chatroom": "DEADBEEF",
	})
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}
	dir, via, ok := idx.Resolve("x@chatroom")
	if !ok {
		t.Fatal("expected resolve")
	}
	if via != "chatroom" {
		t.Errorf("expected chatroom precedence, got via=%q dir=%q", via, dir)
	}
}

func TestBackupIndex_Concurrency(t *testing.T) {
	root := t.TempDir()
	mkDirs(t, root, "群(x@chatroom)")
	idx := NewBackupIndex(root, nil)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}

	// 并发 Scan + Resolve 不应触发 race (go test -race)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = idx.Scan()
		}()
		go func() {
			defer wg.Done()
			_, _, _ = idx.Resolve("x@chatroom")
		}()
	}
	wg.Wait()
}

func TestBackupIndex_UpdateFolderMap(t *testing.T) {
	root := t.TempDir()
	mkDirs(t, root, "群(AABBCCDD)")
	idx := NewBackupIndex(root, nil)
	if err := idx.Scan(); err != nil {
		t.Fatal(err)
	}

	// 最初 talker 不通, 因为 map 为空
	if _, _, ok := idx.Resolve("t@chatroom"); ok {
		t.Error("should miss before map update")
	}
	// 热更 map 后立即命中, 不需要重 Scan
	idx.UpdateFolderMap(map[string]string{"t@chatroom": "AABBCCDD"})
	if _, via, ok := idx.Resolve("t@chatroom"); !ok || via != "map" {
		t.Errorf("after UpdateFolderMap expected via=map ok=true, got via=%q ok=%v", via, ok)
	}
}
