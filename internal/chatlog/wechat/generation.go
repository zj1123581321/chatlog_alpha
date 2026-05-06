package wechat

// generation.go：Step 5a 的纯结构层，不接 decrypt 路径。
//
// 职责（来自 architecture-rework-2026-05-06.md §8.2 + Eng Review Lock A1）：
//   1. 定义 status.json 的 schema（Status struct，version=1）。
//   2. 提供 WriteStatusAtomic / ReadStatus —— A1 要求 NTFS 同卷 os.Rename
//      原子覆盖，替代 spec 早期讨论的 NTFS junction 方案。
//   3. 生成单调唯一的 GenerationID —— 同秒多次调用必须可区分，否则两次连续
//      polling cycle 都成功时第二个 generation 会覆盖第一个的目录。
//   4. ResolveGenerationDir —— A1：server 读 current_generation 解析物理路径
//      (workDir/generations/{id}/) 的唯一约定点。
//
// **明确不在本文件内**：decrypt 调用、schema gate、watcher 主循环、HTTP /healthz。
// 这些由 Step 5b/5c/5d/5e 在后续 PR 中接入。本文件保持纯函数 + 零外部依赖，
// 让 round-trip 测试可以在不启动任何长进程的情况下完整覆盖序列化语义。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatusSchemaVersion 是当前 status.json 的 schema 版本。
//
// 版本规则：
//   - 0：legacy（缺省字段，向后兼容尚未升级到本格式的部署）。ReadStatus 接受 0 视作 1。
//   - 1：当前版本，§8.2 + A1 字段集。
//   - >1：将来扩展时 bump，旧 chatlog 读到会显式拒绝（ErrUnsupportedStatusVersion），
//     避免静默误读出现"伪健康"。
const StatusSchemaVersion = 1

// StatusFileName 是 work_dir 下 status.json 的固定文件名。
const StatusFileName = "status.json"

// statusTempName 是 WriteStatusAtomic 期间使用的临时文件名。
// 使用确定性后缀（非 PID-suffix）：本系统约定单实例 watcher，并发写不该发生；
// 若真发生，rename 之间会被覆盖，但最终态仍是某一次 write 的完整内容（不会半文件）。
const statusTempName = StatusFileName + ".tmp"

// ErrUnsupportedStatusVersion：读到的 status.json version 高于当前支持的 SchemaVersion。
var ErrUnsupportedStatusVersion = errors.New("status.json: unsupported schema version")

// Status 是 work_dir/status.json 的 Go 表示。字段对齐 §8.2 + Eng Review Lock A1。
//
// 设计要点：
//   - GenerationID（最近一次 watcher 创建好、通过 schema gate 的 generation）和
//     CurrentGeneration（当前已 atomic-swap 进来给 server 读的）分开存：
//     swap 失败时旧 CurrentGeneration 不动，server 仍能正确 serve 旧数据。
//   - WatcherHeartbeatTS 是 §8.3 /healthz 检查的 staleness 标志位。
//   - Counters（CorruptCount24h 等）是 §8.2 已定义的运维指标，
//     watcher 主循环负责更新。Step 5a 不写这些字段，只保证 schema 能 round-trip。
type Status struct {
	// Version 是 schema 版本号。新建 Status 时由 WriteStatusAtomic 自动填 StatusSchemaVersion。
	Version int `json:"version"`

	// LastDecryptTS 是 watcher 上次成功完成一轮 decrypt + schema gate + swap 的时间。
	LastDecryptTS time.Time `json:"last_decrypt_ts,omitempty"`

	// LastDecryptDurationMs 是上次 decrypt cycle 的耗时（含 copy + decrypt + schema check + swap）。
	LastDecryptDurationMs int64 `json:"last_decrypt_duration_ms,omitempty"`

	// GenerationID 是上次 watcher 创建的 generation 标识（即使 swap 失败也会更新）。
	GenerationID string `json:"generation_id,omitempty"`

	// CurrentGeneration 是当前 server 应该读的 generation 标识（A1：替代 NTFS junction
	// 的 swap pointer）。Server 进程每 30s poll 这个字段，变化时 invalidateAll 连接池。
	CurrentGeneration string `json:"current_generation,omitempty"`

	// WatcherPID 是当前持有 work_dir 锁的 watcher 进程 PID（§3.5 单实例校验）。
	WatcherPID int `json:"watcher_pid,omitempty"`

	// WatcherHeartbeatTS 是 watcher 主循环每次 tick 更新的时间戳。
	// /healthz 用 (now - WatcherHeartbeatTS > 10min) 判定 watcher hang/crash。
	WatcherHeartbeatTS time.Time `json:"watcher_heartbeat_ts,omitempty"`

	// Healthy 是 watcher 自评健康标志。false 表示 watcher 检测到自己有问题
	// （磁盘满、连续 corrupt、检测到微信 schema 大版本变化等）。
	Healthy bool `json:"healthy"`

	// CorruptCount24h: 过去 24h 内进 corrupt/ 目录的 generation 数。
	CorruptCount24h int `json:"corrupt_count_24h"`

	// SuccessfulCycles24h: 过去 24h 内成功完成 swap 的 cycle 数。
	SuccessfulCycles24h int `json:"successful_cycles_24h"`

	// SkippedCycles24h: 过去 24h 内 polling 后判定无变化跳过的 cycle 数。
	SkippedCycles24h int `json:"skipped_cycles_24h"`

	// WeixinYieldCount24h: §A5 微信高 IO 让位次数。indirect 健康指标，回归探针。
	WeixinYieldCount24h int `json:"weixin_yield_count_24h"`

	// DataDir / WorkDir：冗余写入便于 status.json 单文件诊断（无需另外查 chatlog.json）。
	DataDir string `json:"data_dir,omitempty"`
	WorkDir string `json:"work_dir,omitempty"`
}

