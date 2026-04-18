package http

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// withCapturedLog 把 zerolog 的全局 logger 重定向到 buf，测试结束恢复。
func withCapturedLog(t *testing.T, fn func(buf *bytes.Buffer)) {
	t.Helper()
	var buf bytes.Buffer
	original := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)
	defer func() { log.Logger = original }()
	fn(&buf)
}

// TestSlowRequestMiddleware_FastBelowThreshold 快请求不打 Warn。
func TestSlowRequestMiddleware_FastBelowThreshold(t *testing.T) {
	withCapturedLog(t, func(buf *bytes.Buffer) {
		r := gin.New()
		r.Use(slowRequestMiddleware(200 * time.Millisecond))
		r.GET("/fast", func(c *gin.Context) { c.String(200, "ok") })

		req := httptest.NewRequest(http.MethodGet, "/fast", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != 200 {
			t.Fatalf("status=%d want=200", w.Code)
		}
		if strings.Contains(buf.String(), "slow HTTP request") {
			t.Errorf("fast request must not trigger slow log, got: %s", buf.String())
		}
	})
}

// TestSlowRequestMiddleware_DumpsStackOnSlow 超阈值必须打 Warn 并附 goroutine 栈。
func TestSlowRequestMiddleware_DumpsStackOnSlow(t *testing.T) {
	withCapturedLog(t, func(buf *bytes.Buffer) {
		r := gin.New()
		r.Use(slowRequestMiddleware(20 * time.Millisecond))
		r.GET("/slow", func(c *gin.Context) {
			time.Sleep(80 * time.Millisecond)
			c.String(200, "ok")
		})

		req := httptest.NewRequest(http.MethodGet, "/slow", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		out := buf.String()
		if !strings.Contains(out, "slow HTTP request") {
			t.Errorf("expected slow log; got: %s", out)
		}
		if !strings.Contains(out, "goroutine_stack_head") {
			t.Errorf("expected goroutine stack in log; got: %s", out)
		}
		if !strings.Contains(out, "\"path\":\"/slow\"") {
			t.Errorf("expected path field; got: %s", out)
		}
	})
}

// TestPprofLocalhostOnly_RejectsRemote 非回环 IP 返回 403。
func TestPprofLocalhostOnly_RejectsRemote(t *testing.T) {
	r := gin.New()
	r.Use(pprofLocalhostOnly())
	r.GET("/debug/pprof/", func(c *gin.Context) { c.String(200, "pprof") })

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.RemoteAddr = "192.168.1.10:54321"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("remote IP must be forbidden, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestPprofLocalhostOnly_AllowsLoopback 回环 IP 放行。
func TestPprofLocalhostOnly_AllowsLoopback(t *testing.T) {
	r := gin.New()
	r.Use(pprofLocalhostOnly())
	r.GET("/debug/pprof/", func(c *gin.Context) { c.String(200, "pprof") })

	for _, addr := range []string{"127.0.0.1:1", "[::1]:1"} {
		req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("loopback %s must pass, got %d", addr, w.Code)
		}
	}
}
