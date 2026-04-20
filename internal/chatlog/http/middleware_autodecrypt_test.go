package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newTestRouter(phaseFn func() string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1", firstFullGateMiddleware(phaseFn))
	api.GET("/chatlog", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	api.GET("/autodecrypt/status", func(c *gin.Context) { c.JSON(200, gin.H{"phase": "test"}) })
	api.POST("/cache/clear", func(c *gin.Context) { c.JSON(200, gin.H{"cleared": true}) })
	return r
}

func TestFirstFullGate_IdlePhase_PassesThrough(t *testing.T) {
	r := newTestRouter(func() string { return "idle" })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/chatlog", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("idle phase should allow /chatlog, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "" {
		t.Errorf("should not set Retry-After in idle, got %q", w.Header().Get("Retry-After"))
	}
}

func TestFirstFullGate_FirstFullPhase_Returns503OnDataEndpoint(t *testing.T) {
	r := newTestRouter(func() string { return "first_full" })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/chatlog", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("first_full should return 503 on /chatlog, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") != "30" {
		t.Errorf("Retry-After should be '30', got %q", w.Header().Get("Retry-After"))
	}
	if w.Header().Get("X-Autodecrypt-Hint") != "first-full-in-progress" {
		t.Errorf("X-Autodecrypt-Hint should be 'first-full-in-progress', got %q",
			w.Header().Get("X-Autodecrypt-Hint"))
	}

	// 响应 body 应该是 JSON 且包含 phase
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response should be valid JSON: %v", err)
	}
	if body["phase"] != "first_full" {
		t.Errorf("body.phase = %v, want 'first_full'", body["phase"])
	}
	if body["retry_after_seconds"] == nil {
		t.Error("body should contain retry_after_seconds")
	}
}

func TestFirstFullGate_StatusEndpoint_ExemptFromGate(t *testing.T) {
	// /api/v1/autodecrypt/status 必须在 first_full 期间仍可访问
	r := newTestRouter(func() string { return "first_full" })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/autodecrypt/status", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("/autodecrypt/status should be exempt from gate, got %d", w.Code)
	}
}

func TestFirstFullGate_CacheClear_ExemptFromGate(t *testing.T) {
	r := newTestRouter(func() string { return "first_full" })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/cache/clear", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("/cache/clear should be exempt from gate, got %d", w.Code)
	}
}

func TestFirstFullGate_NilPhaseFn_PassesThrough(t *testing.T) {
	// 测试 / server 子命令场景：phaseFn 未注入，middleware 应直通
	r := newTestRouter(nil)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/chatlog", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("nil phaseFn should allow all requests, got %d", w.Code)
	}
}

func TestFirstFullGate_LivePhase_PassesThrough(t *testing.T) {
	// 稳态 live 不应该 gate
	r := newTestRouter(func() string { return "live" })

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/chatlog", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Errorf("live phase should pass through, got %d", w.Code)
	}
}
