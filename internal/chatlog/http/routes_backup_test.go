package http

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

// mockBackupConfig 实现 Config 接口，用于 backup 端点测试。
type mockBackupConfig struct {
	backupPath string
}

func (m *mockBackupConfig) GetHTTPAddr() string         { return ":5030" }
func (m *mockBackupConfig) GetDataDir() string          { return "" }
func (m *mockBackupConfig) GetSaveDecryptedMedia() bool { return false }
func (m *mockBackupConfig) GetBackupPath() string       { return m.backupPath }

// setupBackupTestRouter 创建一个用于测试的 gin 路由引擎。
func setupBackupTestRouter(conf Config) *Service {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	s := &Service{
		conf:         conf,
		router:       router,
		md5PathCache: make(map[string]string),
	}
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
