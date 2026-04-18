package http

import "sync/atomic"

// backupStats 跟踪 /image/{md5} 请求的来源分布, 用于 /api/v1/backup/stats 返回
// 诊断数据。所有字段走 atomic 操作, 无锁。
//
//   Hardlink   : image_hardlink_info_v4 表命中原图/HD
//   Cache      : md5PathCache 命中 + 磁盘找到原图/HD
//   Recurse    : msg/attach 递归扫描命中原图/HD
//   Backup     : 走 backupIndex + FindImagesByPrefix 命中第三方 hook 保存的原图
//   Thumbnail  : 所有原图都没找到, 只能回退到 _t.dat 缩略图
//   NotFound   : 彻底 404, 连缩略图都没
type backupStats struct {
	Hardlink  atomic.Uint64
	Cache     atomic.Uint64
	Recurse   atomic.Uint64
	Backup    atomic.Uint64
	Thumbnail atomic.Uint64
	NotFound  atomic.Uint64
}

// inc 按来源 tag 原子自增对应计数器。未知 tag 计入 NotFound 以免静默丢失。
func (s *backupStats) inc(source string) {
	switch source {
	case "hardlink":
		s.Hardlink.Add(1)
	case "cache":
		s.Cache.Add(1)
	case "recurse":
		s.Recurse.Add(1)
	case "backup":
		s.Backup.Add(1)
	case "thumbnail":
		s.Thumbnail.Add(1)
	default:
		s.NotFound.Add(1)
	}
}

// snapshot 原子读取所有计数器值, 供 HTTP 响应使用。
func (s *backupStats) snapshot() map[string]uint64 {
	return map[string]uint64{
		"hardlink":  s.Hardlink.Load(),
		"cache":     s.Cache.Load(),
		"recurse":   s.Recurse.Load(),
		"backup":    s.Backup.Load(),
		"thumbnail": s.Thumbnail.Load(),
		"not_found": s.NotFound.Load(),
	}
}

// reset 清零所有计数器, 由 /api/v1/cache/clear 调用。
func (s *backupStats) reset() {
	s.Hardlink.Store(0)
	s.Cache.Store(0)
	s.Recurse.Store(0)
	s.Backup.Store(0)
	s.Thumbnail.Store(0)
	s.NotFound.Store(0)
}
