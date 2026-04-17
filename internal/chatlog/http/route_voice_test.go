package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// setupVoiceTestRouter 构建一个只含 voice 路由的最小测试路由，
// 直接喂给 serveAudioBytes 固定字节，避免依赖 silk 解码与数据库。
func setupVoiceTestRouter() (*gin.Engine, []byte) {
	gin.SetMode(gin.TestMode)
	router := gin.New()

	testBytes := make([]byte, 10000)
	for i := range testBytes {
		testBytes[i] = byte(i % 256)
	}

	handler := func(c *gin.Context) {
		serveAudioBytes(c, "audio/mp3", testBytes)
	}
	router.GET("/voice/:id", handler)
	router.HEAD("/voice/:id", handler)

	return router, testBytes
}

func TestVoiceGET_ReturnsContentLengthAndAcceptRanges(t *testing.T) {
	router, testBytes := setupVoiceTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/voice/abc", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got != "10000" {
		t.Errorf("Content-Length: want 10000, got %q", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges: want bytes, got %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "audio/mp3" {
		t.Errorf("Content-Type: want audio/mp3, got %q", got)
	}
	if w.Body.Len() != len(testBytes) {
		t.Errorf("body length: want %d, got %d", len(testBytes), w.Body.Len())
	}
}

func TestVoiceHEAD_SameHeadersNoBody(t *testing.T) {
	router, _ := setupVoiceTestRouter()

	req := httptest.NewRequest(http.MethodHead, "/voice/abc", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (HEAD must not redirect)", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got != "10000" {
		t.Errorf("Content-Length: want 10000, got %q", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges: want bytes, got %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "audio/mp3" {
		t.Errorf("Content-Type: want audio/mp3, got %q", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body: want empty, got %d bytes", w.Body.Len())
	}
}

func TestVoiceRange_Returns206WithContentRange(t *testing.T) {
	router, _ := setupVoiceTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/voice/abc", nil)
	req.Header.Set("Range", "bytes=0-999")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-999/10000" {
		t.Errorf("Content-Range: want \"bytes 0-999/10000\", got %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "1000" {
		t.Errorf("Content-Length: want 1000, got %q", got)
	}
	if w.Body.Len() != 1000 {
		t.Errorf("body length: want 1000, got %d", w.Body.Len())
	}
}

func TestVoiceRange_Suffix(t *testing.T) {
	router, _ := setupVoiceTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/voice/abc", nil)
	req.Header.Set("Range", "bytes=-500")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusPartialContent {
		t.Fatalf("expected 206, got %d", w.Code)
	}
	if w.Body.Len() != 500 {
		t.Errorf("suffix range body: want 500, got %d", w.Body.Len())
	}
}

func TestVoiceRange_Unsatisfiable(t *testing.T) {
	router, _ := setupVoiceTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/voice/abc", nil)
	req.Header.Set("Range", "bytes=999999-")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", w.Code)
	}
}
