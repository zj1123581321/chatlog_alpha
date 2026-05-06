package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// healthzStaleThreshold 是 /healthz 容忍的"上次自动解密结束"年龄上限。
// 超过这个时间没有成功的解密 cycle, /healthz 返 503 让监控告警。
//
// 选 30min 是因为:
//   - 用户接受的最大数据延迟是 15min (轮询间隔上限)
//   - 给 1 次失败 + retry 留余量, 30min ≈ 2 次轮询 + 一些抖动
//   - 远小于"死了 9 天没人知道" (4/25 → 5/05 silent failure 时长)
//
// 旧 /health 端点 (无 z, 上级 route.go) 永远返回 200 ok 不检查任何业务状态,
// 是 silent failure 的根源之一。/healthz 是它的"真实健康"版本。
const healthzStaleThreshold = 30 * time.Minute

// healthzResponseDTO 是 /healthz 的响应 schema。
// status 是 stable contract, 给监控/告警系统消费:
//   - "ok"          一切正常
//   - "warming_up"  自动解密 first_full 进行中, 数据未完整, 但 server 已接受请求
//   - "unhealthy"   有问题, 详情见 reason
type healthzResponseDTO struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// initHealthRouter 注册 /healthz 端点。
// 跟旧 /health (永远 200 ok) 共存, /health 用于负载均衡那种"端口活着就行"
// 的简单 probe; /healthz 是真实业务健康度。
func (s *Service) initHealthRouter() {
	s.router.GET("/healthz", s.handleHealthz)
}

// handleHealthz 评估业务健康度。
//
// 决策树:
//  1. 没注入 autoDecryptStatusFn → 200 ok (server 子命令 / 测试场景)
//  2. status.Phase == "first_full" → 200 warming_up (启动期, 还没追完)
//  3. status.LastRun == nil → 200 ok (idle, 没跑过, 不能误判)
//  4. status.LastRun.FinalPhase == "error" → 503 (上次跑炸了)
//  5. now - LastRun.EndedAt > healthzStaleThreshold → 503 (太久没成功 cycle)
//  6. else → 200 ok
//
// 故意不去查 SQLite ping —— database.Service 的 State 检查已经由
// checkDBStateMiddleware 覆盖, /healthz 不重复; 主要监控的是 "watcher 还在
// 追新数据吗"。SQLite 死锁是另一个 layer 的问题, 由 watchdog (后续 step) 处理。
func (s *Service) handleHealthz(c *gin.Context) {
	if s.autoDecryptStatusFn == nil {
		c.JSON(http.StatusOK, healthzResponseDTO{Status: "ok"})
		return
	}

	st := s.autoDecryptStatusFn()

	if st.Phase == "first_full" {
		c.JSON(http.StatusOK, healthzResponseDTO{
			Status: "warming_up",
			Reason: "first full decrypt in progress",
		})
		return
	}

	if st.LastRun == nil {
		c.JSON(http.StatusOK, healthzResponseDTO{Status: "ok"})
		return
	}

	if st.LastRun.FinalPhase == "error" {
		c.JSON(http.StatusServiceUnavailable, healthzResponseDTO{
			Status: "unhealthy",
			Reason: "last decrypt cycle failed: " + st.LastRun.Error,
		})
		return
	}

	endedAt, err := time.Parse(time.RFC3339, st.LastRun.EndedAt)
	if err != nil {
		// 时间戳无法解析视为 unhealthy —— 大概率是 manager 注入端 bug,
		// 倾向 fail-loud 让我们及时发现, 而不是 silent 200。
		c.JSON(http.StatusServiceUnavailable, healthzResponseDTO{
			Status: "unhealthy",
			Reason: "invalid last_run.ended_at: " + st.LastRun.EndedAt,
		})
		return
	}

	age := time.Since(endedAt)
	if age > healthzStaleThreshold {
		c.JSON(http.StatusServiceUnavailable, healthzResponseDTO{
			Status: "unhealthy",
			Reason: "last decrypt cycle was " + age.Round(time.Second).String() + " ago (threshold " + healthzStaleThreshold.String() + ")",
		})
		return
	}

	c.JSON(http.StatusOK, healthzResponseDTO{Status: "ok"})
}
