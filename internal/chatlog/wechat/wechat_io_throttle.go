package wechat

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
)

// WeChatIoSampler 抽象"取微信进程当前累计 IO 操作数"的能力。
//
// 实现方式：
//   - 真实环境：gopsutilSampler 通过 process.Process.IOCounters() 拿
//     ReadCount + WriteCount（Windows 上对应 GetProcessIoCounters API）
//   - 测试：mockSampler 按预设序列返回值
//
// 返回的是"自进程启动以来的累计 ops 数"（不是 IOPS 速率），由 IoThrottle
// 内部用相邻两次差值除以采样间隔算速率。
type WeChatIoSampler interface {
	SampleIoOps(ctx context.Context) (totalOps uint64, err error)
}

// IoThrottle 在解密前等微信 IO 安静下来。
//
// 工作机制（方案 4）：
//  1. 周期性采样微信进程的累计 IO ops 数
//  2. 相邻两次差值除以间隔得到 IOPS 速率
//  3. IOPS < threshold 时认为"用户没在主动操作微信"，再等 cooldown 确认后放行
//  4. IOPS >= threshold 持续到 maxWait 时强制放行（数据不能永远不更新）
//  5. sampler 报错时直接放行（不能因为探测失败拖死解密；IO 优先级方案 1 兜底）
//
// 与方案 1（线程级 IO Priority）配合：
//   - 方案 4 在源头避免 chatlog 在微信高 IO 时挤进 IO 队列
//   - 方案 1 在万一挤进去时，让内核调度器优先服务微信
//   - 双层保险，单独任一层失效都不会回到"微信打开新图片卡顿"的状态
type IoThrottle struct {
	sampler   WeChatIoSampler
	interval  time.Duration
	cooldown  time.Duration
	maxWait   time.Duration
	threshold float64 // IOPS 阈值

	isActive atomic.Bool // 最近一次观察是否 busy（供日志/状态查询）
}

// IoThrottleOption 是 functional option 模式的配置入口。
type IoThrottleOption func(*IoThrottle)

// 默认参数（生产环境）：
//   - interval=1s：每秒采一次微信 IO，开销可忽略
//   - cooldown=2s：观察到安静后再等 2s 确认（避免微信只是短暂喘气）
//   - maxWait=30s：再忙也不能拖超过 30s（10min 解密兜底是更外层）
//   - threshold=200 IOPS：经验值，文字消息 < 50，点开图片 > 1000
const (
	defaultIoThrottleInterval  = 1 * time.Second
	defaultIoThrottleCooldown  = 2 * time.Second
	defaultIoThrottleMaxWait   = 30 * time.Second
	defaultIoThrottleThreshold = 200.0
)

// NewIoThrottle 构造一个 throttle。sampler=nil 时所有 Wait 立即返回（no-op）。
func NewIoThrottle(sampler WeChatIoSampler, opts ...IoThrottleOption) *IoThrottle {
	t := &IoThrottle{
		sampler:   sampler,
		interval:  defaultIoThrottleInterval,
		cooldown:  defaultIoThrottleCooldown,
		maxWait:   defaultIoThrottleMaxWait,
		threshold: defaultIoThrottleThreshold,
	}
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func WithIoThrottleInterval(d time.Duration) IoThrottleOption {
	return func(t *IoThrottle) { t.interval = d }
}
func WithIoThrottleCooldown(d time.Duration) IoThrottleOption {
	return func(t *IoThrottle) { t.cooldown = d }
}
func WithIoThrottleMaxWait(d time.Duration) IoThrottleOption {
	return func(t *IoThrottle) { t.maxWait = d }
}
func WithIoThrottleThreshold(iops float64) IoThrottleOption {
	return func(t *IoThrottle) { t.threshold = iops }
}

// IsActive 返回最近一次观察微信是否处于活跃 IO（IOPS >= threshold）。
// nil sampler 永远返回 false。
func (t *IoThrottle) IsActive() bool {
	if t.sampler == nil {
		return false
	}
	return t.isActive.Load()
}

// WaitForQuiet 阻塞直到满足以下条件之一：
//   - 微信 IOPS 低于 threshold 持续 cooldown 时长 → 返回 nil
//   - 累计等待超过 maxWait → 返回 nil（让上层继续；IO 优先级方案 1 兜底）
//   - sampler 持续报错 → 返回 nil（无法探测则放行）
//   - ctx 取消 → 返回 ctx.Err()
//
// nil sampler 直接返回 nil。
func (t *IoThrottle) WaitForQuiet(ctx context.Context) error {
	if t.sampler == nil {
		return nil
	}

	// baseline：第一次 sample 不能算速率，只记录起点
	prev, err := t.sampler.SampleIoOps(ctx)
	if err != nil {
		log.Debug().Err(err).Msg("[ioThrottle] baseline sample failed, treat as quiet")
		t.isActive.Store(false)
		return nil
	}

	start := time.Now()
	for {
		// maxWait 兜底：早于 sleep 检查，避免在 maxWait 之后再睡一轮
		if time.Since(start) >= t.maxWait {
			log.Debug().Dur("waited", time.Since(start)).Msg("[ioThrottle] maxWait reached, releasing")
			return nil
		}

		if err := sleepCtx(ctx, t.interval); err != nil {
			return err
		}

		cur, err := t.sampler.SampleIoOps(ctx)
		if err != nil {
			log.Debug().Err(err).Msg("[ioThrottle] sample failed, treat as quiet")
			t.isActive.Store(false)
			return nil
		}

		iops := computeIops(prev, cur, t.interval)
		prev = cur

		if iops < t.threshold {
			t.isActive.Store(false)
			// cooldown 静默期：微信安静后再等一会儿确认，避免抖动
			if err := sleepCtx(ctx, t.cooldown); err != nil {
				return err
			}
			return nil
		}
		t.isActive.Store(true)
	}
}

// computeIops 算 IOPS 速率。处理 uint64 回绕（极少见，但理论上可能）：
// 进程重启后 IOCounters 归零，cur < prev 时返回 0（视为安静）。
func computeIops(prev, cur uint64, interval time.Duration) float64 {
	if cur < prev {
		return 0
	}
	delta := cur - prev
	return float64(delta) / interval.Seconds()
}

// sleepCtx 是可被 ctx 取消的 sleep。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

