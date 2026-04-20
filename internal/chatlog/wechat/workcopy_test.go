package wechat

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mockConfigWithDirs 允许测试自定义 dataDir / workDir
type mockConfigWithDirs struct {
	mockConfig
	dataDir string
	workDir string
}

func (m *mockConfigWithDirs) GetDataDir() string { return m.dataDir }
func (m *mockConfigWithDirs) GetWorkDir() string { return m.workDir }

// helper：创建 source db + work copy 并各自设置 mtime
func setupWorkCopyFixture(t *testing.T, sourceTime, outputTime time.Time) (string, *Service) {
	t.Helper()
	baseDir := t.TempDir()
	dataDir := filepath.Join(baseDir, "data")
	workDir := filepath.Join(baseDir, "work")

	relPath := filepath.Join("db_storage", "message", "message_0.db")
	sourcePath := filepath.Join(dataDir, relPath)
	outputPath := filepath.Join(workDir, relPath)

	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(sourcePath, []byte("encrypted"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if !sourceTime.IsZero() {
		if err := os.Chtimes(sourcePath, sourceTime, sourceTime); err != nil {
			t.Fatalf("chtimes source: %v", err)
		}
	}

	// outputTime == zero 表示不创建 output
	if !outputTime.IsZero() {
		if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
			t.Fatalf("mkdir output: %v", err)
		}
		if err := os.WriteFile(outputPath, []byte("decrypted"), 0o644); err != nil {
			t.Fatalf("write output: %v", err)
		}
		if err := os.Chtimes(outputPath, outputTime, outputTime); err != nil {
			t.Fatalf("chtimes output: %v", err)
		}
	}

	svc := NewService(&mockConfigWithDirs{dataDir: dataDir, workDir: workDir})
	return sourcePath, svc
}

func TestIsWorkCopyUpToDate_NoWorkDir(t *testing.T) {
	svc := NewService(&mockConfigWithDirs{dataDir: "/some/dir", workDir: ""})
	if svc.isWorkCopyUpToDate("/some/dir/session.db") {
		t.Error("should return false when workDir empty")
	}
}

func TestIsWorkCopyUpToDate_OutputMissing(t *testing.T) {
	// 没有 workdir 副本 → 需要解密
	source, svc := setupWorkCopyFixture(t, time.Now(), time.Time{})
	if svc.isWorkCopyUpToDate(source) {
		t.Error("should return false when output does not exist")
	}
}

func TestIsWorkCopyUpToDate_OutputNewer(t *testing.T) {
	// output 比 source 新 → 已是最新，skip
	now := time.Now()
	source, svc := setupWorkCopyFixture(t,
		now.Add(-1*time.Hour), // source 更旧
		now,                   // output 新
	)
	if !svc.isWorkCopyUpToDate(source) {
		t.Error("output newer than source should be considered up-to-date")
	}
}

func TestIsWorkCopyUpToDate_OutputOlder(t *testing.T) {
	// output 比 source 旧 → 微信改过 db，需要重解
	now := time.Now()
	source, svc := setupWorkCopyFixture(t,
		now,                    // source 新
		now.Add(-1*time.Hour),  // output 旧
	)
	if svc.isWorkCopyUpToDate(source) {
		t.Error("output older than source should NOT be considered up-to-date")
	}
}

func TestIsWorkCopyUpToDate_SameMtime(t *testing.T) {
	// mtime 恰好相同（刚解密完，或文件系统精度落在同一秒）→ 视为最新
	now := time.Now()
	source, svc := setupWorkCopyFixture(t, now, now)
	if !svc.isWorkCopyUpToDate(source) {
		t.Error("equal mtime should be considered up-to-date (not Before)")
	}
}

func TestIsWorkCopyUpToDate_SourceMissing(t *testing.T) {
	// source 不存在（罕见：被删除）→ 保守返回 false，不 skip
	_, svc := setupWorkCopyFixture(t, time.Now(), time.Now())
	if svc.isWorkCopyUpToDate("/nonexistent/source.db") {
		t.Error("missing source should return false (conservative re-decrypt)")
	}
}
