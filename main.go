package main

import (
	stdlog "log"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/cmd/chatlog"
	"github.com/sjzar/chatlog/pkg/util"
)

func main() {
	stdlog.SetFlags(stdlog.LstdFlags | stdlog.Lshortfile)

	// 启动时把进程降为后台优先级，让位给微信的 CPU / IO。
	// 仅 Windows 生效；其他平台为 no-op。
	if err := util.SetBackgroundPriority(); err != nil {
		log.Warn().Err(err).Msg("设置后台进程优先级失败，继续使用默认优先级")
	} else {
		log.Info().Msg("进程优先级已降为 BELOW_NORMAL（后台模式，让位微信）")
	}

	chatlog.Execute()
}
