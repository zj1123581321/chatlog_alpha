package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// mockBackupConfig 实现 Config 接口，用于 backup 端点测试。
type mockBackupConfig struct {
	backupPath      string
	backupFolderMap map[string]string
}

func (m *mockBackupConfig) GetHTTPAddr() string                     { return ":5030" }
func (m *mockBackupConfig) GetDataDir() string                      { return "" }
func (m *mockBackupConfig) GetSaveDecryptedMedia() bool             { return false }
func (m *mockBackupConfig) GetBackupPath() string                   { return m.backupPath }
func (m *mockBackupConfig) GetBackupFolderMap() map[string]string   { return m.backupFolderMap }

// setupBackupTestRouter 创建一个用于测试的 gin 路由引擎。
func setupBackupTestRouter(conf Config) *Service {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &Service{
		conf:         conf,
		router:       router,
		md5PathCache: make(map[string]CachedMediaMeta),
		backupIndex:  NewBackupIndex(conf.GetBackupPath(), conf.GetBackupFolderMap()),
	}
	_ = s.backupIndex.Scan()
	s.initBackupRouter()
	return s
}

func TestBackupNotConfigured_Returns404(t *testing.T) {
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: ""})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/image?folder_id=AABBCCDD&date=2026-04&time=20260407232646", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestBackupMissingParams_Returns400(t *testing.T) {
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: "/tmp/backup"})

	tests := []struct {
		name string
		url  string
	}{
		{"missing folder_id", "/api/v1/backup/image?date=2026-04&time=20260407232646"},
		{"missing date", "/api/v1/backup/image?folder_id=AABBCCDD&time=20260407232646"},
		{"missing time", "/api/v1/backup/image?folder_id=AABBCCDD&date=2026-04"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.url, nil)
			w := httptest.NewRecorder()
			s.router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestBackupInvalidFolderID_Returns400(t *testing.T) {
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: "/tmp/backup"})

	tests := []struct {
		name     string
		folderID string
	}{
		{"too short", "AABB"},
		{"non-hex chars", "GGHHIIJJ"},
		{"too long", "AABBCCDDEE"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/image?folder_id="+tc.folderID+"&date=2026-04&time=20260407232646", nil)
			w := httptest.NewRecorder()
			s.router.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d", w.Code)
			}
		})
	}
}

func TestBackupFolderNotFound_Returns404(t *testing.T) {
	tmpDir := t.TempDir()
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: tmpDir})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/image?folder_id=AABBCCDD&date=2026-04&time=20260407232646", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestBackupImageNotFound_Returns404(t *testing.T) {
	tmpDir := t.TempDir()
	// 创建匹配的文件夹和月份目录，但不放图片
	folderDir := filepath.Join(tmpDir, "测试群(AABBCCDD)", "2026-04")
	if err := os.MkdirAll(folderDir, 0o755); err != nil {
		t.Fatal(err)
	}

	s := setupBackupTestRouter(&mockBackupConfig{backupPath: tmpDir})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/image?folder_id=AABBCCDD&date=2026-04&time=20260407232646", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestBackupStats_InitialZero(t *testing.T) {
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: ""})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/stats", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// 初始所有计数器都是 0, 索引也是空
	for _, want := range []string{
		`"hardlink":0`,
		`"cache":0`,
		`"recurse":0`,
		`"backup":0`,
		`"thumbnail":0`,
		`"not_found":0`,
		`"chatroom_mode":0`,
		`"hex_mode":0`,
		`"unknown":0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q, got %s", want, body)
		}
	}
}

func TestBackupStats_ReflectsCounters(t *testing.T) {
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: ""})
	// 手动模拟几次命中
	s.backupStats.inc("hardlink")
	s.backupStats.inc("hardlink")
	s.backupStats.inc("backup")
	s.backupStats.inc("thumbnail")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/stats", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"hardlink":2`) {
		t.Errorf("hardlink=2 missing, body=%s", body)
	}
	if !strings.Contains(body, `"backup":1`) {
		t.Errorf("backup=1 missing, body=%s", body)
	}
	if !strings.Contains(body, `"thumbnail":1`) {
		t.Errorf("thumbnail=1 missing, body=%s", body)
	}
}

func TestBackupStats_IndexCountsReported(t *testing.T) {
	tmp := t.TempDir()
	// 一个 chatroom + 一个 hex + 一个 unknown
	for _, name := range []string{"群A(x@chatroom)", "群B(AABBCCDD)", "未知"} {
		if err := os.MkdirAll(filepath.Join(tmp, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := setupBackupTestRouter(&mockBackupConfig{backupPath: tmp})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/stats", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		`"chatroom_mode":1`,
		`"hex_mode":1`,
		`"unknown":1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body, got %s", want, body)
		}
	}
}

func TestBackupSuccess_Returns200(t *testing.T) {
	tmpDir := t.TempDir()
	// 创建完整的目录结构和测试图片
	imgDir := filepath.Join(tmpDir, "测试群(AABBCCDD)", "2026-04")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	imgPath := filepath.Join(imgDir, "20260407232646_test.jpg")
	if err := os.WriteFile(imgPath, []byte("fake-jpg-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := setupBackupTestRouter(&mockBackupConfig{backupPath: tmpDir})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/backup/image?folder_id=AABBCCDD&date=2026-04&time=20260407232646", nil)
	w := httptest.NewRecorder()
	s.router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}
