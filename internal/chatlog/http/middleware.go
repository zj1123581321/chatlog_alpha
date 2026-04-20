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

// firstFullGateSkipPaths 是不受 503 gate 影响的路径白名单。
// - /api/v1/autodecrypt/status：查询 gate 自身状态的端点，必须始终可用
// - /api/v1/cache/clear：管理动作，和数据一致性无关
var firstFullGateSkipPaths = map[string]struct{}{
	"/api/v1/autodecrypt/status": {},
	"/api/v1/cache/clear":        {},
}

// firstFullGateMiddleware 在自动解密首次全量期间 (phase=="first_full") 对读数据
// 接口返 503 + Retry-After + X-Autodecrypt-Hint，避免 HTTP 消费者看到跨 db
// 新旧混合的不一致时间线（Codex outside voice Tension #1）。
//
// phaseFn 为 nil 时（测试场景或 server 子命令）直通。
// white-listed paths (firstFullGateSkipPaths) 也直通。
//
// 挂在 /api/v1 group 上。/image/* /voice/* 等 media 路由不挂 —— 图片 md5 查得到
// 就返回部分数据（hardlink + backup 兜底），查不到 404，没有跨 db 一致性问题。
func firstFullGateMiddleware(phaseFn func() string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, skip := firstFullGateSkipPaths[c.Request.URL.Path]; skip {
			c.Next()
			return
		}
		if phaseFn == nil {
			c.Next()
			return
		}
		if phaseFn() != "first_full" {
			c.Next()
			return
		}
		c.Header("Retry-After", "30")
		c.Header("X-Autodecrypt-Hint", "first-full-in-progress")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":               "chatlog is running first full decrypt; data not yet complete",
			"retry_after_seconds": 30,
			"phase":               "first_full",
		})
		c.Abort()
	}
}

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
