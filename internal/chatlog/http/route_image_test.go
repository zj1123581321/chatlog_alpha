package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// mockImageConfig 实现 Config 接口, 用于 /image/{md5} 决策树的完整集成测试。
type mockImageConfig struct {
	dataDir         string
	backupPath      string
	backupFolderMap map[string]string
}

func (m *mockImageConfig) GetHTTPAddr() string                   { return ":0" }
func (m *mockImageConfig) GetDataDir() string                    { return m.dataDir }
func (m *mockImageConfig) GetSaveDecryptedMedia() bool           { return false }
func (m *mockImageConfig) GetBackupPath() string                 { return m.backupPath }
func (m *mockImageConfig) GetBackupFolderMap() map[string]string { return m.backupFolderMap }

func setupImageTestService(t *testing.T, conf Config) *Service {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &Service{
		conf:         conf,
		router:       router,
		md5PathCache: make(map[string]CachedMediaMeta),
		backupIndex:  NewBackupIndex(conf.GetBackupPath(), conf.GetBackupFolderMap()),
	}
	if err := s.backupIndex.Scan(); err != nil {
		t.Fatalf("backup scan: %v", err)
	}
	s.initMediaRouter()
	return s
}

func doImageRequest(t *testing.T, s *Service, md5 string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/image/"+md5, nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)
	return w
}

// 场景 1: 全 miss → 404 + stats.not_found++
func TestServeImage_TotalMiss_Returns404(t *testing.T) {
	s := setupImageTestService(t, &mockImageConfig{dataDir: t.TempDir()})

	w := doImageRequest(t, s, "deadbeefdeadbeefdeadbeefdeadbeef")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
	if got := s.backupStats.NotFound.Load(); got != 1 {
		t.Errorf("expected not_found=1, got %d", got)
	}
}

// 场景 2: cache 无 meta → 不做全树 walk, 直接 404 + X-Backup-Hint 提示
//
// 设计取舍: 真实环境 msg/attach 常达 10 万+ 文件, 全树 walk 会把请求拖到 2 分钟
// (Windows NTFS 冷缓存), 浏览器看作永久 hang。宁可快速 404 + 提示下游预热 cache。
func TestServeImage_NoMeta_FastMissWithHint(t *testing.T) {
	dataDir := t.TempDir()
	md5 := "abcdef1234567890abcdef1234567890"
	// 故意在 msg/attach 下放 _t.dat, 证明即使磁盘上有, 无 meta 也不做全树扫描
	thumbDir := filepath.Join(dataDir, "msg", "attach", "xxx", "2026-04", "Img")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(thumbDir, md5+"_t.dat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := setupImageTestService(t, &mockImageConfig{dataDir: dataDir})
	w := doImageRequest(t, s, md5)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 (no meta = fast miss), got %d", w.Code)
	}
	if got := w.Header().Get("X-Backup-Hint"); got != "warm-cache-first" {
		t.Errorf("expected X-Backup-Hint=warm-cache-first, got %q", got)
	}
	if got := s.backupStats.NotFound.Load(); got != 1 {
		t.Errorf("expected not_found=1, got %d; stats=%v", got, s.backupStats.snapshot())
	}
}

