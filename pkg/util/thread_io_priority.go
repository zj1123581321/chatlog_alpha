package util

import "runtime"

// 包级函数指针：默认指向平台实现，单测可替换为 spy。
//
// 设计动机：syscall 在测试环境难以验证（要 GetThreadInformation 反向读，
// 而且 CI 上跑非 Windows）。把"业务逻辑（顺序、配对、panic 安全）"和
// "syscall 实现"分开后，业务逻辑可以纯 Go 测，syscall 只测"不 panic"。
var (
	setThreadIoPriorityVeryLowFn = setCurrentThreadIoPriorityVeryLowImpl
	restoreThreadIoPriorityFn    = restoreCurrentThreadIoPriorityImpl
)

// WithBackgroundIO 在当前 goroutine 上执行 fn，期间该 OS 线程的 I/O 优先级
// 被降为 VeryLow，让位给 Windows 内核 IO 调度器优先服务前台进程（微信）。
//
// 关键约束：
//  1. runtime.LockOSThread 锁定 goroutine 到当前 OS 线程，避免 Go runtime
//     把 goroutine 迁移到其他线程导致优先级"飘走"
//  2. defer 配对：set 失败 / fn panic / fn return 三种路径都恢复
//  3. set 失败不阻塞 fn 执行（best-effort，让位失败也得继续解密）
//  4. fn 的 error 透传；panic 也透传（restore 在 panic 之前已 defer 恢复）
//
// Windows 上线程级 I/O 优先级是真实的内核机制（IRP 优先级），比进程级
// BELOW_NORMAL_PRIORITY_CLASS 更精确：进程级会同时降 CPU，影响 chatlog
// 自身的 HTTP 接口响应；线程级只压被锁定的解密线程。
//
// 非 Windows 平台是 no-op（fn 直接执行）。
func WithBackgroundIO(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	setErr := setThreadIoPriorityVeryLowFn()
	// 即使 set 失败也安排 restore：可能内核 API 不存在（旧 Windows）
	// 或权限不够，此时 restore 也会 no-op，不会有副作用。
	defer func() {
		if setErr == nil {
			_ = restoreThreadIoPriorityFn()
		}
	}()

	return fn()
}

// SetCurrentThreadIoPriorityVeryLow 把当前 OS 线程的 I/O 优先级降为 VeryLow。
// 调用方需自行 runtime.LockOSThread 并保证 Restore 配对。
//
// 推荐使用 WithBackgroundIO 而不是直接调这个函数。
func SetCurrentThreadIoPriorityVeryLow() error {
	return setThreadIoPriorityVeryLowFn()
}

// RestoreCurrentThreadIoPriority 把当前 OS 线程的 I/O 优先级恢复为 Normal。
func RestoreCurrentThreadIoPriority() error {
	return restoreThreadIoPriorityFn()
}
