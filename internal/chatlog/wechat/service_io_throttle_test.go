package wechat

import (
	"testing"
	"time"
)

// TestAcquireDecryptSlot_NilThrottle_FastPath：未注入 throttle 时行为与改造前一致，
// 立即拿到 slot。
func TestAcquireDecryptSlot_NilThrottle_FastPath(t *testing.T) {
	svc := NewService(&mockConfig{})

	done := make(chan struct{})
	go func() {
		svc.acquireDecryptSlot()
		defer svc.releaseDecryptSlot()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("acquireDecryptSlot without throttle should return immediately")
	}
}

// TestAcquireDecryptSlot_ConsultsThrottle：注入 throttle 后，acquire 路径必须先
// 经过 throttle.WaitForQuiet（通过观察 sampler 被调用次数验证）。
func TestAcquireDecryptSlot_ConsultsThrottle(t *testing.T) {
	svc := NewService(&mockConfig{})

	// 持续 busy → throttle 会一直 sample 直到 maxWait
	samples := make([]uint64, 100)
	for i := range samples {
		samples[i] = uint64(i) * 10000
	}
	sampler := &mockSampler{samples: samples}
	svc.SetIoThrottle(NewIoThrottle(sampler,
		WithIoThrottleInterval(10*time.Millisecond),
		WithIoThrottleCooldown(10*time.Millisecond),
		WithIoThrottleMaxWait(80*time.Millisecond),
		WithIoThrottleThreshold(200),
	))

	done := make(chan struct{})
	go func() {
		svc.acquireDecryptSlot()
		defer svc.releaseDecryptSlot()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("acquireDecryptSlot timed out (throttle should release at maxWait)")
	}

	if calls := sampler.Calls(); calls < 2 {
		t.Errorf("throttle.WaitForQuiet was not consulted: sampler called %d times", calls)
	}
}

// TestAcquireDecryptSlot_QuietWeChat_NoExtraDelay：微信本来就安静时，throttle
// 应该几乎立即放行，不引入 maxWait 级别的延迟。
func TestAcquireDecryptSlot_QuietWeChat_NoExtraDelay(t *testing.T) {
	svc := NewService(&mockConfig{})

	// 持续低 IOPS（每 10ms 增 1 次操作 = 100 IOPS < 200 threshold）
	sampler := &mockSampler{samples: []uint64{1000, 1001, 1002, 1003, 1004}}
	svc.SetIoThrottle(NewIoThrottle(sampler,
		WithIoThrottleInterval(10*time.Millisecond),
		WithIoThrottleCooldown(10*time.Millisecond),
		WithIoThrottleMaxWait(500*time.Millisecond),
		WithIoThrottleThreshold(200),
	))

	start := time.Now()
	done := make(chan struct{})
	go func() {
		svc.acquireDecryptSlot()
		defer svc.releaseDecryptSlot()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("acquireDecryptSlot stuck despite quiet WeChat")
	}

	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("quiet WeChat should release fast, got %v", elapsed)
	}
}

// TestAcquireDecryptSlot_StillEnforcesSingleConcurrency：throttle 集成不应破坏
// 原有的"单并发解密 slot"语义。
func TestAcquireDecryptSlot_StillEnforcesSingleConcurrency(t *testing.T) {
	svc := NewService(&mockConfig{})
	// 注入一个永远 quiet 的 sampler，避免 throttle 自己的延迟干扰判断
	sampler := &mockSampler{samples: []uint64{0, 0, 0, 0, 0}}
	svc.SetIoThrottle(NewIoThrottle(sampler,
		WithIoThrottleInterval(5*time.Millisecond),
		WithIoThrottleCooldown(5*time.Millisecond),
		WithIoThrottleMaxWait(100*time.Millisecond),
	))

	first := make(chan struct{})
	secondReleased := make(chan struct{})

	go func() {
		svc.acquireDecryptSlot()
		close(first)
		time.Sleep(80 * time.Millisecond)
		svc.releaseDecryptSlot()
	}()

	<-first
	go func() {
		svc.acquireDecryptSlot()
		defer svc.releaseDecryptSlot()
		close(secondReleased)
	}()

	// 第二个不应在第一个释放前拿到 slot
	select {
	case <-secondReleased:
		t.Fatal("second acquire should be blocked until first releases")
	case <-time.After(40 * time.Millisecond):
		// expected
	}
	<-secondReleased // 等第二个最终拿到
}
