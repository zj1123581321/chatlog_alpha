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

// AutoDecryptProgress 是首次全量解密进行中的进度快照（Stage M 新增）。
// 只在 phase==first_full 时返回；完成或 idle 时不展示（避免 UI 错位显示）。
type AutoDecryptProgress struct {
	FilesDone   int     `json:"files_done"`
	FilesTotal  int     `json:"files_total"`
	BytesDone   int64   `json:"bytes_done"`
	BytesTotal  int64   `json:"bytes_total"`
	Pct         float64 `json:"pct"`                    // 0-100
	CurrentFile string  `json:"current_file,omitempty"` // basename
	ElapsedSecs float64 `json:"elapsed_s"`
	ETA         string  `json:"eta,omitempty"` // "计算中..." / "42s" / "约 3 分钟"
}

// AutoDecryptStatus 是 /api/v1/autodecrypt/status 的响应体。
//
// 典型响应：
//
//	phase=first_full 进行中：
//	  {"enabled":true,"phase":"first_full",
//	   "progress":{"files_done":12,"files_total":42,"pct":28.5,
//	               "bytes_done":1234567,"bytes_total":4500000,
//	               "current_file":"message_3.db","elapsed_s":120,
//	               "eta":"约 5 分钟"}}
//
//	phase=idle 且有 last_run（Codex T4 决策：带 last_run 摘要）：
//	  {"enabled":false,"phase":"idle",
//	   "last_run":{"started_at":"2026-04-20T03:42:58Z",
//	               "duration_s":420,"final_phase":"live"}}
type AutoDecryptStatus struct {
	Enabled  bool                 `json:"enabled"`
	Phase    string               `json:"phase"`
	LastRun  *AutoDecryptLastRun  `json:"last_run,omitempty"`
	Progress *AutoDecryptProgress `json:"progress,omitempty"`
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
