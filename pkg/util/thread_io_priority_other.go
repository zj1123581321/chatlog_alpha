//go:build !windows

package util

// 非 Windows 平台 no-op：项目仅在 Windows 上真实使用，CI/Linux/macOS 保持构建绿色。

func setCurrentThreadIoPriorityVeryLowImpl() error { return nil }
func restoreCurrentThreadIoPriorityImpl() error    { return nil }

// goroutineOSThreadHint 返回当前 OS 线程的标识（用于测试断言迁移）。
// 非 Windows 上返回 0，测试会 Skip。
func goroutineOSThreadHint() uint64 { return 0 }