// 场景 3: cache 有 meta + 仅 _t.dat + backup @chatroom 命中 → backup 返回
func TestServeImage_BackupHit_ChatroomMode(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()
	md5 := "backuphit000000000000000000000ab"
	talker := "12345@chatroom"
	msgTime := time.Date(2026, 4, 17, 17, 41, 32, 0, time.Local)

	// msg/attach 只放 _t.dat, 迫使决策树走 backup
	thumbDir := filepath.Join(dataDir, "msg", "attach", "yy", "2026-04", "Img")
	_ = os.MkdirAll(thumbDir, 0o755)
	_ = os.WriteFile(filepath.Join(thumbDir, md5+"_t.dat"), []byte("thumbbytes"), 0o644)

	// backup 目录使用 @chatroom 自动模式
	bkMonth := filepath.Join(backupDir, "测试群("+talker+")", "2026-04")
	_ = os.MkdirAll(bkMonth, 0o755)
	backupImgPath := filepath.Join(bkMonth, "20260417174132_img.jpg")
	_ = os.WriteFile(backupImgPath, []byte("BACKUP_ORIGINAL_JPG"), 0o644)

	s := setupImageTestService(t, &mockImageConfig{dataDir: dataDir, backupPath: backupDir})
	// populate cache 里的 meta
	s.md5PathCache[md5] = CachedMediaMeta{
		Path:   filepath.Join("msg", "attach", "yy", "2026-04", "Img", md5),
		Talker: talker,
		Time:   msgTime,
	}

	w := doImageRequest(t, s, md5)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", w.Code, w.Body.String())
	}
	if got := s.backupStats.Backup.Load(); got != 1 {
		t.Errorf("expected backup=1, got %d; stats=%v", got, s.backupStats.snapshot())
	}
	if got := w.Header().Get("X-Image-Source"); got != "backup" {
		t.Errorf("expected X-Image-Source=backup, got %q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), []byte("BACKUP_ORIGINAL_JPG")) {
		t.Errorf("body mismatch: got %q", w.Body.String())
	}
}

// 场景 4: backup hex 模式 + 用户配置 folderMap → 命中
func TestServeImage_BackupHit_HexMode(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()
	md5 := "hexmode000000000000000000000000f"
	talker := "27580424670@chatroom"
	msgTime := time.Date(2026, 4, 17, 17, 41, 32, 0, time.Local)

	// 无本地文件, 强迫走 backup
	bkMonth := filepath.Join(backupDir, "拼车群(C606ACFA)", "2026-04")
	_ = os.MkdirAll(bkMonth, 0o755)
	_ = os.WriteFile(filepath.Join(bkMonth, "20260417174132_Zzz.jpg"), []byte("HEX_BACKUP"), 0o644)

	s := setupImageTestService(t, &mockImageConfig{
		dataDir:         dataDir,
		backupPath:      backupDir,
		backupFolderMap: map[string]string{talker: "C606ACFA"},
	})
	s.md5PathCache[md5] = CachedMediaMeta{Talker: talker, Time: msgTime}

	w := doImageRequest(t, s, md5)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := s.backupStats.Backup.Load(); got != 1 {
		t.Errorf("expected backup=1, got %d", got)
	}
	if !bytes.Equal(w.Body.Bytes(), []byte("HEX_BACKUP")) {
		t.Errorf("body mismatch")
	}
}

// 场景 5: backup 同秒多图碰撞 → 返回第一个
func TestServeImage_BackupSameSecondCollision(t *testing.T) {
	dataDir := t.TempDir()
	backupDir := t.TempDir()
	md5 := "collision0000000000000000000000f"
	talker := "x@chatroom"
	msgTime := time.Date(2026, 4, 17, 17, 41, 32, 0, time.Local)

	bkMonth := filepath.Join(backupDir, "群("+talker+")", "2026-04")
	_ = os.MkdirAll(bkMonth, 0o755)
	// ReadDir 返回顺序按 Unicode 排序 (at least on most Go runtimes), 所以先写的 a 在前
	_ = os.WriteFile(filepath.Join(bkMonth, "202604171741320001_a.jpg"), []byte("FIRST"), 0o644)
	_ = os.WriteFile(filepath.Join(bkMonth, "202604171741320002_b.jpg"), []byte("SECOND"), 0o644)

	s := setupImageTestService(t, &mockImageConfig{dataDir: dataDir, backupPath: backupDir})
	s.md5PathCache[md5] = CachedMediaMeta{Talker: talker, Time: msgTime}

	w := doImageRequest(t, s, md5)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), []byte("FIRST")) {
		t.Errorf("expected FIRST (first match on collision), got %q", w.Body.String())
	}
}

