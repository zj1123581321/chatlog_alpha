package wechat

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// AutoDecryptPhase 是自动解密生命周期的显式状态。
//
// 状态机（Codex tension #2 "enable 语义谎言" 的解法）：
//
//	Idle ─(StartAutoDecrypt)─▶ Precheck ─(ok)──▶ FirstFull ─(done)──▶ Live
//	                              │                  │                  │
//	                              │(fail)            │(fail)            │(StopAutoDecrypt)
//	                              ▼                  ▼                  ▼
//	                            Failed             Failed            Stopping ─▶ Idle
//	                                                                    │
//	                                                                    (Switch)
//
// 消费者：
//   - TUI 状态栏根据 phase 决定显示什么（Stage G）
//   - HTTP /api/v1/autodecrypt/status 返回 phase（Stage H）
//   - HTTP 503 gate 仅在 phase==FirstFull 时挂（Stage H）
type AutoDecryptPhase string

const (
	// PhaseIdle 自动解密未启用，或上次已完成/已停。
	PhaseIdle AutoDecryptPhase = "idle"

	// PhasePrecheck 正在跑单文件预检验证密钥（秒级）。
	PhasePrecheck AutoDecryptPhase = "precheck"

	// PhaseFirstFull 首次全量解密运行中（可能 5-15 分钟，依账号数据量）。
	// HTTP 读数据接口在此阶段返 503，避免消费者看到跨库不一致。
	PhaseFirstFull AutoDecryptPhase = "first_full"

	// PhaseLive 已启动完毕，文件监听处理增量变更（稳态）。
	PhaseLive AutoDecryptPhase = "live"

	// PhaseFailed 预检或首次全量失败，SetAutoDecrypt(false)，等待用户重试。
	PhaseFailed AutoDecryptPhase = "failed"

	// PhaseStopping StopAutoDecrypt 进行中，等后台 goroutine 清理。
	PhaseStopping AutoDecryptPhase = "stopping"
)

// AutoDecryptLastRun 记录最近一次自动解密任务的摘要。
// HTTP /status 在 phase==Idle 时返回它，让运维能看到"上次跑了啥"而非空白。
type AutoDecryptLastRun struct {
	StartedAt    time.Time        `json:"started_at"`
	EndedAt      time.Time        `json:"ended_at"`
	DurationSecs float64          `json:"duration_s"`
	FinalPhase   AutoDecryptPhase `json:"final_phase"`
	FilesTotal   int              `json:"files_total,omitempty"`
	Error        string           `json:"error,omitempty"`
}

// phaseState 在 Service 内内嵌，保护 phase + lastRun 的并发访问。
// 用独立 RWMutex 而非 Service.mutex 避免和 decrypt 热路径相互阻塞。
type phaseState struct {
	mu      sync.RWMutex
	phase   AutoDecryptPhase
	lastRun *AutoDecryptLastRun
}

func newPhaseState() phaseState {
	return phaseState{phase: PhaseIdle}
}

// GetPhase 返回当前 phase（并发安全）。
func (s *Service) GetPhase() AutoDecryptPhase {
	s.phaseState.mu.RLock()
	defer s.phaseState.mu.RUnlock()
	return s.phaseState.phase
}

// SetPhase 原子更新 phase 并打日志。
func (s *Service) SetPhase(p AutoDecryptPhase) {
	s.phaseState.mu.Lock()
	old := s.phaseState.phase
	s.phaseState.phase = p
	s.phaseState.mu.Unlock()
	if old != p {
		log.Info().
			Str("from", string(old)).
			Str("to", string(p)).
			Msg("[autodecrypt] phase transition")
	}
}

// GetLastRun 返回上次运行摘要的快照副本（对 caller 的修改不影响内部状态）。
// 无上次运行时返回 nil。
func (s *Service) GetLastRun() *AutoDecryptLastRun {
	s.phaseState.mu.RLock()
	defer s.phaseState.mu.RUnlock()
	if s.phaseState.lastRun == nil {
		return nil
	}
	cp := *s.phaseState.lastRun
	return &cp
}

// setLastRun 内部使用，记录摘要。caller 应传入完整填好的 struct。
func (s *Service) setLastRun(r AutoDecryptLastRun) {
	s.phaseState.mu.Lock()
	defer s.phaseState.mu.Unlock()
	s.phaseState.lastRun = &r
}

// SpawnFirstFullDecrypt 以 fire-and-forget 方式跑首次全量解密。
//
// 这是 Stage G 的核心：UI 按钮按下 → 预检单文件秒级返回 → SpawnFirstFullDecrypt
// 后台跑全量，phase 进入 FirstFull。完成时自动设 Phase=Live + last_run 摘要；
// 失败时 Phase=Failed + 熔断 handler 触发。
//
// decryptFn 通常是 Manager.DecryptDBFiles —— 封装后可以让 wechat 包不依赖 manager。
//
// 调用前提：caller 已设 Phase=FirstFull。
// 并发保证：defer recover + wg 注册，Stop 能清理。
func (s *Service) SpawnFirstFullDecrypt(decryptFn func() error) {
	s.decryptWg.Add(1)
	go func() {
		defer s.decryptWg.Done()
		defer s.recoverDecryptPanic("firstFullDecrypt")

		started := time.Now()
		log.Info().Msg("[autodecrypt] firstFullDecrypt goroutine 启动")

		err := decryptFn()
		duration := time.Since(started)

		run := AutoDecryptLastRun{
			StartedAt:    started,
			EndedAt:      time.Now(),
			DurationSecs: duration.Seconds(),
		}

		if err != nil {
			run.FinalPhase = PhaseFailed
			run.Error = err.Error()
			s.setLastRun(run)
			s.SetPhase(PhaseFailed)
			log.Error().Err(err).Dur("duration", duration).
				Msg("[autodecrypt] firstFullDecrypt 失败，触发熔断")
			if s.errorHandler != nil {
				s.errorHandler(fmt.Errorf("首次全量解密失败: %w", err))
			}
			return
		}

		run.FinalPhase = PhaseLive
		s.setLastRun(run)
		s.SetPhase(PhaseLive)
		log.Info().Dur("duration", duration).
			Msg("[autodecrypt] firstFullDecrypt 完成，进入 Live phase")
	}()
}