// ReadStatus 从 work_dir/status.json 读 Status。
//
// 错误语义：
//   - 文件不存在 → 包装 os.ErrNotExist，调用方用 errors.Is 区分"还没初始化过"。
//   - JSON 解析失败 → 直接返回 (上游不应吞)。
//   - version > StatusSchemaVersion → 返回 ErrUnsupportedStatusVersion。
//   - version == 0 → 视作 legacy v1 兼容（避免一次 schema 升级把所有现有部署拒之门外）。
func ReadStatus(workDir string) (Status, error) {
	path := filepath.Join(workDir, StatusFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return Status{}, err // os.ErrNotExist 包装由 os.ReadFile 提供
	}

	var s Status
	if err := json.Unmarshal(raw, &s); err != nil {
		return Status{}, fmt.Errorf("status.json: parse: %w", err)
	}

	if s.Version > StatusSchemaVersion {
		return Status{}, fmt.Errorf("%w: got %d, supported up to %d",
			ErrUnsupportedStatusVersion, s.Version, StatusSchemaVersion)
	}

	return s, nil
}

// WriteStatusAtomic 把 Status 写到 work_dir/status.json。
//
// 实现：write tmp → fsync → os.Rename（A1：NTFS 同卷文件级 atomic）。
// 中途崩溃只会留下 status.json.tmp（下次启动可清理），永远不会留下半写状态的 status.json。
//
// 自动行为：
//   - 调用方留空 Version，WriteStatusAtomic 会填当前 StatusSchemaVersion；
//     若调用方明确写了非零 Version，按调用方意图保留（便于测试或工具迁移）。
func WriteStatusAtomic(workDir string, s Status) error {
	if s.Version == 0 {
		s.Version = StatusSchemaVersion
	}

	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("status.json: mkdir work_dir: %w", err)
	}

	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("status.json: marshal: %w", err)
	}
	body = append(body, '\n')

	tmpPath := filepath.Join(workDir, statusTempName)
	finalPath := filepath.Join(workDir, StatusFileName)

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("status.json: open tmp: %w", err)
	}
	if _, werr := f.Write(body); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("status.json: write tmp: %w", werr)
	}
	if serr := f.Sync(); serr != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("status.json: fsync tmp: %w", serr)
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("status.json: close tmp: %w", cerr)
	}

	// os.Rename 在 Windows 上对应 MoveFileEx + MOVEFILE_REPLACE_EXISTING，NTFS 同卷文件级原子。
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("status.json: rename: %w", err)
	}
	return nil
}

// ResolveGenerationDir 把 generation id 解析成 work_dir 下的物理目录。
//
// 这是 A1 约定的"server 读 current_generation 解析物理路径"的唯一入口：
// 任何想从 generation id 拿到 db_storage 路径的代码都该走这里，避免拼接漂移。
//
// id == "" 显式 panic：CurrentGeneration 为空意味着 watcher 还没 swap 过，
// server 不应该 query 这种半初始化状态。silently fallback 到 workDir 会把
// "未初始化"和"初始化完成"两种语义混淆，更危险。
func ResolveGenerationDir(workDir, id string) string {
	if id == "" {
		panic("generation: ResolveGenerationDir called with empty id")
	}
	return filepath.Join(workDir, "generations", id)
}

// genIDState 是 NewGenerationID 的进程级单调状态。
// 同秒多次调用通过 lastSec + counter 组合保证 ID 唯一。
var genIDState struct {
	mu      sync.Mutex
	lastSec time.Time
	counter int
}

// NewGenerationID 返回 generation id，前提 caller 用单调递增的 time.Time（time.Now() 总是满足）。
//
// 格式：
//   - 同秒首次：YYYYMMDD-HHMMSS              （e.g. "20260506-143000"）
//   - 同秒后续：YYYYMMDD-HHMMSS-NNN          （NNN 从 001 起，0-padded 3 位）
//
// 同秒处理：counter 累加；下一秒到达 → counter 归零。string 比较结果与时间序一致
// （因为时间字段固定宽度 + 后缀只增），server 端 polling 用 != 检查变化即可。
//
// 并发安全：sync.Mutex 保护（counter 与 lastSec 是双值耦合，CAS 不够）。
func NewGenerationID(t time.Time) string {
	genIDState.mu.Lock()
	defer genIDState.mu.Unlock()

	sec := t.Truncate(time.Second)
	if sec.Equal(genIDState.lastSec) {
		genIDState.counter++
	} else {
		genIDState.lastSec = sec
		genIDState.counter = 0
	}

	// 本地时间格式化：与 §8.2 的 generation_id 例子对齐（"20260506-143000" 跟
	// last_decrypt_ts 的 +08:00 小时数对齐）。chatlog 是单机 Windows 工具，
	// 时区固定，避免 UTC 和挂钟差 8 小时的诊断混乱。
	base := sec.Format("20060102-150405")
	if genIDState.counter == 0 {
		return base
	}
	return fmt.Sprintf("%s-%03d", base, genIDState.counter)
}
