package util

import (
	"fmt"
	"sync"
	"syscall"
	"unsafe"
)

// Windows 线程信息类（来自 winnt.h），SetThreadInformation 第二参数。
const (
	threadInformationClassIoPriority = 4 // ThreadIoPriority
	threadInformationClassPagePriority = 5 // ThreadPagePriority（备用）
)

// IO_PRIORITY_HINT 枚举值（来自 ntddk.h）。
//
// 实测语义（参考 Windows Internals）：
//   0 = IoPriorityVeryLow  → 后台预读、defrag 等用，被前台请求挤到队列尾
//   1 = IoPriorityLow      → 索引服务、Windows Search
//   2 = IoPriorityNormal   → 默认
//   3 = IoPriorityHigh     → 系统不允许用户态设置
//   4 = IoPriorityCritical → 系统不允许用户态设置
const (
	ioPriorityHintVeryLow uint32 = 0
	ioPriorityHintNormal  uint32 = 2
)

var (
	procSetThreadInformation *syscall.LazyProc
	procGetCurrentThread     *syscall.LazyProc
	procGetCurrentThreadId   *syscall.LazyProc
	procLoadOnce             sync.Once
)

func loadProcs() {
	procLoadOnce.Do(func() {
		kernel32 := syscall.NewLazyDLL("kernel32.dll")
		procSetThreadInformation = kernel32.NewProc("SetThreadInformation")
		procGetCurrentThread = kernel32.NewProc("GetCurrentThread")
		procGetCurrentThreadId = kernel32.NewProc("GetCurrentThreadId")
	})
}

func setThreadIoPriority(hint uint32) error {
	loadProcs()
	if err := procSetThreadInformation.Find(); err != nil {
		// Vista 以下不存在该 API；项目目标是 Windows 10+，这里仍兜底防御。
		return fmt.Errorf("SetThreadInformation unavailable: %w", err)
	}
	hThread, _, _ := procGetCurrentThread.Call()

	hintLocal := hint
	r1, _, e1 := procSetThreadInformation.Call(
		hThread,
		uintptr(threadInformationClassIoPriority),
		uintptr(unsafe.Pointer(&hintLocal)),
		unsafe.Sizeof(hintLocal),
	)
	if r1 == 0 {
		// 0 = FALSE = 失败；e1 是 GetLastError
		return fmt.Errorf("SetThreadInformation(IoPriority=%d) failed: %v", hint, e1)
	}
	return nil
}

func setCurrentThreadIoPriorityVeryLowImpl() error {
	return setThreadIoPriority(ioPriorityHintVeryLow)
}

func restoreCurrentThreadIoPriorityImpl() error {
	return setThreadIoPriority(ioPriorityHintNormal)
}

// goroutineOSThreadHint 返回 GetCurrentThreadId() 结果，供测试验证 LockOSThread。
func goroutineOSThreadHint() uint64 {
	loadProcs()
	if err := procGetCurrentThreadId.Find(); err != nil {
		return 0
	}
	tid, _, _ := procGetCurrentThreadId.Call()
	return uint64(tid)
}
