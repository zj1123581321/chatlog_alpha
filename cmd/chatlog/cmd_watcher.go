package chatlog

// cmd_watcher.go：Step 7c — 注册 `chatlog watcher` 子命令。
//
// 与 `chatlog server` 是对等的两个 daemon：
//   server  长期跑、提供 HTTP/MCP 查询、读 status.json + dbm 池
//   watcher 长期跑、产 generation 快照、原子 swap status.current_generation
//
// flag 设计与 cmd_server.go 一致（data-dir / data-key / work-dir / platform / version），
// 只增加 watcher 专属的 --interval 与 --schema-check-mode。

import (
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/sjzar/chatlog/internal/chatlog"
)

func init() {
	rootCmd.AddCommand(watcherCmd)
	watcherCmd.PersistentPreRun = initLog
	watcherCmd.PersistentFlags().BoolVar(&Debug, "debug", false, "debug")

	watcherCmd.Flags().StringVarP(&watcherDataDir, "data-dir", "d", "", "wechat data dir")
	watcherCmd.Flags().StringVarP(&watcherDataKey, "data-key", "k", "", "wechat data key (hex)")
	watcherCmd.Flags().StringVarP(&watcherWorkDir, "work-dir", "w", "", "chatlog work dir")
	watcherCmd.Flags().StringVarP(&watcherPlatform, "platform", "p", "", "platform")
	watcherCmd.Flags().IntVarP(&watcherVersion, "version", "v", 0, "wechat major version (3 / 4)")
	watcherCmd.Flags().StringVar(&watcherInterval, "interval", "", "polling interval (e.g. 5m, default 5m)")
	watcherCmd.Flags().StringVar(&watcherSchemaMode, "schema-check-mode", "", "schema check mode: quick (default) or full")
}

var (
	watcherDataDir    string
	watcherDataKey    string
	watcherWorkDir    string
	watcherPlatform   string
	watcherVersion    int
	watcherInterval   string
	watcherSchemaMode string
)

var watcherCmd = &cobra.Command{
	Use:   "watcher",
	Short: "Start watcher daemon (Step 5 generation pipeline)",
	Long: `Long-running daemon that periodically copies + decrypts WeChat dbs into
immutable generation snapshots and atomically swaps current_generation in
status.json. Designed to be supervised by Windows Task Scheduler / NSSM
with RestartOnFailure enabled (see docs/supervisor.md).

Set CHATLOG_LEGACY_DECRYPT=1 to fall back to the in-place DecryptDBFiles
path (one-shot per process invocation) if the new pipeline misbehaves.`,
	Run: func(cmd *cobra.Command, args []string) {
		cleanup := initSingleInstance()
		defer cleanup()

		cmdConf := getWatcherConfig()
		log.Info().Msgf("watcher cmd config: %+v", cmdConf)

		m := chatlog.New()
		if err := m.CommandWatcher("", cmdConf); err != nil {
			log.Err(err).Msg("watcher exited with error")
			return
		}
	},
}

func getWatcherConfig() map[string]any {
	cmdConf := make(map[string]any)
	if watcherDataDir != "" {
		cmdConf["data_dir"] = watcherDataDir
	}
	if watcherDataKey != "" {
		cmdConf["data_key"] = watcherDataKey
	}
	if watcherWorkDir != "" {
		cmdConf["work_dir"] = watcherWorkDir
	}
	if watcherPlatform != "" {
		cmdConf["platform"] = watcherPlatform
	}
	if watcherVersion != 0 {
		cmdConf["version"] = watcherVersion
	}
	if watcherInterval != "" {
		cmdConf["watcher_interval"] = watcherInterval
	}
	if watcherSchemaMode != "" {
		cmdConf["watcher_schema_check_mode"] = watcherSchemaMode
	}
	return cmdConf
}
