package wechat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// buildFakeDataDir 在 t.TempDir() 下构造 data_dir/db_storage/{subDir}/{filename}
// 并用给定字节数填充文件内容，返回 dataDir 路径。
func buildFakeDataDir(t *testing.T, files map[string]int) string {
	t.Helper()
	dataDir := t.TempDir()
	for rel, size := range files {
		full := filepath.Join(dataDir, "db_storage", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		content := make([]byte, size)
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return dataDir
}

func TestPickSmallestDB_PrefersSessionDB(t *testing.T) {
	dataDir := buildFakeDataDir(t, map[string]int{
		"session/session.db":        4096,
		"message/message_0.db":      102400,
		"favorite/favorite_list.db": 2048, // 更小但优先级低
	})

	got, err := pickSmallestDB(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dataDir, "db_storage", "session", "session.db")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPickSmallestDB_FallsBackToMessage0(t *testing.T) {
	dataDir := buildFakeDataDir(t, map[string]int{
		// 无 session.db
		"message/message_0.db":      102400,
		"message/message_1.db":      4096, // 更小但优先级低
		"favorite/favorite_list.db": 2048,
	})

	got, err := pickSmallestDB(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dataDir, "db_storage", "message", "message_0.db")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPickSmallestDB_FallsBackToSmallest(t *testing.T) {
	dataDir := buildFakeDataDir(t, map[string]int{
		// 无 session.db，无 message_0.db
		"favorite/favorite_list.db": 102400,
		"contact/contact.db":        2048, // 最小
		"sns/sns.db":                4096,
	})

	got, err := pickSmallestDB(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dataDir, "db_storage", "contact", "contact.db")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPickSmallestDB_EmptyDir_ReturnsErrNoDBFile(t *testing.T) {
	dataDir := t.TempDir()
	// db_storage 目录都不存在

	_, err := pickSmallestDB(dataDir)
	if !errors.Is(err, ErrNoDBFile) {
		t.Errorf("got err %v, want ErrNoDBFile", err)
	}
}

func TestPickSmallestDB_ExcludesFtsFiles(t *testing.T) {
	dataDir := buildFakeDataDir(t, map[string]int{
		// 只有 fts 文件 - 必须被排除
		"message/message_fts.db": 1024,
		"message/message_fts_index.db": 512,
	})

	_, err := pickSmallestDB(dataDir)
	if !errors.Is(err, ErrNoDBFile) {
		t.Errorf("got err %v, want ErrNoDBFile (fts files should be excluded)", err)
	}
}

func TestPickSmallestDB_ExcludesZeroByteFiles(t *testing.T) {
	// 额外健壮性：0 字节文件（占位符）不应被选中
	dataDir := buildFakeDataDir(t, map[string]int{
		"message/message_0.db":  0,    // 不应被 Tier 2 选中
		"contact/contact.db":    2048, // 应降级到 Tier 3 选这个
	})

	got, err := pickSmallestDB(dataDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dataDir, "db_storage", "contact", "contact.db")
	if got != want {
		t.Errorf("got %q, want %q (0-byte message_0.db should be skipped)", got, want)
	}
}
