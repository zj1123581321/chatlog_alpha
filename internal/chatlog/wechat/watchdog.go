package wechat

// watchdog.go：Step 6 phase-aware watchdog（architecture-rework-2026-05-06.md
// §1.2.7 + Eng Review Lock A4）。
//
// 监控主循环 tick 频率，超时则 self-kill 让外部 supervisor 接管：
//   - 主循环每 iteration 起头调 w.Tick() 更新 lastTickNs。
//   - watchdog 后台 goroutine 每 CheckEvery（默认 30s）调 w.Check()。
//   - 检查 (now - lastTickNs) > timeoutForPhase()：
//       * PhaseFirstFull → 60min（首次全量解密 4.9 GB × 50 db 实测 30min+，
//         单一 5min timeout 会反复误触发）
//       * 其他 phase     → 5min（polling cycle、idle 不该卡这么久）
//   - breach → Exit(1)，supervisor 30s 后重启进程，状态从 status.json 恢复。
//
// 为什么独立 goroutine + LockOSThread：SQLite 解密是 cgo 调用，长时间占着
// GOMAXPROCS thread；如果 watchdog goroutine 跟它们抢 thread，可能拿不到调度。
// LockOSThread 把 watchdog 钉死在自己的 OS thread 上，不与 cgo runtime 互相挤。

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// 默认值与 spec §A4 对齐。
const (
	DefaultWatchdogCheckEvery     = 30 * time.Second
	DefaultWatchdogTimeoutLive    = 5 * time.Minute
	DefaultWatchdogTimeoutFirst   = 60 * time.Minute
)

// Watchdog 监控主循环 tick；breach 触发 Exit。
//
// 字段在 Start 之前由调用方填好；Start 之后只读（除内部状态）。
type Watchdog struct {
	// PhaseFn 返回当前 phase 让 watchdog 选超时阈值。nil → 永远按 Live timeout。
	PhaseFn func() AutoDecryptPhase

	// CheckEvery：watchdog 主循环 ticker 周期。0 → DefaultWatchdogCheckEvery。
	CheckEvery time.Duration

	// TimeoutLive / TimeoutFirstFull：超时阈值。0 → defaults。
	TimeoutLive      time.Duration
	TimeoutFirstFull time.Duration

	// Exit 是 self-kill 的注入点，默认 os.Exit。测试用来观测而不真退进程。
	Exit func(code int)
	// Now 注入测试时钟。0 → time.Now。
	Now func() time.Time

	// afterCheck 是测试 hook，每次 Check 完调用一下，让 Start/Stop 测试能可观察 ticker 在跑。
	// 生产代码不设，约定不暴露在公共 API。
	afterCheck func()

	lastTickNs int64

	mu      sync.Mutex
	running bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// Tick 由主循环每 iteration 起头调用，更新最近 tick 时间戳。
//
// 多 goroutine 同时 Tick 是安全的（atomic.Store）；watchdog 也用 atomic.Load 读。
func (w *Watchdog) Tick() {
	now := w.now()
	atomic.StoreInt64(&w.lastTickNs, now.UnixNano())
}

// Check 同步执行一次"读 lastTick + 跟阈值比 + 必要时 Exit"。
// Start 起的后台 ticker 跑这个方法；测试可以直接调它而不启动 goroutine。
func (w *Watchdog) Check() {
	defer func() {
		if w.afterCheck != nil {
			w.afterCheck()
		}
	}()
	last := atomic.LoadInt64(&w.lastTickNs)
	if last == 0 {
		// 还没 Tick 过：可能在 Init 阶段。给主循环留出启动时间。
		return
	}
	age := w.now().Sub(time.Unix(0, last))
	timeout := w.timeoutForPhase()
	if age > timeout {
		log.Error().
			Dur("age", age).
			Dur("timeout", timeout).
			Str("phase", string(w.currentPhase())).
			Msg("[watchdog] main loop hang detected, exiting (supervisor restart in 30s)")
		w.exit(1)
	}
}

// Start 起后台 goroutine，每 CheckEvery 调一次 Check()。Goroutine 内部
// runtime.LockOSThread 防止 cgo 抢光所有 OS thread 时 watchdog 拿不到调度。
//
// 双 Start 返回错误。
func (w *Watchdog) Start() error {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return errors.New("watchdog: already running")
	}
	w.running = true
	w.stopCh = make(chan struct{})
	w.doneCh = make(chan struct{})
	w.mu.Unlock()

	go w.loop()
	return nil
}

// Stop 通知 loop 退出并等它结束。幂等。
func (w *Watchdog) Stop() error {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return nil
	}
	w.running = false
	close(w.stopCh)
	doneCh := w.doneCh
	w.mu.Unlock()
	<-doneCh
	return nil
}

func (w *Watchdog) loop() {
	defer close(w.doneCh)
	// LockOSThread：把本 goroutine 钉到独占的 OS thread，避免 cgo 调用挤占所有
	// runtime thread 时 watchdog 拿不到执行机会。defer Unlock 是良好实践，
	// 但 goroutine 退出时即便不 Unlock 也不影响（Go 1.10+ thread 会被销毁）。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	interval := w.CheckEvery
	if interval <= 0 {
		interval = DefaultWatchdogCheckEvery
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-tick.C:
			w.Check()
		}
	}
}

func (w *Watchdog) timeoutForPhase() time.Duration {
	live := w.TimeoutLive
	if live <= 0 {
		live = DefaultWatchdogTimeoutLive
	}
	first := w.TimeoutFirstFull
	if first <= 0 {
		first = DefaultWatchdogTimeoutFirst
	}
	switch w.currentPhase() {
	case PhaseFirstFull:
		return first
	default:
		return live
	}
}

func (w *Watchdog) currentPhase() AutoDecryptPhase {
	if w.PhaseFn == nil {
		return PhaseLive
	}
	return w.PhaseFn()
}

func (w *Watchdog) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watchdog) exit(code int) {
	if w.Exit != nil {
		w.Exit(code)
		return
	}
	// 默认走 os.Exit；用包级别函数让测试可以全局替换（虽然主推注入 Exit 字段）。
	defaultExit(code)
}
