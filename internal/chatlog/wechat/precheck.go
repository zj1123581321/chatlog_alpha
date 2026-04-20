package wechat

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjzar/chatlog/internal/wechat/decrypt"

	chatlogerrors "github.com/sjzar/chatlog/internal/errors"
)

// ErrNoDBFile 表示 data dir 的 db_storage 下找不到任何可用于预检的 .db 文件。
// 出现场景：新账号刚登录、微信数据目录还没初始化、用户选错目录。
// 调用方收到此错误应当降级走 skipPrecheck 继续启动自动解密，由运行期熔断兜底。
var ErrNoDBFile = errors.New("no db file found for precheck")

// PickSmallestDBForPrecheck 是 pickSmallestDB 的导出别名，供 manager 包调用。
// 保留 pickSmallestDB 小写版本给 package-internal 用，避免破坏 test 可读性。
func PickSmallestDBForPrecheck(dataDir string) (string, error) {
	return pickSmallestDB(dataDir)
}

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

// DecryptSingleDBForPrecheck 解一个 db 验证密钥是否正确 / 环境是否可用，
// 输出写到 io.Discard 丢弃，不污染 workdir。用于"开启自动解密"按钮路径
// 的秒级预检（替代原先跑全量的 DecryptDBFiles）。
//
// 返回：
//   - nil                          密钥对、文件可读、解密逻辑通畅
//   - ErrAlreadyDecrypted          该文件已经是明文（视为密钥验证通过）
//   - decrypt.* 其他错误           冒泡给调用方走熔断 handler
//
// 调用方需要先 pickSmallestDB 挑一个稳定的 db 文件作为参数。
func (s *Service) DecryptSingleDBForPrecheck(ctx context.Context, dbFile string) error {
	decryptor, err := decrypt.NewDecryptor(s.conf.GetPlatform(), s.conf.GetVersion())
	if err != nil {
		return err
	}

	if err := decryptor.Decrypt(ctx, dbFile, s.conf.GetDataKey(), io.Discard); err != nil {
		if err == chatlogerrors.ErrAlreadyDecrypted {
			return nil
		}
		return err
	}
	return nil
}
