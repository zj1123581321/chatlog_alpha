package http

import (
	"net/http"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/rs/zerolog/log"
)

// initBackupRouter 注册 backup 相关的路由。
func (s *Service) initBackupRouter() {
	s.router.GET("/api/v1/backup/image", s.handleBackupImage)
	s.router.GET("/api/v1/backup/stats", s.handleBackupStats)
}

// handleBackupStats 返回 /image/{md5} 的来源分布统计 + backup 索引概况,
// 便于用户验证 backup 兜底是否生效、统计近期请求命中率。所有计数器在
// /api/v1/cache/clear 时归零。
func (s *Service) handleBackupStats(c *gin.Context) {
	var chatroom, hex, unknown int
	if s.backupIndex != nil {
		chatroom, hex, unknown = s.backupIndex.Stats()
	}

	resp := gin.H{
		"backup_path":        s.conf.GetBackupPath(),
		"folder_map_entries": len(s.conf.GetBackupFolderMap()),
		"chatroom_mode":      chatroom,
		"hex_mode":           hex,
		"unknown":            unknown,
	}
	for k, v := range s.backupStats.snapshot() {
		resp[k] = v
	}
	c.JSON(http.StatusOK, resp)
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
	subFolder, err := FindBackupSubFolder(backupPath, folderID)
	if err != nil {
		log.Debug().Str("folder_id", folderID).Err(err).Msg("backup: folder not found")
		c.JSON(http.StatusNotFound, gin.H{"error": "folder not found"})
		return
	}

	// 4. 在 {子目录}/{date}/ 下查找 {timePrefix}*
	monthDir := filepath.Join(subFolder, date)
	matches, err := FindImagesByPrefix(monthDir, timePrefix)
	if err != nil {
		log.Debug().Str("month_dir", monthDir).Str("time", timePrefix).Err(err).Msg("backup: month dir not accessible")
		c.JSON(http.StatusNotFound, gin.H{"error": "image not found"})
		return
	}
	if len(matches) == 0 {
		log.Debug().Str("month_dir", monthDir).Str("time", timePrefix).Msg("backup: no image with prefix")
		c.JSON(http.StatusNotFound, gin.H{"error": "image not found"})
		return
	}
	if len(matches) > 1 {
		log.Warn().Strs("matches", matches).Str("time", timePrefix).Msg("backup: multiple images share same time prefix, returning first")
	}

	log.Debug().Str("path", matches[0]).Msg("backup: serving image")
	c.File(matches[0])
}
