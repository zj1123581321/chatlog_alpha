package wechat

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// --- retryOnFileLock 行为测试 ---

func TestRetryOnFileLock_SuccessFirstAttempt(t *testing.T) {
	calls := 0
	err := retryOnFileLock(func() error {
		calls++
		return nil
	}, 3, 10*time.Millisecond)

	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestRetryOnFileLock_SuccessAfterRetries(t *testing.T) {
	calls := 0
	lockErr := &os.PathError{Op: "open", Path: "test.db", Err: fileLockErrno()}
	err := retryOnFileLock(func() error {
		calls++
		if calls < 3 {
			return lockErr
		}
		return nil
	}, 5, 10*time.Millisecond)

	if err != nil {
		t.Fatalf("expected nil error after retries, got: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestRetryOnFileLock_NonLockErrorNoRetry(t *testing.T) {
	calls := 0
	otherErr := fmt.Errorf("some other error")
	err := retryOnFileLock(func() error {
		calls++
		return otherErr
	}, 5, 10*time.Millisecond)

	if err != otherErr {
		t.Fatalf("expected original error, got: %v", err)
	}
	if calls != 1 {
		t.Errorf("non-lock error should not trigger retry, got %d calls", calls)
	}
}

func TestRetryOnFileLock_ExhaustedRetries(t *testing.T) {
	calls := 0
	lockErr := &os.PathError{Op: "open", Path: "test.db", Err: fileLockErrno()}
	err := retryOnFileLock(func() error {
		calls++
		return lockErr
	}, 3, 10*time.Millisecond)

	if err == nil {
		t.Fatal("expected error after exhausted retries")
	}
	if calls != 3 {
		t.Errorf("expected exactly 3 calls, got %d", calls)
	}
}

func TestRetryOnFileLock_RespectsBackoff(t *testing.T) {
	lockErr := &os.PathError{Op: "open", Path: "test.db", Err: fileLockErrno()}
	start := time.Now()
	retryOnFileLock(func() error {
		return lockErr
	}, 3, 50*time.Millisecond)
	elapsed := time.Since(start)

	// 3 次尝试, 退避间隔: 50ms + 100ms = 150ms 最小
	// 允许一些调度误差
	if elapsed < 100*time.Millisecond {
		t.Errorf("backoff too fast: %v, expected >= 100ms", elapsed)
	}
}
