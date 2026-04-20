package wechat

import (
	"context"
	"fmt"

	"github.com/shirou/gopsutil/v4/process"
)

// gopsutilSampler 通过 gopsutil v4 的 process.IOCounters() 取微信进程的累计 IO 操作数。
//
// Windows 上底层是 GetProcessIoCounters Win32 API，返回 IO_COUNTERS 结构体里的
// ReadOperationCount + WriteOperationCount。这两个字段是"操作次数"（≈ syscall 数），
// 不是字节数；适合算 IOPS。
//
// PID 通过 SetPID 注入，便于微信重启换 PID 时切换。PID=0 时所有 sample 返回 (0, nil)，
// 等同于"无微信运行 → throttle 视为安静"。
type gopsutilSampler struct {
	pidProvider func() int32
}

// NewGopsutilSampler 构造一个真实环境用的 sampler。
//
//	pidProvider: 返回当前微信主进程 PID 的闭包。每次采样都重新调用，
//	             以便支持微信重启换 PID 的场景。返回 0 表示无微信运行。
func NewGopsutilSampler(pidProvider func() int32) WeChatIoSampler {
	return &gopsutilSampler{pidProvider: pidProvider}
}

func (s *gopsutilSampler) SampleIoOps(ctx context.Context) (uint64, error) {
	if s.pidProvider == nil {
		return 0, nil
	}
	pid := s.pidProvider()
	if pid <= 0 {
		return 0, nil
	}

	p, err := process.NewProcessWithContext(ctx, pid)
	if err != nil {
		return 0, fmt.Errorf("NewProcess(%d): %w", pid, err)
	}

	io, err := p.IOCountersWithContext(ctx)
	if err != nil {
		return 0, fmt.Errorf("IOCounters(%d): %w", pid, err)
	}
	if io == nil {
		return 0, nil
	}
	return io.ReadCount + io.WriteCount, nil
}
