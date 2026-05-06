package wechat

// watcher_daemon.go：Step 7a，把 Step 5/6 的所有 primitive 装在一起做"一次完整的
// watcher iteration"。caller 负责外层 ticker（5-15min）+ 进程生命周期。
//
// 一次 RunOnce 做的事：
//   1. watchdog Tick （防止 watchdog 误判 hang）
//   2. RunGenerationCycle（5d 编排器：copy + decrypt + schema gate + atomic swap）
//   3. 按 PruneEvery 节奏调 PruneGenerations（5f：grace + retry-cap）
//
// 不做的事：
//   - smart timing / IO yield —— caller 在外层（service.go 已有 ioThrottle）做
//   - polling 检测变化 —— 当前每个 RunOnce 必跑 cycle，是否有新数据由 cycle
//     结果反映；spec §4.1 step 2-3 的 stat-skip 优化属未来扩展
//   - legacy flag 分支 —— caller 在 RunOnce 之外判 IsLegacyDecryptEnabled，
//     true 时走 Service.DecryptDBFiles 旧路径
//
// 这个 daemon 是纯结构层：DecryptFunc 由 caller 注入（生产里包 service.DecryptDBFileExplicit），
// 测试里包一个生成 SQLite 的假实现。

import (
	"errors"
	"fmt"
	"time"
)

// WatcherDaemonOpts 配置一个 daemon 实例。Watchdog 可选；其他必填。
type WatcherDaemonOpts struct {
	WorkDir string
	DataDir string

	// DBs 是本次 cycle 要处理的所有 db。每个含 RelPath + 业务级 SchemaSpec。
	DBs []DBJob

	// DecryptFunc 把 rawDir/db_storage/<rel> 解密到 dstDir/db_storage/<rel>。
	// 必填；nil → RunOnce 报错。
	DecryptFunc func(rawDir, dstDir string) error

	SchemaMode SchemaCheckMode
	WatcherPID int

	// PruneEvery：每 N 次 RunOnce 跑一次 prune。0 → 默认 1（每次都跑）。
	// 设大些可以减少 prune 系统调用频率（默认 1 已经足够轻量）。
	PruneEvery int

	// Prune 参数（透传到 PruneGenerations）。0 → 各自的 default。
	PruneGracePeriod time.Duration
	PruneRetryDelay  time.Duration
	PruneRetryCap    time.Duration
}

// WatcherDaemon 是无锁的"按需调用"结构 —— 不持有 goroutine、不自己 ticker。
// caller 决定何时调 RunOnce（任务计划程序触发外层、定时器、手动等）。
type WatcherDaemon struct {
	Opts     WatcherDaemonOpts
	Watchdog *Watchdog // 可选：每次 RunOnce 起头会 Tick，无则跳过

	cycleCount int
}

// RunOnce 执行一次完整 watcher iteration。
// 返回的 GenerationCycleResult 反映本次 cycle 结果（OutcomeSwapped/Corrupt/Skipped）。
// error 仅用于"完全没法跑"的致命情况（构造缺陷如 DecryptFunc=nil）。
func (d *WatcherDaemon) RunOnce() (GenerationCycleResult, error) {
	if d.Opts.DecryptFunc == nil {
		return GenerationCycleResult{}, errors.New("watcher daemon: DecryptFunc is required")
	}

	// Watchdog Tick 放在最前 + 最后各一次：
	//   - 起头 Tick 让本次 iteration 在 watchdog 视角下"重置时钟"
	//   - 收尾再 Tick 让长 cycle（FirstFull 30min+）也保持心跳
	// 缺一不可：长 cycle 中段没 tick，watchdog 会误触发；只有起头 tick，
	// caller 间隔过长时也会触发。
	if d.Watchdog != nil {
		d.Watchdog.Tick()
	}
	defer func() {
		if d.Watchdog != nil {
			d.Watchdog.Tick()
		}
	}()

	res, err := RunGenerationCycle(CycleInput{
		WorkDir:     d.Opts.WorkDir,
		DataDir:     d.Opts.DataDir,
		DBs:         d.Opts.DBs,
		DecryptFunc: d.Opts.DecryptFunc,
		SchemaMode:  d.Opts.SchemaMode,
		WatcherPID:  d.Opts.WatcherPID,
	})
	if err != nil {
		return res, fmt.Errorf("watcher daemon: cycle: %w", err)
	}

	d.cycleCount++

	// Prune 节奏判定。PruneEvery <= 0 → 默认每次都跑。
	pruneEvery := d.Opts.PruneEvery
	if pruneEvery <= 0 {
		pruneEvery = 1
	}
	if d.cycleCount%pruneEvery == 0 {
		// 即使 cycle 失败也跑 prune：上一次 cycle 成功后留下的 inactive 老 gen
		// 不该被本次 cycle 失败拖延清理。
		current := ""
		if res.Outcome == OutcomeSwapped {
			current = res.GenerationID
		} else {
			// cycle 失败：从 status.json 读 current 用作 prune 的 active。
			if st, _ := ReadStatus(d.Opts.WorkDir); st.CurrentGeneration != "" {
				current = st.CurrentGeneration
			}
		}
		if current != "" {
			_, _ = PruneGenerations(PruneOpts{
				WorkDir:     d.Opts.WorkDir,
				CurrentGen:  current,
				GracePeriod: d.Opts.PruneGracePeriod,
				RetryDelay:  d.Opts.PruneRetryDelay,
				RetryCap:    d.Opts.PruneRetryCap,
			})
		}
	}

	return res, nil
}
