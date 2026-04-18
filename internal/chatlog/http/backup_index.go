package http

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
)

// BackupIndex 维护 backup 目录的两张反查表:
//
//   - talkerToDir: 当目录名尾部括号里直接是 talker 标识 (xxx@chatroom 或 wxid_xxx)
//     时, 自动建立 talker → 绝对目录 的映射。无需用户配置。
//   - hexToDir:    当目录名尾部括号里是 8 位十六进制 (hook 软件自行 hash 生成) 时,
//     建立 hex → 绝对目录 的映射。用户需通过 BackupFolderMap 配置
//     talker → hex 才能联动。
//
// 两表查询顺序: 先 talkerToDir, 后 folderMap→hexToDir, 皆 miss 才返回 ok=false。
// 所有方法线程安全, 读多写少场景使用 sync.RWMutex。
//
//   启动流程:
//   NewService ── 构造 BackupIndex ── Scan() 一次 (几十 ms 量级)
//         │
//         └─ 后续请求通过 Resolve(talker) 做 O(1) map 查询
//
//   刷新: /api/v1/cache/clear 触发重扫 + folderMap 热更
type BackupIndex struct {
	mu         sync.RWMutex
	backupRoot string
	folderMap  map[string]string // talker → hex (upper), from user config

	talkerToDir map[string]string // talker (as-is) → absolute dir
	hexToDir    map[string]string // upper hex → absolute dir

	// stats for /api/v1/backup/stats 和启动日志
	chatroomCount int
	hexCount      int
	unknownCount  int
}

// folderParenRegex 提取目录名末尾的 "(xxx)" 括号内容。
var folderParenRegex = regexp.MustCompile(`\(([^()]+)\)$`)

// chatroomSuffixRegex 判断括号内容是否是 "xxx@chatroom" 形式。
var chatroomSuffixRegex = regexp.MustCompile(`@chatroom$`)

// NewBackupIndex 创建索引但不立即扫盘。调用方需随后调用 Scan() 一次。
// root 为空时, Scan/Resolve 都会返回安全默认值, 等同于 "backup 未配置"。
func NewBackupIndex(root string, folderMap map[string]string) *BackupIndex {
	return &BackupIndex{
		backupRoot:  root,
		folderMap:   normalizeFolderMap(folderMap),
		talkerToDir: make(map[string]string),
		hexToDir:    make(map[string]string),
	}
}

// Scan 遍历 backup root 的一级子目录, 按命名格式分别放入 talkerToDir / hexToDir。
// 对每个子目录做 symlink 逃逸检查, 逃逸的目录计入 unknown 且不加索引。
// Scan 可重复调用, 每次都会清空并重建索引。
func (b *BackupIndex) Scan() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.talkerToDir = make(map[string]string)
	b.hexToDir = make(map[string]string)
	b.chatroomCount, b.hexCount, b.unknownCount = 0, 0, 0

	if b.backupRoot == "" {
		return nil
	}

	entries, err := os.ReadDir(b.backupRoot)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warn().Str("path", b.backupRoot).Msg("backup: root path does not exist, index stays empty")
			return nil
		}
		log.Warn().Err(err).Str("path", b.backupRoot).Msg("backup: cannot read root, index stays empty")
		return nil
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		absPath := filepath.Join(b.backupRoot, name)

		if err := assertWithinBackup(b.backupRoot, absPath); err != nil {
			if errors.Is(err, ErrBackupSymlinkEscape) {
				log.Warn().Str("dir", name).Msg("backup: symlink escapes backup_root, skipping")
			} else {
				log.Debug().Err(err).Str("dir", name).Msg("backup: cannot resolve path, skipping")
			}
			b.unknownCount++
			continue
		}

		content := extractFolderTag(name)
		switch {
		case content == "":
			b.unknownCount++
		case hexFolderIDRegex.MatchString(content):
			b.hexToDir[strings.ToUpper(content)] = absPath
			b.hexCount++
		case isTalkerLike(content):
			b.talkerToDir[content] = absPath
			b.chatroomCount++
		default:
			b.unknownCount++
		}
	}

	log.Info().
		Str("root", b.backupRoot).
		Int("chatroom_mode", b.chatroomCount).
		Int("hex_mode", b.hexCount).
		Int("unknown", b.unknownCount).
		Int("folder_map_entries", len(b.folderMap)).
		Msg("backup: index built")

	return nil
}

// Resolve 给定 talker (如 "27580424670@chatroom" 或 "wxid_xxx"), 返回对应的 backup
// 子目录绝对路径。via 取值:
//
//   - "chatroom": talker 本身作为目录尾部标识命中 (自动识别)
//   - "map":      talker 通过用户配置的 folderMap 转 hex 后命中
//   - "":         未命中 (ok == false)
//
// 两表同时命中时 chatroom 优先, 因为自动识别比配置映射更可靠。
func (b *BackupIndex) Resolve(talker string) (dir string, via string, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if dir, hit := b.talkerToDir[talker]; hit {
		return dir, "chatroom", true
	}
	if hex, hasMap := b.folderMap[talker]; hasMap {
		if dir, hit := b.hexToDir[hex]; hit {
			return dir, "map", true
		}
	}
	return "", "", false
}

// Stats 返回索引构建时各类目录的数量, 供 /api/v1/backup/stats 和 TUI 展示使用。
func (b *BackupIndex) Stats() (chatroomMode, hexMode, unknown int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.chatroomCount, b.hexCount, b.unknownCount
}

// UpdateFolderMap 热更替换 folderMap, 不触发重扫。Scan 期间的 talkerToDir/hexToDir
// 保持不变, 只影响后续 Resolve 的 map→hex 这一分支。
func (b *BackupIndex) UpdateFolderMap(m map[string]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.folderMap = normalizeFolderMap(m)
}

// extractFolderTag 从目录名末尾的 "(xxx)" 中抽出 xxx; 无括号或括号在中间返回空串。
func extractFolderTag(name string) string {
	m := folderParenRegex.FindStringSubmatch(name)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// isTalkerLike 判断字符串是否像 talker: 要么以 @chatroom 结尾, 要么以 wxid_ 开头。
func isTalkerLike(s string) bool {
	if chatroomSuffixRegex.MatchString(s) {
		return true
	}
	return strings.HasPrefix(s, "wxid_")
}

// normalizeFolderMap 把 folderMap 的 value 统一成大写 hex。key (talker) 保持原样。
func normalizeFolderMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = strings.ToUpper(v)
	}
	return out
}
