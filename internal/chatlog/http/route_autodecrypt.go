package http

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AutoDecryptLastRun 是 /api/v1/autodecrypt/status 中 last_run 摘要的 DTO。
// 与 wechat.AutoDecryptLastRun 独立，避免 http 包反向依赖 wechat 包；由 manager
// 层在注入 StatusGetter 时做 string 化转换（时间戳 RFC3339）。
type AutoDecryptLastRun struct {
	StartedAt    string  `json:"started_at"`
	EndedAt      string  `json:"ended_at"`
	DurationSecs float64 `json:"duration_s"`
	FinalPhase   string  `json:"final_phase"`
	Error        string  `json:"error,omitempty"`
}

// AutoDecryptStatus 是 /api/v1/autodecrypt/status 的响应体。
//
// 典型响应：
//
//	phase=first_full 时：
//	  {"enabled":true, "phase":"first_full"}  ← 首次全量进行中
//	phase=idle 且有 last_run 时（Codex T4 决策：带 last_run 摘要）：
//	  {"enabled":false, "phase":"idle",
//	   "last_run":{"started_at":"2026-04-20T03:42:58Z",
//	               "duration_s":420, "final_phase":"live"}}
type AutoDecryptStatus struct {
	Enabled bool                `json:"enabled"`
	Phase   string              `json:"phase"`
	LastRun *AutoDecryptLastRun `json:"last_run,omitempty"`
}

// AutoDecryptStatusGetter 由 manager 注入，动态返回当前快照。
type AutoDecryptStatusGetter func() AutoDecryptStatus

// SetAutoDecryptStatusFunc 注入 status 查询闭包。nil 时 /status 返回 idle 默认值。
func (s *Service) SetAutoDecryptStatusFunc(fn AutoDecryptStatusGetter) {
	s.autoDecryptStatusFn = fn
}

// handleAutoDecryptStatus 返回自动解密当前状态快照。
// 这个接口通过 firstFullGateMiddleware 的 skipPaths 白名单豁免，在 first_full
// 期间仍然可访问 —— 这是运维查 "现在到底卡在哪" 的窗口。
func (s *Service) handleAutoDecryptStatus(c *gin.Context) {
	if s.autoDecryptStatusFn == nil {
		c.JSON(http.StatusOK, AutoDecryptStatus{
			Enabled: false,
			Phase:   "idle",
		})
		return
	}
	c.JSON(http.StatusOK, s.autoDecryptStatusFn())
}
