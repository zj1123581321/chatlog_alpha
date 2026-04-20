package wechat

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoDBFile 表示 data dir 的 db_storage 下找不到任何可用于预检的 .db 文件。
// 出现场景：新账号刚登录、微信数据目录还没初始化、用户选错目录。
// 调用方收到此错误应当降级走 skipPrecheck 继续启动自动解密，由运行期熔断兜底。
var ErrNoDBFile = errors.New("no db file found for precheck")

// pickSmallestDB 从 dataDir/db_storage 里挑一个 .db 文件用于预检解密。
//
// 预检目标是"解一个 db 验证密钥正确"，所以优选稳定、存在概率高、体积小的文件。
// 查找顺序：
//  1. db_storage/session/session.db   最稳定、最小
//  2. db_storage/message/message_0.db 主消息库首片
//  3. db_storage/ 下最小的 .db        兜底（排除 *_fts.db 全文索引库）
//
// 返回找到的绝对路径，或 ErrNoDBFile（整个 db_storage 下一个可用 db 都没有）。
// isUsableDB 检查文件是否可用作预检目标：存在、非目录、非空。
// 0 字节文件可能是占位符，不能验证密钥。
func isUsableDB(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() <= 0 {
		return false
	}
	return true
}

func pickSmallestDB(dataDir string) (string, error) {
	dbStorage := filepath.Join(dataDir, "db_storage")

	// Tier 1: session.db
	sessionPath := filepath.Join(dbStorage, "session", "session.db")
	if isUsableDB(sessionPath) {
		return sessionPath, nil
	}

	// Tier 2: message_0.db
	msgPath := filepath.Join(dbStorage, "message", "message_0.db")
	if isUsableDB(msgPath) {
		return msgPath, nil
	}

	// Tier 3: walk & pick smallest .db, excluding fts
	var (
		smallestPath string
		smallestSize int64 = -1
	)
	err := filepath.WalkDir(dbStorage, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // 忽略遍历错误（比如权限），继续扫剩下的
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".db") {
			return nil
		}
		if strings.Contains(name, "fts") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		size := info.Size()
		if size <= 0 {
			return nil
		}
		if smallestSize < 0 || size < smallestSize {
			smallestSize = size
			smallestPath = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if smallestPath == "" {
		return "", ErrNoDBFile
	}
	return smallestPath, nil
}
