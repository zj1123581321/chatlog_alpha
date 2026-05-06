package wechat

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchdog_TickWithinTimeout_NoExit(t *testing.T) {
	var exitCalled int32
	now := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	w := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseLive },
		Now:     func() time.Time { return now },
		Exit:    func(int) { atomic.StoreInt32(&exitCalled, 1) },
	}
	w.Tick()
	now = now.Add(2 * time.Minute) // tick 后 2min 检查 — 5min 限内
	w.Check()
	if atomic.LoadInt32(&exitCalled) != 0 {
		t.Errorf("watchdog should not exit within timeout")
	}
}

func TestWatchdog_TickExceedsLiveTimeout_Exits(t *testing.T) {
	var exitCalled int32
	now := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	w := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseLive },
		Now:     func() time.Time { return now },
		Exit:    func(int) { atomic.StoreInt32(&exitCalled, 1) },
	}
	w.Tick()
	now = now.Add(6 * time.Minute) // > 5min Live 超时
	w.Check()
	if atomic.LoadInt32(&exitCalled) != 1 {
		t.Errorf("watchdog should exit after Live timeout")
	}
}

func TestWatchdog_FirstFullHasLongerTimeout(t *testing.T) {
	var exitCalled int32
	now := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	w := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseFirstFull },
		Now:     func() time.Time { return now },
		Exit:    func(int) { atomic.StoreInt32(&exitCalled, 1) },
	}
	w.Tick()
	now = now.Add(30 * time.Minute) // 30min — Live 早就超了，FirstFull 还没
	w.Check()
	if atomic.LoadInt32(&exitCalled) != 0 {
		t.Errorf("watchdog should NOT exit within FirstFull 60min: 30min < 60min")
	}

	now = now.Add(35 * time.Minute) // 累计 65min > 60min
	w.Check()
	if atomic.LoadInt32(&exitCalled) != 1 {
		t.Errorf("watchdog should exit after FirstFull 60min timeout")
	}
}

func TestWatchdog_NoTickYet_NoExit(t *testing.T) {
	var exitCalled int32
	w := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseLive },
		Now:     time.Now,
		Exit:    func(int) { atomic.StoreInt32(&exitCalled, 1) },
	}
	// 从未 Tick → Check 不应触发 exit（程序可能还在 Init 阶段）
	w.Check()
	if atomic.LoadInt32(&exitCalled) != 0 {
		t.Errorf("watchdog should not exit before first Tick")
	}
}

func TestWatchdog_NilPhaseFn_DefaultsToLiveTimeout(t *testing.T) {
	var exitCalled int32
	now := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	w := &Watchdog{
		PhaseFn: nil, // 默认行为
		Now:     func() time.Time { return now },
		Exit:    func(int) { atomic.StoreInt32(&exitCalled, 1) },
	}
	w.Tick()
	now = now.Add(6 * time.Minute)
	w.Check()
	if atomic.LoadInt32(&exitCalled) != 1 {
		t.Errorf("expected default 5min timeout to fire")
	}
}

func TestWatchdog_StartStop(t *testing.T) {
	var checks int32
	w := &Watchdog{
		PhaseFn:    func() AutoDecryptPhase { return PhaseLive },
		Now:        time.Now,
		Exit:       func(int) {},
		CheckEvery: 5 * time.Millisecond,
		// 注入观察 hook：每次 check 完后 +1
		afterCheck: func() { atomic.AddInt32(&checks, 1) },
	}
	w.Tick() // 让 Check 不会因为 lastTickNs=0 早退
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(40 * time.Millisecond)
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if atomic.LoadInt32(&checks) < 2 {
		t.Errorf("expected ≥2 ticks of check loop, got %d", checks)
	}
	// Stop 后状态稳定
	before := atomic.LoadInt32(&checks)
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&checks) != before {
		t.Errorf("checks increased after Stop")
	}
}

func TestWatchdog_DoubleStartIsError(t *testing.T) {
	w := &Watchdog{
		PhaseFn: func() AutoDecryptPhase { return PhaseLive },
		Now:     time.Now,
		Exit:    func(int) {},
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()
	if err := w.Start(); err == nil {
		t.Errorf("expected error on double Start")
	}
}
