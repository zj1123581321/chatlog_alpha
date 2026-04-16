package wechat

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- 并发控制测试 ---

func TestDecryptSemaphore_LimitsConcurrency(t *testing.T) {
	svc := NewService(&mockConfig{})

	var maxConcurrent int64
	var currentConcurrent int64
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.acquireDecryptSlot()
			defer svc.releaseDecryptSlot()

			cur := atomic.AddInt64(&currentConcurrent, 1)
			// 记录峰值
			for {
				old := atomic.LoadInt64(&maxConcurrent)
				if cur <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt64(&currentConcurrent, -1)
		}()
	}

	wg.Wait()
	peak := atomic.LoadInt64(&maxConcurrent)
	if peak > 1 {
		t.Errorf("expected max concurrency 1, got %d", peak)
	}
}

// --- 熔断隔离测试 ---

func TestFileLockError_DoesNotTriggerErrorHandler(t *testing.T) {
	handlerCalled := false
	svc := NewService(&mockConfig{})
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerCalled = true
	})

	lockErr := &os.PathError{Op: "open", Path: "test.db", Err: fileLockErrno()}
	svc.handleDecryptError(lockErr)

	if handlerCalled {
		t.Error("error handler should NOT be called for file lock errors")
	}
}

func TestNonLockError_TriggersErrorHandler(t *testing.T) {
	handlerCalled := false
	svc := NewService(&mockConfig{})
	svc.SetAutoDecryptErrorHandler(func(err error) {
		handlerCalled = true
	})

	svc.handleDecryptError(os.ErrPermission)

	if !handlerCalled {
		t.Error("error handler should be called for non-lock errors")
	}
}

// --- mock config ---

type mockConfig struct{}

func (m *mockConfig) GetDataKey() string              { return "" }
func (m *mockConfig) GetDataDir() string              { return "" }
func (m *mockConfig) GetWorkDir() string              { return "" }
func (m *mockConfig) GetPlatform() string             { return "windows" }
func (m *mockConfig) GetVersion() int                 { return 4 }
func (m *mockConfig) GetWalEnabled() bool             { return false }
func (m *mockConfig) GetAutoDecryptDebounce() int     { return 1000 }
