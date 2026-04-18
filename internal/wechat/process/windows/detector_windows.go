package windows

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/sjzar/chatlog/internal/wechat/model"
)

// initializeProcessInfo 获取进程的数据目录和账户名。
//
// 识别策略：优先找 session.db（V4DBFile，最精确），没有时退而求其次，
// 用任意 `\<wxid_xxx>\db_storage\...` 路径推导。
// 这是因为 Weixin 4.1.5+ 观察到稳态主进程并不持续持有 session.db 的 File HANDLE
// （只保持 message_fts.db / favorite_*.db-wal / login_configv2 等），
// 只认 session.db 会导致 DataDir 一直为空、cache 永远不命中、每秒 p.OpenFiles
// 被反复调用、命中 gopsutil OpenFilesWithContext 的 HANDLE 泄漏 bug。
// 通配 db_storage 让已登录微信几乎一次探测就能拿到 DataDir。
func initializeProcessInfo(p *process.Process, info *model.Process) error {
	files, err := p.OpenFiles()
	if err != nil {
		log.Err(err).Msgf("获取进程 %d 的打开文件失败", p.Pid)
		info.AccountName = fmt.Sprintf("未登录微信_%d", p.Pid)
		return nil
	}

	// 第一遍：精确匹配 session.db（最可靠，能直接拿到完整账号名）
	for _, f := range files {
		if strings.HasSuffix(f.Path, V4DBFile) {
			filePath := stripNTPrefix(f.Path)
			parts := strings.Split(filePath, string(filepath.Separator))
			if len(parts) < 4 {
				log.Debug().Msg("无效的文件路径: " + filePath)
				continue
			}
			info.Status = model.StatusOnline
			info.DataDir = strings.Join(parts[:len(parts)-3], string(filepath.Separator))
			info.AccountName = parts[len(parts)-4]
			return nil
		}
	}

	// 第二遍：任意 db_storage 子文件，反推到 .../xwechat_files/<wxid>/
	for _, f := range files {
		filePath := stripNTPrefix(f.Path)
		idx := strings.Index(filePath, `\db_storage\`)
		if idx < 0 {
			continue
		}
		dataDir := filePath[:idx]
		info.Status = model.StatusOnline
		info.DataDir = dataDir
		info.AccountName = filepath.Base(dataDir)
		return nil
	}

	// 既不是 session.db 也没 db_storage 路径：大概率是 Weixin 的 renderer/gpu
	// 子进程，或是主进程还没登录。
	info.AccountName = fmt.Sprintf("未登录微信_%d", p.Pid)
	return nil
}

// stripNTPrefix 去掉 gopsutil 在 Windows 下给文件路径加的 `\\?\` 前缀。
func stripNTPrefix(path string) string {
	if strings.HasPrefix(path, `\\?\`) {
		return path[4:]
	}
	return path
}
