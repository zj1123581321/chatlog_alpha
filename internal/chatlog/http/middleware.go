package http

import (
	"net/http"
	"runtime"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/internal/chatlog/database"
)

// SlowRequestThreshold 是 slowRequestMiddleware 判定请求"慢"的阈值。
// 超过时会打 Warn 日志 + 抓当前 goroutine stack 头部，便于事后定位 hang。
var SlowRequestThreshold = 1 * time.Second

// slowRequestMiddleware 记录超过 SlowRequestThreshold 的 HTTP 请求，
// 并 dump 当前 goroutine 栈到日志中，用于复现偶发死锁/长锁场景时一击抓住现场。
// 写成独立 middleware 而不是扩展现有 gin.LoggerWithWriter，方便未来按需开关。
func slowRequestMiddleware(threshold time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		elapsed := time.Since(start)
		if elapsed < threshold {
			return
		}

		// 只抓前 8KB goroutine 栈，避免日志被冲爆；all=true 能看到所有 goroutine。
		buf := make([]byte, 8*1024)
		n := runtime.Stack(buf, true)
		log.Warn().
			Dur("elapsed", elapsed).
			Str("method", c.Request.Method).
			Str("path", c.Request.URL.Path).
			Int("status", c.Writer.Status()).
			Str("goroutine_stack_head", string(buf[:n])).
			Msg("slow HTTP request (>= threshold): goroutine dump attached")
	}
}

func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-CSRF-Token")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func (s *Service) checkDBStateMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch s.db.State {
		case database.StateInit:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is not ready"})
			c.Abort()
			return
		case database.StateDecrypting:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is decrypting, please wait"})
			c.Abort()
			return
		case database.StateError:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database is error: " + s.db.StateMsg})
			c.Abort()
			return
		}

		c.Next()
	}
}
