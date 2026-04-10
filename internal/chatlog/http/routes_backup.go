package http

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// supportedImageExts 定义支持的图片扩展名集合。
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

// initBackupRouter 注册 backup 相关的路由。
func (s *Service) initBackupRouter() {
	s.router.GET("/api/v1/backup/image", s.handleBackupImage)
}

// handleBackupImage 处理备份图片请求。
// 根据 folder_id、date、time 参数在 backup_path 下查找对应图片并返回。
func (s *Service) handleBackupImage(c *gin.Context) {
	// 1. 检查 backup_path 是否已配置
	backupPath := s.conf.GetBackupPath()
	if backupPath == "" {
		log.Debug().Msg("backup: backup_path not configured")
		c.JSON(http.StatusNotFound, gin.H{"error": "backup not configured"})
		return
	}

	// 2. 读取并校验查询参数
	folderID := c.Query("folder_id")
	date := c.Query("date")
	timePrefix := c.Query("time")

	if folderID == "" || date == "" || timePrefix == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing required params: folder_id, date, time"})
		return
	}

	if !hexFolderIDRegex.MatchString(folderID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "folder_id must be 8 hex characters"})
		return
	}

	// 3. 在 backup_path 下查找匹配 *(FOLDER_ID) 的子目录
	subFolder, err := findBackupSubFolder(backupPath, folderID)
	if err != nil {
		log.Debug().Str("folder_id", folderID).Err(err).Msg("backup: folder not found")
		c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
		return
	}

	// 4. 在 {子目录}/{date}/ 下查找 {timePrefix}*.{ext}
	monthDir := filepath.Join(subFolder, date)
	imgPath, err := findImageByPrefix(monthDir, timePrefix)
	if err != nil {
		log.Debug().Str("month_dir", monthDir).Str("time", timePrefix).Err(err).Msg("backup: image not found")
		c.JSON(http.StatusNotFound, gin.H{"error": "image not found"})
		return
	}

	log.Debug().Str("path", imgPath).Msg("backup: serving image")
	c.File(imgPath)
}

// findBackupSubFolder 在 backupPath 下扫描子目录，查找名称以 (folderID) 结尾的目录。
// 例如 folderID 为 "AABBCCDD"，则匹配 "测试群(AABBCCDD)"。
func findBackupSubFolder(backupPath, folderID string) (string, error) {
	suffix := fmt.Sprintf("(%s)", strings.ToUpper(folderID))

	entries, err := os.ReadDir(backupPath)
	if err != nil {
		return "", fmt.Errorf("read backup dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasSuffix(strings.ToUpper(entry.Name()), suffix) {
			return filepath.Join(backupPath, entry.Name()), nil
		}
	}

	return "", fmt.Errorf("no folder matching *(%s)", folderID)
}

// findImageByPrefix 在 monthDir 下查找文件名以 timePrefix 开头且扩展名为支持图片格式的文件。
func findImageByPrefix(monthDir, timePrefix string) (string, error) {
	entries, err := os.ReadDir(monthDir)
	if err != nil {
		return "", fmt.Errorf("read month dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if !supportedImageExts[ext] {
			continue
		}
		if strings.HasPrefix(name, timePrefix) {
			return filepath.Join(monthDir, name), nil
		}
	}

	return "", fmt.Errorf("no image with prefix %s", timePrefix)
}
