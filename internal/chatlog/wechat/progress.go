package wechat

import (
	"sync"
	"time"
)

// ProgressEvent 是后台解密任务向订阅者广播的单条进度快照。
//
// Codex outside voice Tension #4 决策：Phase 字段区分 first_full /
// incremental / recovery_scan，避免 UI 把 recovery 进度误显示成 first-full。
type ProgressEvent struct {
	Phase       AutoDecryptPhase // 进度归属的阶段
	FilesDone   int              // 已解密的文件数
	FilesTotal  int              // 总文件数（预扫得出）
	BytesDone   int64            // 已解密的字节数
	BytesTotal  int64            // 总字节数（预扫得出）
	CurrentFile string           // 当前正在处理的文件（绝对路径），供日志 & UI 展示
	StartedAt   time.Time        // 任务开始时间，本次任务所有 event 一致
	UpdatedAt   time.Time        // 本 event 发布时间
}

// ProgressPublisher 是 cap=1 broadcast 发布者：每个订阅者拿独立 chan (cap=1)，
// 慢消费者 select-default 丢旧保新（Codex T1 决策）。
//
// 语义：
//   - 进度数据单调，消费者只在意"最新值"，中间点丢了不影响终态
//   - TUI 被 cmd.exe 卡顿 / HTTP poll 延迟 → 不阻塞 producer
//   - 每订阅者独立 chan，不互相影响（codex 指出的 "work queue 伪装 pub-sub" 陷阱）
//   - Close 后 Publish no-op，订阅者 range 自然退出
type ProgressPublisher struct {
	mu     sync.RWMutex
	subs   []chan ProgressEvent
	closed bool
}

// NewProgressPublisher 构造一个空的发布者。调用方应在任务结束时 Close()。
func NewProgressPublisher() *ProgressPublisher {
	return &ProgressPublisher{}
}

// Subscribe 返回一个 cap=1 接收 channel 和 cancel 闭包。
// cancel 幂等；cancel 后 channel 被 close，订阅者 range 自然退出。
// 典型用法：
//
//	ch, cancel := pub.Subscribe()
//	defer cancel()
//	for evt := range ch {
//	    // 处理 evt
//	}
func (p *ProgressPublisher) Subscribe() (<-chan ProgressEvent, func()) {
	ch := make(chan ProgressEvent, 1)

	p.mu.Lock()
	p.subs = append(p.subs, ch)
	p.mu.Unlock()

	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			for i, s := range p.subs {
				if s == ch {
					p.subs = append(p.subs[:i], p.subs[i+1:]...)
					close(ch)
					return
				}
			}
		})
	}
	return ch, cancel
}

// Publish 向所有订阅者广播一个 event。
// 对每个订阅者：若 chan 满则丢弃旧值再塞新值（keep-latest）。
// Close 后的 Publish 是 no-op（不 panic）。
func (p *ProgressPublisher) Publish(e ProgressEvent) {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return
	}
	// 复制订阅者快照避免在发布期间持锁（防止慢订阅者被 drain 时阻塞其他订阅者）
	subs := make([]chan ProgressEvent, len(p.subs))
	copy(subs, p.subs)
	p.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- e:
			// 送达
		default:
			// 订阅者 chan 满：drain 旧值再塞新值（keep-latest）
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- e:
			default:
				// 再失败（罕见：其他 producer 抢位），放弃本条
			}
		}
	}
}

// Close 关闭所有订阅者 chan。幂等。
// Close 后 Publish 变 no-op，Subscribe 仍可返回但收不到 event。
func (p *ProgressPublisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	for _, ch := range p.subs {
		close(ch)
	}
	p.subs = nil
}
