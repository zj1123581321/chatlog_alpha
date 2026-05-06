package wechat

// migration.go：Step 5g 启动迁移检测（architecture-rework-2026-05-06.md §9.2）。
//
// 启动时 watcher 调一次 DetectAndMigrate：
//
//	if exists(work_dir/generations/) → 已迁移，无操作
//	else if exists(work_dir/db_storage/) → 旧 in-place 模式
//	  - mkdir generations/{id}/
//	  - mv work_dir/db_storage → generations/{id}/db_storage
//	  - schema check（pass → swap current；fail → mv 到 corrupt/）
//	else → 全新部署，无操作（caller 走 firstFullDecrypt 路径）
//
// 设计取舍：迁移用 os.Rename 而非 copy + delete —— 同卷 NTFS rename 是原子的，
// 进程在迁移中段被 kill 至多留下一个 generation 目录里没 status 切换，下次启动
// 重新走 schema check 即可，不会数据损坏。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MigrationOutcome 描述 DetectAndMigrate 的结果分类。
type MigrationOutcome string

const (
	// MigrationAlreadyMigrated: workdir 已存在 generations/ 目录，本次不动。
	MigrationAlreadyMigrated MigrationOutcome = "already-migrated"
	// MigrationFresh: 既无 generations/ 也无 db_storage/，全新部署，无迁移可做。
	MigrationFresh MigrationOutcome = "fresh-install"
	// MigrationSwapped: 迁移完成 + schema 通过 + current_generation 已切。
	MigrationSwapped MigrationOutcome = "swapped"
	// MigrationCorrupt: 迁移完成但 schema 失败，generation 已 mv 到 corrupt/，
	// current_generation 未切。caller 应当走 firstFullDecrypt 重新追赶。
	MigrationCorrupt MigrationOutcome = "corrupt"
)

// MigrationOpts 是 DetectAndMigrate 的输入。
type MigrationOpts struct {
	WorkDir    string
	DBs        []DBJob          // 用于迁移后的 schema check；空切片跳过 schema 检查
	SchemaMode SchemaCheckMode  // 默认 quick
	WatcherPID int

	Now func() time.Time
}

// MigrationResult 是 DetectAndMigrate 的输出。
type MigrationResult struct {
	Outcome      MigrationOutcome
	GenerationID string // Swapped/Corrupt 时填，其余空
	Reason       string // Corrupt 时附错误细节
}

// DetectAndMigrate 执行启动期一次性迁移逻辑。
//
// 幂等性：第二次跑时（generations/ 已存在）走 AlreadyMigrated 分支，无副作用。
func DetectAndMigrate(opts MigrationOpts) (MigrationResult, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.SchemaMode == "" {
		opts.SchemaMode = DefaultSchemaCheckMode
	}

	genRoot := filepath.Join(opts.WorkDir, "generations")
	if info, err := os.Stat(genRoot); err == nil && info.IsDir() {
		return MigrationResult{Outcome: MigrationAlreadyMigrated}, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return MigrationResult{}, fmt.Errorf("migrate: stat generations: %w", err)
	}

	legacyDB := filepath.Join(opts.WorkDir, "db_storage")
	legacyInfo, err := os.Stat(legacyDB)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return MigrationResult{Outcome: MigrationFresh}, nil
		}
		return MigrationResult{}, fmt.Errorf("migrate: stat db_storage: %w", err)
	}
	if !legacyInfo.IsDir() {
		// 不寻常但非致命：当作 fresh，让 caller 决定。
		return MigrationResult{Outcome: MigrationFresh}, nil
	}

	id := NewGenerationID(opts.Now())
	genDir := ResolveGenerationDir(opts.WorkDir, id)

	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return MigrationResult{}, fmt.Errorf("migrate: mkdir gen: %w", err)
	}

	// 把 legacy db_storage 整体 mv 到 generations/{id}/db_storage
	target := filepath.Join(genDir, "db_storage")
	if err := os.Rename(legacyDB, target); err != nil {
		// 清理空 gen 目录避免下次 AlreadyMigrated 误判
		_ = os.Remove(genDir)
		_ = os.Remove(genRoot)
		return MigrationResult{}, fmt.Errorf("migrate: rename db_storage: %w", err)
	}

	// schema check（如有 DBs）
	if len(opts.DBs) > 0 {
		for _, job := range opts.DBs {
			path := filepath.Join(target, job.RelPath)
			if err := CheckSchema(path, job.Schema, opts.SchemaMode); err != nil {
				return moveMigrationToCorrupt(opts, genDir, id, err), nil
			}
		}
	}

	// swap status.json，把 current_generation 指向迁移后的 id
	now := opts.Now()
	s := Status{
		Version:             StatusSchemaVersion,
		LastDecryptTS:       now,
		GenerationID:        id,
		CurrentGeneration:   id,
		WatcherPID:          opts.WatcherPID,
		WatcherHeartbeatTS:  now,
		Healthy:             true,
		SuccessfulCycles24h: 1,
		WorkDir:             opts.WorkDir,
	}
	if err := WriteStatusAtomic(opts.WorkDir, s); err != nil {
		// status 写不进，但物理迁移已完成。下次启动会看到 generations/ 存在
		// → AlreadyMigrated 分支 → caller 应当用普通 polling 路径感知缺失的 status.json。
		return MigrationResult{Outcome: MigrationSwapped, GenerationID: id,
			Reason: fmt.Sprintf("status write failed (will recover next start): %v", err)}, nil
	}
	return MigrationResult{Outcome: MigrationSwapped, GenerationID: id}, nil
}

func moveMigrationToCorrupt(opts MigrationOpts, genDir, id string, cause error) MigrationResult {
	corruptRoot := filepath.Join(opts.WorkDir, "corrupt")
	if err := os.MkdirAll(corruptRoot, 0o755); err == nil {
		target := filepath.Join(corruptRoot, fmt.Sprintf("%s-migration-schema", id))
		_ = os.Rename(genDir, target)
	}
	return MigrationResult{
		Outcome:      MigrationCorrupt,
		GenerationID: id,
		Reason:       fmt.Sprintf("migration schema check failed: %v", cause),
	}
}
