package http

import (
	"net"
	"net/http"
	"net/http/pprof"

	"github.com/gin-gonic/gin"
)

// initPprofRouter 在 /debug/pprof/* 下挂载标准库 net/http/pprof handler，
// 仅允许本地访问（127.0.0.1 / ::1 / 未路由的 LL / loopback）。
// 用途：出现 hang 时 `curl http://127.0.0.1:5030/debug/pprof/goroutine?debug=2`
// 抓栈定位哪条路径卡住。远端 IP 直接返回 403，避免暴露到 LAN / Tailscale。
func (s *Service) initPprofRouter() {
	debug := s.router.Group("/debug/pprof", pprofLocalhostOnly())
	debug.GET("/", gin.WrapF(pprof.Index))
	debug.GET("/cmdline", gin.WrapF(pprof.Cmdline))
	debug.GET("/profile", gin.WrapF(pprof.Profile))
	debug.GET("/symbol", gin.WrapF(pprof.Symbol))
	debug.POST("/symbol", gin.WrapF(pprof.Symbol))
	debug.GET("/trace", gin.WrapF(pprof.Trace))
	// allocs / block / goroutine / heap / mutex / threadcreate 等命名 profile
	debug.GET("/:name", func(c *gin.Context) {
		pprof.Handler(c.Param("name")).ServeHTTP(c.Writer, c.Request)
	})
}

// pprofLocalhostOnly 拒绝非回环 IP 的调用。c.ClientIP() 会尊重
// X-Forwarded-For，但 service.go 里已 SetTrustedProxies(nil)，所以这里拿到的
// 就是 RemoteAddr 的 IP。
func pprofLocalhostOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := net.ParseIP(c.ClientIP())
		if ip == nil || !ip.IsLoopback() {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "pprof: loopback only"})
			return
		}
		c.Next()
	}
}
