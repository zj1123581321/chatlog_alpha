package util

import (
	"testing"
)

// TestSetBackgroundPriority_DoesNotPanic 验证设置后台优先级不会导致崩溃。
// 实际效果（进程优先级类）只能在 Windows 任务管理器中验证，这里仅保证健壮性。
func TestSetBackgroundPriority_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SetBackgroundPriority panicked: %v", r)
		}
	}()

	err := SetBackgroundPriority()
	// 在 CI / 非 Windows 环境下可能返回 nil 或平台不支持错误，都是可接受的
	if err != nil {
		t.Logf("SetBackgroundPriority returned: %v (acceptable in test env)", err)
	}
}

// TestSetBackgroundPriority_SecondCallIsSafe 验证重复调用不会崩溃或死锁。
func TestSetBackgroundPriority_SecondCallIsSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second call panicked: %v", r)
		}
	}()

	_ = SetBackgroundPriority()
	_ = SetBackgroundPriority()
}