// 场景 6: cache 有 meta + 仅 _t.dat + backup 未配置 → 返缩略图 (无 X-Backup-Hint, 因为有 meta)
func TestServeImage_BackupNotConfigured_ThumbnailNoHint(t *testing.T) {
	dataDir := t.TempDir()
	md5 := "nobackup00000000000000000000000a"
	thumbDir := filepath.Join(dataDir, "msg", "attach", "zz", "2026-04", "Img")
	_ = os.MkdirAll(thumbDir, 0o755)
	_ = os.WriteFile(filepath.Join(thumbDir, md5+"_t.dat"), []byte("x"), 0o644)

	s := setupImageTestService(t, &mockImageConfig{dataDir: dataDir, backupPath: ""})
	s.md5PathCache[md5] = CachedMediaMeta{
		Path:   filepath.Join("msg", "attach", "zz", "2026-04", "Img", md5),
		Talker: "foo@chatroom",
		Time:   time.Now(),
	}

	w := doImageRequest(t, s, md5)

	if got := s.backupStats.Thumbnail.Load(); got != 1 {
		t.Errorf("expected thumbnail=1, got %d", got)
	}
	if got := w.Header().Get("X-Image-Quality"); got != "thumbnail" {
		t.Errorf("expected X-Image-Quality=thumbnail, got %q", got)
	}
	if got := w.Header().Get("X-Backup-Hint"); got != "" {
		t.Errorf("expected no X-Backup-Hint (meta present), got %q", got)
	}
}

// 场景 7: cache 有 meta + 磁盘有原图 .dat → cache 严格查找命中 (不再用 recurse)
func TestServeImage_CacheStrictFindsOriginal(t *testing.T) {
	dataDir := t.TempDir()
	md5 := "abcdef0011223344556677889900aabb"
	origDir := filepath.Join(dataDir, "msg", "attach", "xx", "2026-04", "Img")
	_ = os.MkdirAll(origDir, 0o755)
	_ = os.WriteFile(filepath.Join(origDir, md5+".dat"), []byte("fake-encrypted"), 0o644)

	s := setupImageTestService(t, &mockImageConfig{dataDir: dataDir})
	// populate cache meta 指向磁盘上的 .dat
	s.md5PathCache[md5] = CachedMediaMeta{
		Path: filepath.Join("msg", "attach", "xx", "2026-04", "Img", md5),
	}

	_ = doImageRequest(t, s, md5)
	if got := s.backupStats.Cache.Load(); got != 1 {
		t.Errorf("expected cache=1, got %d; stats=%v", got, s.backupStats.snapshot())
	}
}

// 场景 8: TZ 敏感 - 指定 TZ 确保时间戳前缀格式稳定
func TestServeImage_BackupLookup_TZStability(t *testing.T) {
	t.Setenv("TZ", "Asia/Shanghai")
	// 构造 Unix 秒 1776418892 = 2026-04-17 17:41:32 +08:00
	unixSec := int64(1776418892)
	msgTime := time.Unix(unixSec, 0)

	dataDir := t.TempDir()
	backupDir := t.TempDir()
	md5 := "tzstable0000000000000000000000ab"
	talker := "tz@chatroom"

	bkMonth := filepath.Join(backupDir, "群("+talker+")", "2026-04")
	_ = os.MkdirAll(bkMonth, 0o755)
	// 用北京时间 17:41:32 前缀
	_ = os.WriteFile(filepath.Join(bkMonth, "20260417174132_x.jpg"), []byte("TZ_OK"), 0o644)

	s := setupImageTestService(t, &mockImageConfig{dataDir: dataDir, backupPath: backupDir})
	s.md5PathCache[md5] = CachedMediaMeta{Talker: talker, Time: msgTime}

	w := doImageRequest(t, s, md5)
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), []byte("TZ_OK")) {
		t.Errorf("TZ mismatch: code=%d body=%q", w.Code, w.Body.String())
	}
}
