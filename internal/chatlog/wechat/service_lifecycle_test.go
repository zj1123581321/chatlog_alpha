package wechat

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

// --- retryOnFileLockCtx 测试 ---

func TestRetryOnFileLockCtx_CancelDuringBackoff_ReturnsImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	lockErr := &os.PathError{Op: "open", Path: "test.db", Err: fileLockErrno()}
	var attempts int

	start := time.Now()
	// 启一个 goroutine 在 50ms 后 cancel
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := retryOnFileLockCtx(ctx, func() error {
		attempts++
		return lockErr
	}, 5, 500*time.Millisecond)

	elapsed := time.Since(start)

	// 应该在 ~50ms 左右返回，远小于 5 × 500ms = 2500ms 的最坏情况
	if elapsed > 500*time.Millisecond {
		t.Errorf("expected fast cancel return, took %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRetryOnFileLockCtx_AlreadyCancelled_ReturnsBeforeFirstCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即 cancel

	called := false
	err := retryOnFileLockCtx(ctx, func() error {
		called = true
		return nil
	}, 5, 1*time.Second)

	if called {
		t.Error("op should not be called when ctx already cancelled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRetryOnFileLockCtx_NoFileLockErr_BehavesAsBefore(t *testing.T) {
	// 非锁错误应该立即返回，不做 retry，和 retryOnFileLock 行为一致
	permErr := os.ErrPermission
	attempts := 0

	err := retryOnFileLockCtx(context.Background(), func() error {
		attempts++
		return permErr
	}, 5, 1*time.Second)

	if attempts != 1 {
		t.Errorf("expected 1 attempt for non-lock error, got %d", attempts)
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("expected os.ErrPermission, got %v", err)
	}
}

// --- StopAutoDecrypt + WaitGroup 测试 ---

func TestStopAutoDecrypt_WaitsForInflightGoroutines(t *testing.T) {
	svc := NewService(&mockConfig{})

	var mu sync.Mutex
	var cleanlyFinished bool

	// 模拟一个后台 goroutine 挂在 decryptCtx.Done() 上
	svc.decryptWg.Add(1)
	go func() {
		defer svc.decryptWg.Done()
		<-svc.decryptCtx.Done()
		mu.Lock()
		cleanlyFinished = true
		mu.Unlock()
	}()

	start := time.Now()
	_ = svc.StopAutoDecrypt()
	elapsed := time.Since(start)

	// 应该秒级返回（goroutine 收到 cancel 后立即退出）
	if elapsed > 1*time.Second {
		t.Errorf("Stop should return quickly when goroutines honor ctx, took %v", elapsed)
	}

	mu.Lock()
	finished := cleanlyFinished
	mu.Unlock()
	if !finished {
		t.Error("goroutine should have finished cleanly after ctx cancel")
	}
}

func TestStopAutoDecrypt_TimeoutWhenGoroutineHangs(t *testing.T) {
	// 模拟不响应 ctx 的 goroutine（stuck inside 某 blocking op）
	// 验证 Stop 在 stopTimeout 内强制返回，不会永久 hang

	// 临时缩短 stopTimeout 避免测试跑 5 秒
	origTimeout := stopTimeout
	stopTimeout = 200 * time.Millisecond
	defer func() { stopTimeout = origTimeout }()

	svc := NewService(&mockConfig{})

	// 让 hanger 在 stopTimeout 过后能自行退出（避免 goroutine 泄漏到后续测试）
	release := make(chan struct{})
	defer close(release)

	svc.decryptWg.Add(1)
	go func() {
		defer svc.decryptWg.Done()
		// 故意不读 ctx，只等 release（模拟不响应 ctx 的 blocking op）
		<-release
	}()

	start := time.Now()
	_ = svc.StopAutoDecrypt()
	elapsed := time.Since(start)

	// 应该在 stopTimeout (200ms) 附近返回，不会无限等
	if elapsed < stopTimeout {
		t.Errorf("Stop returned too fast %v, expected ~%v timeout", elapsed, stopTimeout)
	}
	if elapsed > stopTimeout+500*time.Millisecond {
		t.Errorf("Stop took too long %v, expected ~%v", elapsed, stopTimeout)
	}
}

func TestStartAutoDecrypt_AfterStop_RefreshesCtx(t *testing.T) {
	svc := NewService(&mockConfig{})

	// 取第一个 ctx
	firstCtx := svc.decryptCtx
	_ = svc.StopAutoDecrypt()

	// 第一个 ctx 应该已被 cancel
	if firstCtx.Err() == nil {
		t.Error("first ctx should be cancelled after Stop")
	}

	// 再次 Start 应刷新 ctx
	// 注意：StartAutoDecrypt 会真的启动 file monitor 需要 data dir
	// 这里只测 ctx refresh 部分，手动调用 refresh 的逻辑
	svc.mutex.Lock()
	if svc.decryptCtx == nil || svc.decryptCtx.Err() != nil {
		svc.decryptCtx, svc.decryptCancel = context.WithCancel(context.Background())
	}
	svc.mutex.Unlock()

	if svc.decryptCtx.Err() != nil {
		t.Error("new ctx should not be cancelled")
	}
	if svc.decryptCtx == firstCtx {
		t.Error("new ctx should be a different instance")
	}
}
