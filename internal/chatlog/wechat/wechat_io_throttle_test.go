package wechat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockSampler 是一个受控的 WeChatIoSampler：按预设序列返回累计 IO 操作数。
//
// 用法：samples 是"累计 ops 计数"序列（不是 IOPS 速率）。throttle 内部
// 用相邻两次差值除以 pollInterval 算 IOPS。
//
//   samples=[100, 200] + interval=100ms → IOPS = (200-100)/0.1s = 1000
//   samples=[100, 105] + interval=100ms → IOPS = (105-100)/0.1s = 50
//
// idx 用尽后重复最后一个值（模拟"持续低 IO"或"持续高 IO"）。
type mockSampler struct {
	mu        sync.Mutex
	samples   []uint64
	idx       int
	err       error
	callCount int32
}

func (m *mockSampler) SampleIoOps(ctx context.Context) (uint64, error) {
	atomic.AddInt32(&m.callCount, 1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	if len(m.samples) == 0 {
		return 0, nil
	}
	if m.idx >= len(m.samples) {
		return m.samples[len(m.samples)-1], nil
	}
	v := m.samples[m.idx]
	m.idx++
	return v, nil
}

func (m *mockSampler) Calls() int32 { return atomic.LoadInt32(&m.callCount) }

// shortThrottle 创建用于测试的快速 throttle：interval 10ms / cooldown 20ms / maxWait 200ms / threshold 200 IOPS。
func shortThrottle(s WeChatIoSampler) *IoThrottle {
	return NewIoThrottle(s,
		WithIoThrottleInterval(10*time.Millisecond),
		WithIoThrottleCooldown(20*time.Millisecond),
		WithIoThrottleMaxWait(200*time.Millisecond),
		WithIoThrottleThreshold(200),
	)
}

// TestThrottle_LowIOPS_ReturnsQuickly：连续低 IOPS 应在 ~2 个 interval + cooldown 内返回。
func TestThrottle_LowIOPS_ReturnsQuickly(t *testing.T) {
	// 100ms 内累计增加 1 次操作 → IOPS = 100，低于 threshold=200
	m := &mockSampler{samples: []uint64{1000, 1001, 1002, 1003}}
	tt := shortThrottle(m)

	start := time.Now()
	if err := tt.WaitForQuiet(context.Background()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	elapsed := time.Since(start)
	// baseline (10ms) + judge (10ms) + cooldown (20ms) ≈ 40ms；放宽到 150ms 容忍调度抖动
	if elapsed > 150*time.Millisecond {
		t.Errorf("expected fast return on quiet WeChat, got %v", elapsed)
	}
}

// TestThrottle_BusyThenQuiet_WaitsThenReturns：先高后低，等到低才返回。
func TestThrottle_BusyThenQuiet_WaitsThenReturns(t *testing.T) {
	// 序列：[0, 100, 200, 201, 202]
	// 第 1→2: (100-0)/10ms = 10000 IOPS，busy
	// 第 2→3: (200-100)/10ms = 10000 IOPS，busy
	// 第 3→4: (201-200)/10ms = 100 IOPS，quiet ✓
	m := &mockSampler{samples: []uint64{0, 100, 200, 201, 202, 203}}
	tt := shortThrottle(m)

	start := time.Now()
	if err := tt.WaitForQuiet(context.Background()); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	elapsed := time.Since(start)
	// 至少要 3 个 interval（0→1→2→3）后才能判 quiet，再 + cooldown
	if elapsed < 30*time.Millisecond {
		t.Errorf("returned too fast (didn't wait for busy → quiet transition): %v", elapsed)
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("returned too slow: %v", elapsed)
	}
}

// TestThrottle_AlwaysBusy_ReturnsAfterMaxWait：持续高 IOPS，maxWait 兜底返回 nil。
func TestThrottle_AlwaysBusy_ReturnsAfterMaxWait(t *testing.T) {
	// 每个 sample 增加 10000，永远高 IOPS
	samples := make([]uint64, 100)
	for i := range samples {
		samples[i] = uint64(i) * 10000
	}
	m := &mockSampler{samples: samples}
	tt := shortThrottle(m)

	start := time.Now()
	err := tt.WaitForQuiet(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("WaitForQuiet should return nil after maxWait (let upper layer continue), got %v", err)
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("returned before maxWait (200ms), got %v", elapsed)
	}
	if elapsed > 350*time.Millisecond {
		t.Errorf("waited too long past maxWait, got %v", elapsed)
	}
}

// TestThrottle_ContextCanceled_ReturnsErr：ctx 取消应立即返回 ctx.Err()。
func TestThrottle_ContextCanceled_ReturnsErr(t *testing.T) {
	samples := make([]uint64, 100)
	for i := range samples {
		samples[i] = uint64(i) * 10000 // 永远 busy
	}
	m := &mockSampler{samples: samples}
	tt := shortThrottle(m)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := tt.WaitForQuiet(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("ctx cancel not respected promptly, took %v", elapsed)
	}
}

// TestThrottle_SamplerError_DoesNotBlock：sampler 一直报错应视为"无法判断 → 安静"，立即返回。
//
// 设计动机：sampler 失败时阻塞解密会让数据永远追不上；宁可放过一次解密
// 让 IO 优先级（方案 1）兜底，也不要把 chatlog 卡死。
func TestThrottle_SamplerError_DoesNotBlock(t *testing.T) {
	m := &mockSampler{err: errors.New("gopsutil failed")}
	tt := shortThrottle(m)

	start := time.Now()
	err := tt.WaitForQuiet(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("sampler error should be swallowed, got %v", err)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("sampler error should fast-fail, got %v", elapsed)
	}
}

// TestThrottle_IsActive_ReflectsLastObservation：IsActive() 反映最近一次观察。
func TestThrottle_IsActive_ReflectsLastObservation(t *testing.T) {
	// 持续高 IOPS
	samples := make([]uint64, 100)
	for i := range samples {
		samples[i] = uint64(i) * 10000
	}
	m := &mockSampler{samples: samples}
	tt := shortThrottle(m)

	// 触发至少几次 sample
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = tt.WaitForQuiet(ctx)

	if !tt.IsActive() {
		t.Error("expected IsActive=true after observing busy WeChat")
	}
}

// TestThrottle_NilSampler_NoOp：sampler=nil 应直接返回 nil（防御性，便于无微信场景）。
func TestThrottle_NilSampler_NoOp(t *testing.T) {
	tt := NewIoThrottle(nil)
	start := time.Now()
	if err := tt.WaitForQuiet(context.Background()); err != nil {
		t.Errorf("nil sampler should no-op, got %v", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Error("nil sampler should return immediately")
	}
	if tt.IsActive() {
		t.Error("nil sampler should never report active")
	}
}
