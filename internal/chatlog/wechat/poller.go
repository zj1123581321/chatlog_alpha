package wechat

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// DefaultDecryptPollInterval 是 watcher IntervalPoller 的默认轮询节奏。
//
// 5 分钟在数据新鲜度 (Hard Constraint #3 接受 5-15min) 与 IO 干扰
// (单次扫描对 db_storage/ 几十文件的 stat 风暴 ~10ms) 之间取平衡。
//
// 用 var 而非 const 让测试可以注入更短值；生产保持 5min。
var DefaultDecryptPollInterval = 5 * time.Minute

// ChangeCallback 在 IntervalPoller 检测到匹配文件 mtime 前进或新增时被调用。
//
// 调用是同步的：从 TickOnce 的 caller goroutine 直接 invoke。生产路径上
// 由 Service.DecryptFileCallback 处理，自身会 spawn waitAndProcess goroutine
// 完成 debounce + 解密，所以同步 invoke 不会拖累 polling 节奏。
type ChangeCallback func(path string) error

// IntervalPoller 是 Step 3 引入的"基于轮询的文件变化检测器"，替代
// pkg/filemonitor 的 fsnotify-based watch。
//
// Why polling: spec §1.2.2 + Eng Review A1 — fsnotify 在 Windows 上需要 watch
// directory handle，与微信对 .db 文件的原子操作 (rename/delete/recreate)
// 在内核层有干扰，会让用户感知"打开图片卡顿"。Step 3 把 watcher 端的变化
// 检测从"事件驱动"改成"间隔扫描"，彻底消除 watch handle。
//
// Semantics（与移除前的 fsnotify 用法对齐）：
//   - 第一次 TickOnce 是 **baseline** —— 仅记录现有文件 mtime，不触发 callback。
//     和 fsnotify 的"only events after Start"语义对齐，避免 chatlog 重启即触发
//     全量重解（首次全量由独立的 firstFullDecrypt 路径负责）。
//   - 后续 tick：发现新文件、或 mtime 严格前进的文件，按发现顺序同步触发 callback。
//   - 文件删除：从 lastSeen 移除，不触发 callback (与 DecryptFileCallback 忽略
//     fsnotify Remove 的行为一致)。
//   - rootDir 不存在：返回 nil 错误 + 空快照（微信关闭/盘卸载是合理状态，
//     下次 tick 自然恢复，不应让 watcher 整个崩）。
//
// Lifecycle：
//   - NewIntervalPoller 仅做参数校验，不启动 goroutine
//   - Start 起一个后台 goroutine，先跑一次 baseline TickOnce 然后按 interval 循环
//   - Stop 取消 ctx 并等待 goroutine 退出
//   - 重复 Start 报错；重复 Stop 是 no-op
//   - 测试可直接 TickOnce 而不 Start，跳过 goroutine 拿到确定性
type IntervalPoller struct {
	rootDir   string
	pattern   *regexp.Regexp
	blacklist []string
	interval  time.Duration
	callback  ChangeCallback

	mu        sync.Mutex
	lastSeen  map[string]time.Time
	baselined bool

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewIntervalPoller 构造一个 poller。interval<=0 时回退到 DefaultDecryptPollInterval。
//
// pattern 是基于 filepath.Base 的正则；blacklist 是基于 rootDir 相对路径的子串
// (与 pkg/filemonitor.FileGroup 行为一致，便于直接复用现有调用点的过滤参数)。
func NewIntervalPoller(rootDir, pattern string, blacklist []string, interval time.Duration, cb ChangeCallback) (*IntervalPoller, error) {
	if cb == nil {
		return nil, errors.New("IntervalPoller: callback must not be nil")
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("IntervalPoller: invalid pattern %q: %w", pattern, err)
	}
	if interval <= 0 {
		interval = DefaultDecryptPollInterval
	}
	return &IntervalPoller{
		rootDir:   filepath.Clean(rootDir),
		pattern:   re,
		blacklist: append([]string{}, blacklist...),
		interval:  interval,
		callback:  cb,
		lastSeen:  make(map[string]time.Time),
	}, nil
}

// TickOnce 执行一次 scan + diff，按需触发 callback。
//
// 暴露给测试以避免依赖 goroutine + ticker 调度，达到确定性；生产由 Start
// 启的后台循环每 interval 调一次。
//
// ctx 检查点：
//   - 进入函数时不检查（让一次完整 tick 跑完比中途 abort 更合理）
//   - 在 callback dispatch 循环里每次 invoke 前检查 ctx.Err，方便 Stop 快速生效
func (p *IntervalPoller) TickOnce(ctx context.Context) error {
	seen, err := p.scan()
	if err != nil {
		return err
	}

	p.mu.Lock()
	if !p.baselined {
		// 第一次：只 baseline 不触发回调
		p.lastSeen = seen
		p.baselined = true
		p.mu.Unlock()
		return nil
	}

	// 找出需要触发的路径（新文件 or mtime 前进）
	var fires []string
	for path, mtime := range seen {
		prev, existed := p.lastSeen[path]
		if !existed || mtime.After(prev) {
			fires = append(fires, path)
		}
	}
	// lastSeen 整体替换：自动处理删除（不在新 seen 里的从 map 清掉）
	p.lastSeen = seen
	p.mu.Unlock()

	// 在锁外触发 callback：DecryptFileCallback 内部会持有 service mutex，
	// 在 poller 锁里同步调用会引入跨锁顺序约束，未来踩坑风险高。
	for _, path := range fires {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.callback(path); err != nil {
			log.Warn().Err(err).Str("path", path).Msg("[poller] callback returned error")
		}
	}
	return nil
}

// scan 走一次 rootDir，返回所有匹配文件的 path -> mtime 快照。
//
// 容错：
//   - rootDir 整个不存在：返回空 map + nil err（合理的"未启动微信/盘卸载"状态）
//   - 子目录不可读：SkipDir 跳过，继续扫其他
//   - 单文件 stat 失败 (race 删除): 跳过该文件，不报错
func (p *IntervalPoller) scan() (map[string]time.Time, error) {
	seen := make(map[string]time.Time)

	// rootDir 不存在 → 直接返回空快照
	if _, err := os.Stat(p.rootDir); err != nil {
		if os.IsNotExist(err) {
			return seen, nil
		}
		return nil, fmt.Errorf("stat rootDir %q: %w", p.rootDir, err)
	}

	walkErr := filepath.WalkDir(p.rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 不可读子目录跳过；其他读错（含 root 自身）已在外层 Stat 兜底
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !p.matches(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 文件可能在 walk 期间被删
		}
		seen[path] = info.ModTime()
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return seen, nil
}

// matches: pattern 匹配 base + blacklist 过滤。
func (p *IntervalPoller) matches(path string) bool {
	if !p.pattern.MatchString(filepath.Base(path)) {
		return false
	}
	rel, err := filepath.Rel(p.rootDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	// 规范化分隔符做子串匹配，避免 Windows \ vs Linux / 在 blacklist 上踩坑
	relSlash := filepath.ToSlash(rel)
	for _, item := range p.blacklist {
		if item == "" {
			continue
		}
		if strings.Contains(relSlash, item) {
			return false
		}
	}
	return true
}

// Start 起后台 goroutine：先跑一次 baseline TickOnce，然后按 interval 循环。
//
// 重复 Start 报错；用 Stop 后再 Start 也报错（poller 是一次性 lifecycle，
// 与 pkg/filemonitor.FileMonitor 的 Start/Stop 反复语义不同 —— 后者历史
// 复用过 watcher 实例，但 chatlog 在 StopAutoDecrypt 后会丢弃旧 poller、
// StartAutoDecrypt 时新建一个，所以一次性 lifecycle 更简单）。
func (p *IntervalPoller) Start() error {
	p.mu.Lock()
	if p.cancel != nil {
		p.mu.Unlock()
		return errors.New("IntervalPoller: already started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.mu.Unlock()

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		// 立刻跑一次 baseline tick：确保 lastSeen 在 interval 流逝前就建好
		if err := p.TickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Warn().Err(err).Msg("[poller] baseline tick failed")
		}

		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.TickOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
					log.Warn().Err(err).Msg("[poller] tick failed")
				}
			}
		}
	}()
	return nil
}

// Stop 取消 ctx 并等待 goroutine 退出。重复 Stop 是 no-op (返回 nil)。
func (p *IntervalPoller) Stop() error {
	p.mu.Lock()
	cancel := p.cancel
	p.cancel = nil
	p.mu.Unlock()

	if cancel == nil {
		return nil
	}
	cancel()
	p.wg.Wait()
	return nil
}
