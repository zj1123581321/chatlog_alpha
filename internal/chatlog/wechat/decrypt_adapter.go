package wechat

// decrypt_adapter.go：Step 7b，把 Service.DecryptDBFileExplicit 适配成 5d
// RunGenerationCycle / WatcherDaemon 期望的 DecryptFunc 签名。
//
// 输入：rawDir = generations/{id}/raw   dstDir = generations/{id}
// 内部：walk rawDir/db_storage 找所有 *.db（跳 -wal/-shm/fts），对每个调
// Service.DecryptDBFileExplicit(src, dst)。

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// SchemaSpecLookup 映射相对路径 → 业务级 schema 期望。
// caller 在外层根据 chatlog 自己维护的 db 元数据列表填，避免本包硬编码 schema 知识。
type SchemaSpecLookup func(relPath string) SchemaSpec

// NewServiceDecryptFunc 返回一个适配器：调用方把它注入 WatcherDaemonOpts.DecryptFunc，
// 内部走 Service.DecryptDBFileExplicit 真解密，输出到 dstDir/db_storage/<rel>。
//
// 不在 Service 上加方法是因为这是 Step 5/7 边界粘合层 —— 让 Service 的 API 表面
// 保持只暴露"原子单文件解密"，编排逻辑（walk、跳过非 db、并发等）独立成一个文件
// 便于以后改并发模型时只动这里。
func NewServiceDecryptFunc(s *Service) func(rawDir, dstDir string) error {
	return func(rawDir, dstDir string) error {
		rawDBStorage := filepath.Join(rawDir, "db_storage")
		dstDBStorage := filepath.Join(dstDir, "db_storage")

		return filepath.WalkDir(rawDBStorage, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// 跳过 fts 子目录（chatlog 不解密 fts 索引；与 service.go 配置的 blacklist 一致）
				if d.Name() == "fts" && path != rawDBStorage {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			// 只处理 *.db；跳过 -wal、-shm、其他副产物（-journal 等）
			if !strings.HasSuffix(name, ".db") {
				return nil
			}
			rel, relErr := filepath.Rel(rawDBStorage, path)
			if relErr != nil {
				return fmt.Errorf("decrypt adapter: rel for %s: %w", path, relErr)
			}
			dst := filepath.Join(dstDBStorage, rel)
			if err := s.DecryptDBFileExplicit(path, dst); err != nil {
				return fmt.Errorf("decrypt adapter: %s: %w", rel, err)
			}
			return nil
		})
	}
}

// BuildDBJobs 扫 dataDir/db_storage 列出所有 .db 文件，每个套一个调用方传的 schema spec。
// 给 watcher 启动时算 DBs 列表用。
//
// 跳过 fts 子目录、跳过非 .db 文件、按 dbFilePriority 排序（message → session → 其他）
// —— 与现有 Service.DecryptDBFiles 的扫描逻辑保持一致，避免 watcher 漏扫某类 db。
func BuildDBJobs(dataDir string, lookup SchemaSpecLookup) ([]DBJob, error) {
	dbStorage := filepath.Join(dataDir, "db_storage")
	var jobs []DBJob
	err := filepath.WalkDir(dbStorage, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "fts" && path != dbStorage {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".db") {
			return nil
		}
		rel, relErr := filepath.Rel(dbStorage, path)
		if relErr != nil {
			return relErr
		}
		var spec SchemaSpec
		if lookup != nil {
			spec = lookup(rel)
		}
		jobs = append(jobs, DBJob{RelPath: rel, Schema: spec})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}
