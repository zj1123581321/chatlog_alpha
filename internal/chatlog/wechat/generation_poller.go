package wechat

// generation_poller.go：Step 5e Server 端 invalidation 轮询
// （architecture-rework-2026-05-06.md Eng Review Lock A3 server-side）。
//
// 为什么需要：移除 fsnotify 后，dbm.go 原本基于 fsnotify event 的 cache invalidation
// 失效。watcher 改成"原子 swap status.json"模式后，server 进程必须用另一个机制
// 探知 current_generation 变化。本 poller 每 30s 读一次 status.json，发现
// current_generation 字符串变化就触发 OnChange 回调（生产里 wire 到
// dbm.InvalidateAll）。
//
// 不直接依赖 dbm 包：通过 OnChange 回调把 invalidation 决定留给消费者。这让
// poller 可以在 server 进程拆分（Step 7）后被复用，且 unit test 不需要 mock dbm。

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// DefaultGenerationPollInterval 是 spec Eng A3 锁定的 server 端 polling 节奏。
const DefaultGenerationPollInterval = 30 * time.Second

// GenerationPollerOpts 配置一个新 poller。OnChange 必填；OnError 可选。
type GenerationPollerOpts struct {
	WorkDir  string
	Interval time.Duration

	// OnChange 在每次检测到 current_generation 变化（含首次检测到非空）时被调用，
	// 参数是新的 current_generation。返回 error 仅打 log，不影响 poller 继续运行。
	OnChange func(newCurrent string) error

	// OnError 可选；status.json 读取/解析错误（不含 not-found）时调用。
	// 不设置 → 错误被吞掉。生产应至少 wire 到 zerolog warn。
	OnError func(error)
}

// GenerationPoller 在后台周期检查 status.json.current_generation，触发 OnChange。
// 单实例：Start 后再 Start 返回错误；Stop 是幂等的。
type GenerationPoller struct {
	opts GenerationPollerOpts

	mu          sync.Mutex
	lastCurrent string
	running     bool
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewGenerationPoller 构造 poller。返回值未 Start，调用方决定何时 Start。
//
// Interval ≤ 0 → DefaultGenerationPollInterval。
func NewGenerationPoller(opts GenerationPollerOpts) *GenerationPoller {
	if opts.Interval <= 0 {
		opts.Interval = DefaultGenerationPollInterval
	}
	return &GenerationPoller{opts: opts}
}

// CheckOnce 同步执行一次"读 status.json + 触发 OnChange（如变化）"。
// 主循环每个 tick 走这里，外部测试也可以直接调用。
//
// 返回 error 仅在调用方需要排查严重问题时使用；运行时正常情况下返回 nil。
func (p *GenerationPoller) CheckOnce() error {
	st, err := ReadStatus(p.opts.WorkDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// status.json 还没建（watcher 首次启动前 server 可能先跑）：静默跳过。
			return nil
		}
		if p.opts.OnError != nil {
			p.opts.OnError(err)
		}
		return err
	}

	cur := st.CurrentGeneration

	p.mu.Lock()
	changed := cur != p.lastCurrent && cur != ""
	if changed {
		p.lastCurrent = cur
	}
	p.mu.Unlock()

	if changed && p.opts.OnChange != nil {
		if cbErr := p.opts.OnChange(cur); cbErr != nil && p.opts.OnError != nil {
			p.opts.OnError(fmt.Errorf("OnChange callback: %w", cbErr))
		}
	}
	return nil
}

// Start 起后台 ticker goroutine。返回错误仅在已 Start 时（防重复启动）。
func (p *GenerationPoller) Start() error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return errors.New("generation poller: already running")
	}
	p.running = true
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	go p.loop()
	return nil
}

// Stop 通知 loop 退出并等它结束。幂等：未 Start 或已 Stop 时返回 nil。
func (p *GenerationPoller) Stop() error {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return nil
	}
	p.running = false
	close(p.stopCh)
	doneCh := p.doneCh
	p.mu.Unlock()
	<-doneCh
	return nil
}

func (p *GenerationPoller) loop() {
	defer close(p.doneCh)
	tick := time.NewTicker(p.opts.Interval)
	defer tick.Stop()

	// 起头立刻 check 一次，让首次 fire 不等一个完整 interval。
	_ = p.CheckOnce()

	for {
		select {
		case <-p.stopCh:
			return
		case <-tick.C:
			_ = p.CheckOnce()
		}
	}
}
