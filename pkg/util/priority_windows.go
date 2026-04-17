package util

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Windows 进程优先级常量，来自 <winbase.h>。
// golang.org/x/sys/windows 未导出这些具体值，此处显式定义。
const (
	belowNormalPriorityClass  uint32 = 0x00004000
	processModeBackgroundBegin uint32 = 0x00100000
)

// SetBackgroundPriority 把当前进程降为后台优先级。
//
// 两步降级：
//  1. BELOW_NORMAL_PRIORITY_CLASS：CPU 调度让位给普通优先级进程（微信）
//  2. PROCESS_MODE_BACKGROUND_BEGIN：同时降低 I/O 优先级和内存优先级
//
// 第 2 步只在进程当前优先级 >= NORMAL 时生效；如果某些环境下失败是正常的，
// 只记录不报错。核心目标（CPU 让位）靠第 1 步就能达成。
func SetBackgroundPriority() error {
	h := windows.CurrentProcess()
	if err := windows.SetPriorityClass(h, belowNormalPriorityClass); err != nil {
		return fmt.Errorf("set BELOW_NORMAL_PRIORITY_CLASS failed: %w", err)
	}
	// BACKGROUND_BEGIN 失败不致命，I/O 优先级只是额外优化
	_ = windows.SetPriorityClass(h, processModeBackgroundBegin)
	return nil
}
