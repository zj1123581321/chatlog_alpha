package chatlog

import (
	"fmt"

	"github.com/sjzar/chatlog/internal/chatlog/wechat"
)

// buildAutoDecryptText 根据 phase + progress 构造 TUI 状态栏显示文本。
//
// 纯函数，便于单测（不依赖 tview / ctx / Manager）。
//
// 显示规则（按 Codex T2 决策：lifecycle 显式展示，不让用户以为"已开启"就万事大吉）：
//
//	Idle + !enabled           "[未开启]"
//	Idle + enabled (瞬时)     "[恢复中]"      recovery 路径的短窗口
//	Precheck                  "[预检中]"       ~1-2s
//	FirstFull + 有进度         "[首次全量] 12/42 (28%, 约 5 分钟)"
//	FirstFull 无进度快照        "[首次全量] 准备中..."
//	Live                      "[已开启]" 或 "[已开启] 60000ms"
//	Failed                    "[已失败]"
//	Stopping                  "[停止中]"
func buildAutoDecryptText(
	phase wechat.AutoDecryptPhase,
	progress *wechat.ProgressEvent,
	enabled bool,
	debounceMs int,
) string {
	switch phase {
	case wechat.PhasePrecheck:
		return "[yellow][预检中][white]"

	case wechat.PhaseFirstFull:
		if progress == nil || progress.BytesTotal <= 0 {
			return "[yellow][首次全量][white] 准备中..."
		}
		pct := float64(progress.BytesDone) / float64(progress.BytesTotal) * 100
		etaStr := ""
		if !progress.StartedAt.IsZero() {
			etaStr = wechat.NewETACalculator(progress.StartedAt).Format(progress.BytesDone, progress.BytesTotal)
		}
		if etaStr != "" {
			return fmt.Sprintf("[yellow][首次全量][white] %d/%d (%.0f%%, %s)",
				progress.FilesDone, progress.FilesTotal, pct, etaStr)
		}
		return fmt.Sprintf("[yellow][首次全量][white] %d/%d (%.0f%%)",
			progress.FilesDone, progress.FilesTotal, pct)

	case wechat.PhaseLive:
		if debounceMs > 0 {
			return fmt.Sprintf("[green][已开启][white] %dms", debounceMs)
		}
		return "[green][已开启][white]"

	case wechat.PhaseFailed:
		return "[red][已失败][white]"

	case wechat.PhaseStopping:
		return "[yellow][停止中][white]"

	default: // PhaseIdle or unknown
		if enabled {
			// autoDecrypt flag 还在 但 phase=Idle：recovery 刚启动的瞬时窗口
			return "[yellow][恢复中][white]"
		}
		return "[未开启]"
	}
}
