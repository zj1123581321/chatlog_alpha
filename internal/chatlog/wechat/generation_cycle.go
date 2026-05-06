package wechat

// generation_cycle.go：Step 5d，把 5a/5b/5c 串成一次完整的 decrypt cycle
// （architecture-rework-2026-05-06.md §4.1 主数据流）。
//
// 流程：
//
//	mkdir gen/{id}/raw  +  gen/{id}/db_storage
//	  → for each db: WAL-aware copy + coherency check
//	  → DecryptFunc(raw, db_storage)
//	  → for each db: schema gate
//	  → write manifest.json
//	  → atomic swap：WriteStatusAtomic 把 current_generation 切到新 id（A1）
//
// 任意一步失败 → moveToCorrupt：mv gen 到 corrupt/{id}-{reason}/，
// 旧 status.current_generation 不动（A1 + A3 的安全语义）。
//
// 不在本文件内：
//   - 真实的 decrypt 调用（caller 注入 DecryptFunc，沿用现有 Manager.DecryptDBFiles）
//   - polling tick / smart timing / IO yield —— 由后续 PR 在主循环里包外层
//   - prune —— 5f 的 PruneGenerations 由 caller 在 cycle 后调用，避免单次失败的
//     cycle 触发 prune 的副作用

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CycleOutcome 描述一次 RunGenerationCycle 的结果分类。
type CycleOutcome string

const (
	// OutcomeSwapped: 完整 cycle 成功，current_generation 已切到新 id。
	OutcomeSwapped CycleOutcome = "swapped"
	// OutcomeSkipped: 没有工作要做（DBs 为空），不动 status.json。
	OutcomeSkipped CycleOutcome = "skipped"
	// OutcomeCorrupt: 某一步失败，gen 已搬到 corrupt/，current 保持上一次。
	OutcomeCorrupt CycleOutcome = "corrupt"
)

// DBJob 是单个 db 的处理任务。
type DBJob struct {
	// RelPath：相对 data_dir/db_storage 的路径，e.g. "message/multi/message_0.db"。
	RelPath string
	// Schema：该 db 的业务级 schema 期望，传给 CheckSchema。
	Schema SchemaSpec
}

// CycleInput 是 RunGenerationCycle 的输入。
type CycleInput struct {
	WorkDir     string
	DataDir     string
	DBs         []DBJob
	DecryptFunc func(rawDir, dstDir string) error
	SchemaMode  SchemaCheckMode
	WatcherPID  int

	// Now 注入测试时钟。零值 → time.Now。
	Now func() time.Time
	// MtimeWindow：WAL coherency 容忍偏差。零值 → DefaultWALMtimeWindow (2s)。
	MtimeWindow time.Duration
}

// GenerationCycleResult 是 RunGenerationCycle 的输出。
type GenerationCycleResult struct {
	GenerationID string
	Outcome      CycleOutcome
	Reason       string // 人类可读的简短说明（成功时为空）
}

// manifest 写到 generations/{id}/manifest.json，记录该 generation 的元数据。
type manifest struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	SchemasOK  bool      `json:"schemas_ok"`
	DBs        []string  `json:"dbs"`
	SchemaMode string    `json:"schema_mode"`
}

// RunGenerationCycle 执行一次完整 decrypt cycle。
// 调用方应在外层管理 polling tick + IO yield，并在 cycle 结束后视情况调用 PruneGenerations。
func RunGenerationCycle(in CycleInput) (GenerationCycleResult, error) {
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.MtimeWindow == 0 {
		in.MtimeWindow = DefaultWALMtimeWindow
	}
	if in.SchemaMode == "" {
		in.SchemaMode = DefaultSchemaCheckMode
	}

	if len(in.DBs) == 0 {
		return GenerationCycleResult{Outcome: OutcomeSkipped, Reason: "no dbs in cycle input"}, nil
	}

	id := NewGenerationID(in.Now())
	genDir := ResolveGenerationDir(in.WorkDir, id)
	rawRoot := filepath.Join(genDir, "raw", "db_storage")
	decRoot := filepath.Join(genDir, "db_storage")

	if err := os.MkdirAll(rawRoot, 0o755); err != nil {
		return failWithoutDir(in, id, "mkdir-raw", err), nil
	}
	if err := os.MkdirAll(decRoot, 0o755); err != nil {
		return failAndCorrupt(in, genDir, id, "mkdir-decrypted", err), nil
	}

	// Step 1: WAL-aware copy + coherency
	for _, job := range in.DBs {
		if err := copyAndVerify(in, rawRoot, job); err != nil {
			return failAndCorrupt(in, genDir, id, classifyCopyErr(err), err), nil
		}
	}

	// Step 2: decrypt
	if err := in.DecryptFunc(filepath.Join(genDir, "raw"), genDir); err != nil {
		return failAndCorrupt(in, genDir, id, "decrypt", err), nil
	}

	// Step 3: schema gate per db
	for _, job := range in.DBs {
		dec := filepath.Join(decRoot, job.RelPath)
		if err := CheckSchema(dec, job.Schema, in.SchemaMode); err != nil {
			return failAndCorrupt(in, genDir, id, "schema", err), nil
		}
	}

	// Step 4: manifest（先写 manifest 再 swap status：manifest 是 generation 自描述，
	// status 是全局指针；顺序保证如果 swap 成功，目标 generation 一定已自描述完整）。
	if err := writeManifest(genDir, id, in); err != nil {
		return failAndCorrupt(in, genDir, id, "manifest", err), nil
	}

	// Step 5: atomic swap via status.json (Eng A1)
	if err := swapCurrent(in, id); err != nil {
		return failAndCorrupt(in, genDir, id, "status-write", err), nil
	}

	return GenerationCycleResult{GenerationID: id, Outcome: OutcomeSwapped}, nil
}

