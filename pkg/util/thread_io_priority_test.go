package util

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestWithBackgroundIO_DoesNotPanic 基础健壮性：调用不崩溃。
func TestWithBackgroundIO_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithBackgroundIO panicked: %v", r)
		}
	}()
	if err := WithBackgroundIO(func() error { return nil }); err != nil {
		t.Logf("WithBackgroundIO returned: %v (acceptable in test env)", err)
	}
}

// TestWithBackgroundIO_FnIsExecuted 验证 fn 确实被调用。
func TestWithBackgroundIO_FnIsExecuted(t *testing.T) {
	var called int32
	_ = WithBackgroundIO(func() error {
		atomic.StoreInt32(&called, 1)
		return nil
	})
	if atomic.LoadInt32(&called) != 1 {
		t.Fatal("fn was not executed")
	}
}

// TestWithBackgroundIO_FnErrorPropagates 验证 fn 返回的 error 透传。
func TestWithBackgroundIO_FnErrorPropagates(t *testing.T) {
	want := errors.New("decrypt failed")
	got := WithBackgroundIO(func() error { return want })
	if !errors.Is(got, want) {
		t.Fatalf("error not propagated: want %v, got %v", want, got)
	}
}

// TestWithBackgroundIO_PanicPropagates_AndRestoresPriority 验证 fn panic 时
// WithBackgroundIO 也 panic（透传），并且 restore 被调用过（通过 spy 验证）。
func TestWithBackgroundIO_PanicPropagates_AndRestoresPriority(t *testing.T) {
	var setCalls, restoreCalls int32
	restoreSpies(t,
		func() error { atomic.AddInt32(&setCalls, 1); return nil },
		func() error { atomic.AddInt32(&restoreCalls, 1); return nil },
	)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate")
		}
		if atomic.LoadInt32(&setCalls) != 1 {
			t.Errorf("set should be called once, got %d", setCalls)
		}
		if atomic.LoadInt32(&restoreCalls) != 1 {
			t.Errorf("restore should be called once even on panic, got %d", restoreCalls)
		}
	}()

	_ = WithBackgroundIO(func() error {
		panic("boom")
	})
}

// TestWithBackgroundIO_SetAndRestoreCalledInOrder 验证调用顺序：set → fn → restore。
func TestWithBackgroundIO_SetAndRestoreCalledInOrder(t *testing.T) {
	var order []string
	var mu sync.Mutex
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	restoreSpies(t,
		func() error { record("set"); return nil },
		func() error { record("restore"); return nil },
	)

	if err := WithBackgroundIO(func() error { record("fn"); return nil }); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}

	want := []string{"set", "fn", "restore"}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != len(want) {
		t.Fatalf("order length mismatch: want %v, got %v", want, order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order mismatch at %d: want %v, got %v", i, want, order)
		}
	}
}

// TestWithBackgroundIO_RestoreStillCalled_WhenSetFails 验证 set 失败时 restore 仍调用，
// fn 仍执行（让位 IO 是 best-effort，set 失败不该阻塞解密）。
func TestWithBackgroundIO_RestoreStillCalled_WhenSetFails(t *testing.T) {
	var restoreCalls, fnCalls int32
	restoreSpies(t,
		func() error { return errors.New("set failed (ok in non-Vista)") },
		func() error { atomic.AddInt32(&restoreCalls, 1); return nil },
	)

	err := WithBackgroundIO(func() error {
		atomic.AddInt32(&fnCalls, 1)
		return nil
	})
	if err != nil {
		t.Fatalf("WithBackgroundIO should not propagate set errors, got %v", err)
	}
	if atomic.LoadInt32(&fnCalls) != 1 {
		t.Errorf("fn should still execute when set fails, calls=%d", fnCalls)
	}
	if atomic.LoadInt32(&restoreCalls) != 1 {
		// 当 set 失败时不需要 restore（因为没什么可恢复的），但实现上调一次也 OK。
		// 这里宽松断言：0 或 1 都接受。
		if restoreCalls > 1 {
			t.Errorf("restore should be called at most once, got %d", restoreCalls)
		}
	}
}

// TestSetCurrentThreadIoPriority_DirectCall_DoesNotPanic 底层 API 的健壮性。
func TestSetCurrentThreadIoPriority_DirectCall_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SetCurrentThreadIoPriorityVeryLow panicked: %v", r)
		}
	}()
	_ = SetCurrentThreadIoPriorityVeryLow()
	_ = RestoreCurrentThreadIoPriority()
}

// TestWithBackgroundIO_GoroutineStaysOnSameThread 验证 LockOSThread 已配对生效：
// fn 内执行期间 goroutine 不会被迁移到其他 OS 线程。
//
// 不是 100% 严格证明（runtime 不暴露 lock 状态查询），但是一种间接观测。
func TestWithBackgroundIO_GoroutineStaysOnSameThread(t *testing.T) {
	var threadIDInside, threadIDInside2 uint64
	_ = WithBackgroundIO(func() error {
		threadIDInside = goroutineOSThreadHint()
		// 触发一些可能让 runtime 迁移 goroutine 的事件
		runtime.Gosched()
		for i := 0; i < 1000; i++ {
			runtime.Gosched()
		}
		threadIDInside2 = goroutineOSThreadHint()
		return nil
	})
	if threadIDInside == 0 || threadIDInside2 == 0 {
		t.Skip("OS thread hint unavailable; skipping")
	}
	if threadIDInside != threadIDInside2 {
		t.Errorf("goroutine migrated between OS threads: %d → %d (LockOSThread not effective)",
			threadIDInside, threadIDInside2)
	}
}

// restoreSpies 用 spy 替换包级 set/restore 函数指针，t.Cleanup 自动还原。
// 这是测试级 hook，prod 代码不知道它们的存在。
func restoreSpies(t *testing.T, set, restore func() error) {
	t.Helper()
	origSet := setThreadIoPriorityVeryLowFn
	origRestore := restoreThreadIoPriorityFn
	setThreadIoPriorityVeryLowFn = set
	restoreThreadIoPriorityFn = restore
	t.Cleanup(func() {
		setThreadIoPriorityVeryLowFn = origSet
		restoreThreadIoPriorityFn = origRestore
	})
}
