package chatlog

// manager_watcher.go：Step 7c watcher daemon entry point。把 Step 5 generation
// 管道、Step 6 watchdog、Step 7a/b 编排器 + 解密 adapter 全部 wire 到一个 cobra
// 子命令背后的 Manager 方法上。
//
// 调用关系：
//   cmd/chatlog/cmd_watcher.go (cobra)
//     └─> Manager.CommandWatcher(configPath, cmdConf)
//          ├─ LoadServiceConfig（同 CommandHTTPServer）
//          ├─ wechat.IsLegacyDecryptEnabled() ？走旧 DecryptDBFiles 直接退出
//          ├─ wechat.BuildDBJobs() + defaultSchemaLookup 列出本机所有 db 任务
//          ├─ wechat.DetectAndMigrate() 一次性把 legacy db_storage 搬进 generations/
//          ├─ wechat.Watchdog Start（in-proc goroutine + LockOSThread）
//          └─ ticker 循环：daemon.RunOnce() 每 interval 跑一次 cycle + prune

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/wechat"
)

// DefaultWatcherInterval 是 ticker 周期默认值（spec §1.2.5：5-15min 内任选）。
const DefaultWatcherInterval = 5 * time.Minute

// CommandWatcher 是 watcher 子命令的入口。阻塞至收到 SIGTERM/SIGINT 才返回。
func (m *Manager) CommandWatcher(configPath string, cmdConf map[string]any) error {
	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	workDir := m.sc.GetWorkDir()
	if dataDir == "" || workDir == "" {
		return fmt.Errorf("watcher: data_dir and work_dir are both required")
	}
	if m.sc.GetDataKey() == "" {
		return fmt.Errorf("watcher: data_key is required")
	}

	log.Info().Msgf("watcher config: %+v", m.sc)

	m.wechat = wechat.NewService(m.sc)

	// 紧急 rollback：CHATLOG_LEGACY_DECRYPT 设了 → 跑旧 DecryptDBFiles 一次后退出。
	// 让 watcher 在新管道出问题时仍能保留"跑一次解密"的最低能力，外层 supervisor
	// 30s 后还会重启它，等同于旧的 in-place 模式 polling cadence。
	if wechat.IsLegacyDecryptEnabled() {
		log.Warn().Str("env", wechat.LegacyDecryptEnv).
			Msg("[watcher] legacy flag set — running legacy DecryptDBFiles once then exiting")
		return m.wechat.DecryptDBFiles()
	}

	// 列出所有 db 任务 + 业务级 schema 期望
	jobs, err := wechat.BuildDBJobs(dataDir, defaultSchemaLookup)
	if err != nil {
		return fmt.Errorf("watcher: build dbjobs: %w", err)
	}
	if len(jobs) == 0 {
		return fmt.Errorf("watcher: no .db files under %s/db_storage", dataDir)
	}
	log.Info().Int("dbs", len(jobs)).Msg("[watcher] enumerated db jobs")

	// Step 5g：把 legacy work_dir/db_storage 一次性搬进 generations/
	migRes, err := wechat.DetectAndMigrate(wechat.MigrationOpts{
		WorkDir:    workDir,
		DBs:        jobs,
		WatcherPID: os.Getpid(),
	})
	if err != nil {
		return fmt.Errorf("watcher: migrate: %w", err)
	}
	log.Info().
		Str("outcome", string(migRes.Outcome)).
		Str("gen", migRes.GenerationID).
		Str("reason", migRes.Reason).
		Msg("[watcher] migration result")

	// Step 6：watchdog 防主循环 hang，配 in-proc Tick
	wdog := &wechat.Watchdog{
		PhaseFn: func() wechat.AutoDecryptPhase { return m.wechat.GetPhase() },
	}
	if err := wdog.Start(); err != nil {
		return fmt.Errorf("watcher: watchdog start: %w", err)
	}
	defer wdog.Stop()

	// Step 7a：daemon 编排单次 iteration
	daemon := &wechat.WatcherDaemon{
		Opts: wechat.WatcherDaemonOpts{
			WorkDir:     workDir,
			DataDir:     dataDir,
			DBs:         jobs,
			DecryptFunc: wechat.NewServiceDecryptFunc(m.wechat),
			WatcherPID:  os.Getpid(),
			// 每 4 个 cycle 跑一次 prune：5min cycle × 4 = 20min/prune，
			// 老 generation 平均存活 30-40min（充分覆盖 server invalidate 的 30s polling 间隔）。
			PruneEvery: 4,
		},
		Watchdog: wdog,
	}

	interval := watcherIntervalFromConf(cmdConf)
	log.Info().Dur("interval", interval).Msg("[watcher] entering main loop")

	// 起头立刻跑一次：让用户启动后不用等一个完整 interval 才看到第一份解密。
	if res, err := daemon.RunOnce(); err != nil {
		log.Error().Err(err).Msg("[watcher] initial RunOnce error")
	} else {
		log.Info().
			Str("outcome", string(res.Outcome)).
			Str("gen", res.GenerationID).
			Str("reason", res.Reason).
			Msg("[watcher] initial cycle done")
	}

	// signal 监听 + ticker
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tick := time.NewTicker(interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("[watcher] signal received, exiting")
			return nil
		case <-tick.C:
			res, err := daemon.RunOnce()
			if err != nil {
				log.Error().Err(err).Msg("[watcher] RunOnce error")
				continue
			}
			log.Info().
				Str("outcome", string(res.Outcome)).
				Str("gen", res.GenerationID).
				Str("reason", res.Reason).
				Msg("[watcher] cycle done")
		}
	}
}

// watcherIntervalFromConf 从 cmdConf 取 interval；缺省回 DefaultWatcherInterval。
// 接受 string ("5m") 或 time.Duration。
func watcherIntervalFromConf(cmdConf map[string]any) time.Duration {
	if v, ok := cmdConf["watcher_interval"]; ok {
		switch t := v.(type) {
		case time.Duration:
			if t > 0 {
				return t
			}
		case string:
			if d, err := time.ParseDuration(t); err == nil && d > 0 {
				return d
			}
		}
	}
	return DefaultWatcherInterval
}

// defaultSchemaLookup 是默认的 db→SchemaSpec 映射。
//
// 当前覆盖：
//   - message_*.db → Timestamp 表必须存在 + smoke select Timestamp
//     （4/25 损坏现场就是这张表 race 后没了，所以这是最值得 catch 的一类）
//
// 其他 db（contact.db / session.db / openIMContact.db 等）暂不写死期望表名 —
// SchemaSpec 为空时 CheckSchema 仍会跑 quick_check(50) 兜底结构损坏。后续摸清
// 各类 db 的稳定 schema 后可以加进来；过度具体反而在微信版本升级时容易误报。
func defaultSchemaLookup(rel string) wechat.SchemaSpec {
	base := filepath.Base(rel)
	if strings.HasPrefix(base, "message_") && strings.HasSuffix(base, ".db") {
		return wechat.SchemaSpec{
			ExpectedTables: []string{"Timestamp"},
			SmokeQuery:     "SELECT count(*) FROM Timestamp",
		}
	}
	return wechat.SchemaSpec{}
}