// copyAndVerify 复制 + 校验单个 DBJob。
func copyAndVerify(in CycleInput, rawRoot string, job DBJob) error {
	src := filepath.Join(in.DataDir, "db_storage", job.RelPath)
	dst := filepath.Join(rawRoot, job.RelPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", job.RelPath, err)
	}
	walCopied, err := CopyDBPair(src, dst)
	if err != nil {
		return err
	}
	walPath := ""
	if walCopied {
		walPath = dst + "-wal"
	}
	if err := CheckWALCoherency(dst, walPath, in.MtimeWindow); err != nil {
		return err
	}
	return nil
}

// classifyCopyErr 把复制阶段错误归类成短 reason 字符串（用于 corrupt 目录命名）。
func classifyCopyErr(err error) string {
	if errors.Is(err, ErrWALIncoherent) {
		return "wal-incoherent"
	}
	return "copy"
}

// writeManifest 把 generation 自描述写入 generations/{id}/manifest.json。
func writeManifest(genDir, id string, in CycleInput) error {
	m := manifest{
		ID:         id,
		CreatedAt:  in.Now(),
		SchemasOK:  true,
		SchemaMode: string(in.SchemaMode),
	}
	for _, j := range in.DBs {
		m.DBs = append(m.DBs, j.RelPath)
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(genDir, "manifest.json"), append(body, '\n'), 0o600)
}

// swapCurrent 写新 status.json：current_generation = id，counters 累加。
// 旧 status 读不到也不致命（首次 swap）；其它字段从 prev 继承让 watcher 进程
// 重启后状态能延续。
func swapCurrent(in CycleInput, id string) error {
	prev, _ := ReadStatus(in.WorkDir)
	now := in.Now()
	s := Status{
		Version:             StatusSchemaVersion,
		LastDecryptTS:       now,
		GenerationID:        id,
		CurrentGeneration:   id,
		WatcherPID:          in.WatcherPID,
		WatcherHeartbeatTS:  now,
		Healthy:             true,
		CorruptCount24h:     prev.CorruptCount24h,
		SuccessfulCycles24h: prev.SuccessfulCycles24h + 1,
		SkippedCycles24h:    prev.SkippedCycles24h,
		WeixinYieldCount24h: prev.WeixinYieldCount24h,
		DataDir:             in.DataDir,
		WorkDir:             in.WorkDir,
	}
	return WriteStatusAtomic(in.WorkDir, s)
}

// failAndCorrupt：把 genDir 搬到 corrupt/{id}-{reason}/ + 写失败状态。
// 返回包含原因的 GenerationCycleResult。永不返回 error（失败已处理）。
func failAndCorrupt(in CycleInput, genDir, id, reason string, cause error) GenerationCycleResult {
	moveToCorrupt(in.WorkDir, genDir, id, reason)
	writeFailStatus(in, id)
	return GenerationCycleResult{
		GenerationID: id,
		Outcome:      OutcomeCorrupt,
		Reason:       fmt.Sprintf("%s: %v", reason, cause),
	}
}

// failWithoutDir：在 mkdir gen/{id}/raw 都失败的情况下走这里 —— genDir 可能不完整，
// 不尝试 mv 到 corrupt（避免 mv 失败掩盖原因）。仅写状态。
func failWithoutDir(in CycleInput, id, reason string, cause error) GenerationCycleResult {
	writeFailStatus(in, id)
	return GenerationCycleResult{
		GenerationID: id,
		Outcome:      OutcomeCorrupt,
		Reason:       fmt.Sprintf("%s: %v", reason, cause),
	}
}

func moveToCorrupt(workDir, genDir, id, reason string) {
	corruptRoot := filepath.Join(workDir, "corrupt")
	if err := os.MkdirAll(corruptRoot, 0o755); err != nil {
		return
	}
	target := filepath.Join(corruptRoot, fmt.Sprintf("%s-%s", id, reason))
	_ = os.Rename(genDir, target)
}

// writeFailStatus 在 cycle 失败时更新 status.json：保留 current_generation，
// 但更新 GenerationID（最近一次尝试）+ corrupt counter +1 + watcher heartbeat。
// 写盘失败本身被吞掉 —— 状态写不了不该让 cycle 失败语义升级为 panic。
func writeFailStatus(in CycleInput, attemptedID string) {
	prev, _ := ReadStatus(in.WorkDir)
	now := in.Now()
	s := Status{
		Version:             StatusSchemaVersion,
		LastDecryptTS:       prev.LastDecryptTS, // 保留，失败 cycle 不算"上次解密成功"
		GenerationID:        attemptedID,        // 即便失败也更新（spec §8.2 注释）
		CurrentGeneration:   prev.CurrentGeneration,
		WatcherPID:          in.WatcherPID,
		WatcherHeartbeatTS:  now,
		Healthy:             prev.Healthy, // 单次失败不直接翻 false，由外层熔断决定
		CorruptCount24h:     prev.CorruptCount24h + 1,
		SuccessfulCycles24h: prev.SuccessfulCycles24h,
		SkippedCycles24h:    prev.SkippedCycles24h,
		WeixinYieldCount24h: prev.WeixinYieldCount24h,
		DataDir:             in.DataDir,
		WorkDir:             in.WorkDir,
	}
	_ = WriteStatusAtomic(in.WorkDir, s)
}
