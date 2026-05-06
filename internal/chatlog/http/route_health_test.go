package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// healthzResponse 是 /healthz 接口契约。stable JSON 给监控系统消费。
type healthzResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// makeHealthRouter 构造一个最小 Service 实例（只关心 /healthz 路由）。
// 不启 backupIndex / database / mcp，避免拉重依赖。
func makeHealthRouter(t *testing.T) (*Service, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	s := &Service{router: r}
	s.initHealthRouter()
	return s, r
}

// TestHealthz_OKWhenNoStatusFn 没注入 status fn 时 /healthz 返回 200 ok。
// 用于 server 子命令 / 测试场景，不阻塞启动。
func TestHealthz_OKWhenNoStatusFn(t *testing.T) {
	_, r := makeHealthRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want=200", w.Code)
	}

	var got healthzResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("Status=%q want=ok", got.Status)
	}
}

// TestHealthz_503WhenAutoDecryptStaleLastRun 自动解密上次 run 已过 30min,
// 说明 watcher 没在追新数据, 应该返 503 让监控告警。
func TestHealthz_503WhenAutoDecryptStaleLastRun(t *testing.T) {
	s, r := makeHealthRouter(t)
	staleTime := time.Now().Add(-31 * time.Minute).UTC().Format(time.RFC3339)
	s.SetAutoDecryptStatusFunc(func() AutoDecryptStatus {
		return AutoDecryptStatus{
			Enabled: true,
			Phase:   "live",
			LastRun: &AutoDecryptLastRun{
				StartedAt:    staleTime,
				EndedAt:      staleTime,
				DurationSecs: 5,
				FinalPhase:   "live",
			},
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale last_run should give 503, got status=%d", w.Code)
	}

	var got healthzResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Status == "ok" {
		t.Errorf("status must not be 'ok' when stale, got=%q reason=%q", got.Status, got.Reason)
	}
}

// TestHealthz_OKWhenAutoDecryptFresh autodecrypt 上次 run 在 30min 内, 应该 ok。
func TestHealthz_OKWhenAutoDecryptFresh(t *testing.T) {
	s, r := makeHealthRouter(t)
	freshTime := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	s.SetAutoDecryptStatusFunc(func() AutoDecryptStatus {
		return AutoDecryptStatus{
			Enabled: true,
			Phase:   "live",
			LastRun: &AutoDecryptLastRun{
				StartedAt:    freshTime,
				EndedAt:      freshTime,
				DurationSecs: 5,
				FinalPhase:   "live",
			},
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("fresh last_run should give 200, got status=%d", w.Code)
	}
}

// TestHealthz_OKDuringFirstFull autodecrypt 处于 first_full 阶段时, /healthz
// 应该 200 (warming up), 但响应 status 应明示, 让监控可以识别这种 "已启动但还没追完"
// 的窗口。
func TestHealthz_OKDuringFirstFull(t *testing.T) {
	s, r := makeHealthRouter(t)
	s.SetAutoDecryptStatusFunc(func() AutoDecryptStatus {
		return AutoDecryptStatus{
			Enabled: true,
			Phase:   "first_full",
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("first_full should give 200 (warming up), got status=%d", w.Code)
	}

	var got healthzResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Status != "warming_up" {
		t.Errorf("first_full Status=%q want=warming_up", got.Status)
	}
}

// TestHealthz_OKWhenIdleNoLastRun autodecrypt 没启用过 (idle 且无 last_run),
// 视为 ok —— 这种场景代表 server 刚启动, 还没触发过自动解密, 不应误报。
func TestHealthz_OKWhenIdleNoLastRun(t *testing.T) {
	s, r := makeHealthRouter(t)
	s.SetAutoDecryptStatusFunc(func() AutoDecryptStatus {
		return AutoDecryptStatus{
			Enabled: false,
			Phase:   "idle",
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("idle without last_run should give 200, got status=%d", w.Code)
	}
}

// TestHealthz_503WhenAutoDecryptError 上次 run 报错, /healthz 应该 503,
// 即使时间戳是新的。
func TestHealthz_503WhenAutoDecryptError(t *testing.T) {
	s, r := makeHealthRouter(t)
	freshTime := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	s.SetAutoDecryptStatusFunc(func() AutoDecryptStatus {
		return AutoDecryptStatus{
			Enabled: true,
			Phase:   "idle",
			LastRun: &AutoDecryptLastRun{
				StartedAt:    freshTime,
				EndedAt:      freshTime,
				DurationSecs: 1,
				FinalPhase:   "error",
				Error:        "decrypt key invalid",
			},
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("last_run with error should give 503, got status=%d", w.Code)
	}
}

// TestHealthz_StaleThresholdConfigurable 验证常量在合理范围。
// 如果改成 < 5min 会跟 watchdog tick 周期冲突，> 1h 失去意义。
func TestHealthz_StaleThresholdInRange(t *testing.T) {
	if healthzStaleThreshold < 5*time.Minute {
		t.Errorf("threshold too tight: %v", healthzStaleThreshold)
	}
	if healthzStaleThreshold > time.Hour {
		t.Errorf("threshold too loose: %v", healthzStaleThreshold)
	}
}
