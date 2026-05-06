package wechat

// legacy_flag.go：Step 5h --legacy-decrypt 紧急 rollback flag
// （architecture-rework-2026-05-06.md §1.7）。
//
// 存在的唯一目的：Step 5 generation 管道在生产出问题时的 escape hatch。
// 不需要重新打包发布；运维设环境变量 → 重启进程 → 回到旧 in-place decrypt 路径。
//
// 上游约定：
//   - 在 watcher 主循环 spawn cycle 之前调 IsLegacyDecryptEnabled()。
//   - true → 走旧 service.DecryptDBFiles in-place 路径，跳过 RunGenerationCycle。
//   - false（默认）→ 走 RunGenerationCycle 新管道。
//
// 故意不放进 Config interface：每加一个 Config 方法都要更新两份实现 + 两份 mock，
// rollback flag 本质是临时紧急开关，环境变量比 config schema 更容易在事故现场翻动。

import (
	"os"
	"strings"
)

// LegacyDecryptEnv 是控制开关的环境变量名。
const LegacyDecryptEnv = "CHATLOG_LEGACY_DECRYPT"

// IsLegacyDecryptEnabled 读环境变量返回是否启用旧 decrypt 路径。
// 接受 "1" / "true" / "yes" / "on"（大小写不敏感、首尾空白忽略）；其余皆为 false。
func IsLegacyDecryptEnabled() bool {
	return parseLegacyDecryptFlag(os.Getenv(LegacyDecryptEnv))
}

// parseLegacyDecryptFlag 把字符串规范化后判定是否为 truthy。
// 抽出来便于 unit test 直接覆盖各种输入而不污染进程环境变量。
func parseLegacyDecryptFlag(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
