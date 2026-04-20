package wechat

import (
	"os"
	"path/filepath"
)

// isWorkCopyUpToDate 检查 workdir 里的解密副本是否仍然是最新：
// 存在 + mtime >= source dbFile 的 mtime。
//
// 语义：
//   - true：workdir 副本新鲜，可以 skip 本次重解
//   - false：workdir 副本不存在 / 更旧 / stat 失败（保守重解）
//
// 为什么 mtime 比较就够：SQLite checkpoint / WAL merge 会让源 .db 文件 mtime
// 更新，我们的解密输出 rename 到 target 时 target mtime 也会更新。两者同步。
// 如果微信在 chatlog 停机期间写过 db，source mtime > output mtime → 需要重解。
// 反之说明没改过，workdir 副本依然对应最新状态。
//
// 注意：WAL 文件 (.db-wal) 不在 DecryptDBFiles 扫描范围内（pattern=.*\.db$），
// checkpoint 发生时 main .db 的 mtime 会更新，所以 WAL 触发的变化也能被捕获。
func (s *Service) isWorkCopyUpToDate(dbFile string) bool {
	workDir := s.conf.GetWorkDir()
	if workDir == "" {
		return false
	}
	relPath, err := filepath.Rel(s.conf.GetDataDir(), dbFile)
	if err != nil {
		return false
	}
	output := filepath.Join(workDir, relPath)
	outputStat, err := os.Stat(output)
	if err != nil {
		return false
	}
	sourceStat, err := os.Stat(dbFile)
	if err != nil {
		return false
	}
	// output.mtime >= source.mtime 视为最新
	return !outputStat.ModTime().Before(sourceStat.ModTime())
}
