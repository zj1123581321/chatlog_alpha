package wechat

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// SpawnFirstFullDecrypt 是 Stage G 的核心 helper，本组测试锁定其契约。
// REGRESSION ANCHOR：对应 /plan-eng-review test plan 的
// TestFirstFullDecrypt_* 规约。

func TestSpawnFirstFullDecrypt_HappyPath_LivePhase(t *testing.T) {
	svc := NewService(&mockConfig{})
	svc.SetPhase(PhaseFirstFull) // caller 契约

	var called int32
	svc.SpawnFirstFullDecrypt(func() error {
		atomic.StoreInt32(&called, 1)
		return nil
	})

	// 等 goroutine 完成 —— 用 decryptWg
	waitWithTimeout(t, &svc.decryptWg, 1*time.Second)

	if atomic.LoadInt32(&called) != 1 {
		t.Error("decryptFn should have been called")
	}
	if got := svc.GetPhase(); got != PhaseLive {
		t.Errorf("phase after success = %q, want Live", got)
	}
	lr := svc.GetLastRun()
	if lr == nil {
		t.Fatal("lastRun should be set")
	}
	if lr.FinalPhase != PhaseLive {
		t.Errorf("lastRun.FinalPhase = %q, want Live", lr.FinalPhase)
	}
	if lr.Error != "" {
		t.Errorf("lastRun.Error should be empty, got %q", lr.Error)
	}
}

func TestSpawnFirstFullDecrypt_ErrorTriggersCircuitBreaker(t *testing.T) {
	svc := NewService(&mockConfig{})
	svc.SetPhase(PhaseFirstFull)

	handlerErr := make(chan error, 1)
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerErr <- err
	})

	wantErr := errors.New("bad key simulated")
	svc.SpawnFirstFullDecrypt(func() error {
		return wantErr
	})

	waitWithTimeout(t, &svc.decryptWg, 1*time.Second)

	if got := svc.GetPhase(); got != PhaseFailed {
		t.Errorf("phase after error = %q, want Failed", got)
	}
	lr := svc.GetLastRun()
	if lr == nil || lr.FinalPhase != PhaseFailed {
		t.Errorf("lastRun should be Failed, got %+v", lr)
	}
	if lr.Error == "" {
		t.Error("lastRun.Error should be populated on failure")
	}

	select {
	case gotErr := <-handlerErr:
		if !errContains(gotErr, "bad key simulated") {
			t.Errorf("handler err should wrap original, got %v", gotErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("errorHandler should have been called")
	}
}

func TestSpawnFirstFullDecrypt_PanicRecovered(t *testing.T) {
	// REGRESSION ANCHOR：首次全量 goroutine panic 必须被 defer recover 吞，
	// 不炸进程。和 waitAndProcess 共享同一 recoverDecryptPanic helper。
	svc := NewService(&mockConfig{})
	svc.SetPhase(PhaseFirstFull)

	handlerErr := make(chan error, 1)
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerErr <- err
	})

	svc.SpawnFirstFullDecrypt(func() error {
		panic("simulated crash in decryptFn")
	})

	waitWithTimeout(t, &svc.decryptWg, 1*time.Second)

	// recoverDecryptPanic 会把 panic 转成 err 给 errorHandler
	select {
	case gotErr := <-handlerErr:
		if !errContains(gotErr, "panic") {
			t.Errorf("handler err should mention panic, got %v", gotErr)
		}
	case <-time.After(500 * time.Millisecond):
		t.Error("errorHandler should have been called with panic error")
	}
}

func TestSpawnFirstFullDecrypt_RespectsWaitGroup(t *testing.T) {
	// Stage E 的 Stop 依赖 wg 能清理 firstFullDecrypt goroutine
	svc := NewService(&mockConfig{})
	svc.SetPhase(PhaseFirstFull)

	blocker := make(chan struct{})
	svc.SpawnFirstFullDecrypt(func() error {
		<-blocker
		return nil
	})

	// 立即 Wait 应该 block（goroutine 还在跑）
	done := make(chan struct{})
	go func() {
		svc.decryptWg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Error("wg.Wait should block while goroutine running")
	case <-time.After(100 * time.Millisecond):
		// expected
	}

	// 释放 goroutine
	close(blocker)
	select {
	case <-done:
		// clean exit
	case <-time.After(1 * time.Second):
		t.Error("wg.Wait should unblock after goroutine done")
	}
}

// --- helpers ---

func waitWithTimeout(t *testing.T, wg interface{ Wait() }, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("waitgroup did not complete within %v", timeout)
	}
}

func errContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	return stringContains(err.Error(), substr)
}

func stringContains(s, substr string) bool {
	return len(s) >= len(substr) && indexOf(s, substr) >= 0
}

func indexOf(s, substr string) int {
	// 简陋 naive，测试代码可读性优先
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
