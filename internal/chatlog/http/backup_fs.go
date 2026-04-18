package http

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// supportedImageExts 定义 backup 目录下被视作图片的扩展名白名单。
var supportedImageExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".gif":  true,
	".bmp":  true,
	".webp": true,
}

// hexFolderIDRegex 校验 folder_id 必须为 8 位十六进制字符。
var hexFolderIDRegex = regexp.MustCompile(`^[0-9A-Fa-f]{8}$`)

// ErrBackupSymlinkEscape 表示某个 backup 子目录通过 symlink 指向了 backup_path 之外,
// 拒绝加入索引以免被作为沙盒逃逸利用。
var ErrBackupSymlinkEscape = errors.New("backup: symlink escapes backup_path")

// FindBackupSubFolder 在 backupPath 下扫描一级子目录, 查找名称以 "(folderID)" 结尾
// 的目录 (如 "拼车群(C606ACFA)"), 返回该目录的绝对路径。
//
// folderID 对比大小写不敏感。对找到的目录做 symlink 逃逸检查 (EvalSymlinks 后
// 必须仍在 backupPath 子树内), 防御恶意配置或被篡改的 backup 目录。
func FindBackupSubFolder(backupPath, folderID string) (string, error) {
	suffix := fmt.Sprintf("(%s)", strings.ToUpper(folderID))

	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return "", fmt.Errorf("read backup dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToUpper(entry.Name()), suffix) {
			continue
		}
		resolved := filepath.Join(backupPath, entry.Name())
		if err := assertWithinBackup(backupPath, resolved); err != nil {
			return "", err
		}
		return resolved, nil
	}

	return "", fmt.Errorf("no folder matching *(%s)", folderID)
}

// FindImagesByPrefix 在 monthDir 下查找文件名以 timePrefix 开头、且扩展名属于
// supportedImageExts 白名单的全部文件, 返回绝对路径列表。
//
// 返回切片可能为 0-N 项; 调用方需自行处理 "无匹配" (空切片) 和 "多匹配" (>1 项,
// 通常由同秒多图导致, 调用方应记录告警并选择其中之一)。
func FindImagesByPrefix(monthDir, timePrefix string) ([]string, error) {
	entries, err := os.ReadDir(monthDir)
	if err != nil {
		return nil, fmt.Errorf("read month dir: %w", err)
	}

	var matches []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, timePrefix) {
			continue
		}
		if !supportedImageExts[strings.ToLower(filepath.Ext(name))] {
			continue
		}
		matches = append(matches, filepath.Join(monthDir, name))
	}

	return matches, nil
}

// assertWithinBackup 解析 path 的真实位置 (穿透 symlink), 确保它仍然在 backupRoot
// 子树内。返回 ErrBackupSymlinkEscape 若 path 指向 backup 之外的任何位置。
func assertWithinBackup(backupRoot, path string) error {
	realRoot, err := filepath.EvalSymlinks(backupRoot)
	if err != nil {
		return fmt.Errorf("eval backup root: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("eval path: %w", err)
	}
	// 规范化后判断 realPath 是否以 realRoot + Separator 开头 (或等于 realRoot)
	realRoot = filepath.Clean(realRoot)
	realPath = filepath.Clean(realPath)
	if realPath == realRoot {
		return nil
	}
	prefix := realRoot + string(filepath.Separator)
	if !strings.HasPrefix(realPath, prefix) {
		return ErrBackupSymlinkEscape
	}
	return nil
}
