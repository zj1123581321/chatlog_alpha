package wechat

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedStatus 构造一个所有字段都填齐的 Status，方便 round-trip / 缺省字段的对比基线。
func fixedStatus() Status {
	loc := time.FixedZone("CST", 8*3600)
	return Status{
		Version:               StatusSchemaVersion,
		LastDecryptTS:         time.Date(2026, 5, 6, 14, 30, 0, 0, loc),
		LastDecryptDurationMs: 5234,
		GenerationID:          "20260506-143000",
		CurrentGeneration:     "20260506-143000",
		WatcherPID:            12345,
		WatcherHeartbeatTS:    time.Date(2026, 5, 6, 14, 30, 15, 0, loc),
		Healthy:               true,
		CorruptCount24h:       0,
		SuccessfulCycles24h:   96,
		SkippedCycles24h:      132,
		WeixinYieldCount24h:   23,
		DataDir:               `D:\MyFolders\xwechat_files\wxid_xxx`,
		WorkDir:               `D:\MyFolders\xwechat_files\wxid_xxx_workdir`,
	}
}

// TestStatus_StructRoundTrip：所有字段经 json.Marshal/Unmarshal 后值不变（含时区/数值精度）。
func TestStatus_StructRoundTrip(t *testing.T) {
	in := fixedStatus()

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out Status
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !out.LastDecryptTS.Equal(in.LastDecryptTS) {
		t.Errorf("LastDecryptTS round-trip mismatch: in=%v out=%v", in.LastDecryptTS, out.LastDecryptTS)
	}
	if !out.WatcherHeartbeatTS.Equal(in.WatcherHeartbeatTS) {
		t.Errorf("WatcherHeartbeatTS round-trip mismatch: in=%v out=%v", in.WatcherHeartbeatTS, out.WatcherHeartbeatTS)
	}

	// 把时间字段 zero 出来再做整体对比（time.Time 内部 wall/loc 实现细节不可比）。
	in.LastDecryptTS = time.Time{}
	in.WatcherHeartbeatTS = time.Time{}
	out.LastDecryptTS = time.Time{}
	out.WatcherHeartbeatTS = time.Time{}
	if in != out {
		t.Errorf("non-time fields drift after round-trip:\n  in =%+v\n  out=%+v", in, out)
	}
}

// TestStatus_JSONFieldNames 锁 §8.2 schema 字段名。误改字段 tag 会立刻被 catch。
func TestStatus_JSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(fixedStatus())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	must := []string{
		`"version":1`,
		`"generation_id":"20260506-143000"`,
		`"current_generation":"20260506-143000"`,
		`"watcher_pid":12345`,
		`"healthy":true`,
		`"corrupt_count_24h":0`,
		`"successful_cycles_24h":96`,
		`"skipped_cycles_24h":132`,
		`"weixin_yield_count_24h":23`,
		`"last_decrypt_duration_ms":5234`,
	}
	body := string(raw)
	for _, m := range must {
		if !strings.Contains(body, m) {
			t.Errorf("expected JSON to contain %q, got: %s", m, body)
		}
	}
}

// TestWriteReadStatus_FileRoundTrip：文件级 round-trip + 没有 .tmp 残留 + 文件存在。
func TestWriteReadStatus_FileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := fixedStatus()

	if err := WriteStatusAtomic(dir, in); err != nil {
		t.Fatalf("WriteStatusAtomic: %v", err)
	}

	statusPath := filepath.Join(dir, StatusFileName)
	if _, err := os.Stat(statusPath); err != nil {
		t.Fatalf("status.json not created: %v", err)
	}

	// .tmp 不应残留
	matches, err := filepath.Glob(filepath.Join(dir, StatusFileName+".*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("expected no temp files, got: %v", matches)
	}

	out, err := ReadStatus(dir)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if !out.LastDecryptTS.Equal(in.LastDecryptTS) {
		t.Errorf("LastDecryptTS mismatch: in=%v out=%v", in.LastDecryptTS, out.LastDecryptTS)
	}
	if out.GenerationID != in.GenerationID || out.CurrentGeneration != in.CurrentGeneration {
		t.Errorf("generation fields mismatch: in=%+v out=%+v", in, out)
	}
	if out.Version != StatusSchemaVersion {
		t.Errorf("version mismatch: %d", out.Version)
	}
}

// TestWriteStatusAtomic_Overwrite：第二次写入应原子覆盖第一次（不 append、无残留 .tmp）。
func TestWriteStatusAtomic_Overwrite(t *testing.T) {
	dir := t.TempDir()

	first := fixedStatus()
	first.GenerationID = "20260506-100000"
	first.CurrentGeneration = "20260506-100000"
	if err := WriteStatusAtomic(dir, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	second := fixedStatus()
	second.GenerationID = "20260506-200000"
	second.CurrentGeneration = "20260506-200000"
	if err := WriteStatusAtomic(dir, second); err != nil {
		t.Fatalf("second write: %v", err)
	}

	out, err := ReadStatus(dir)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if out.GenerationID != "20260506-200000" {
		t.Errorf("expected second write to overwrite, got generation_id=%q", out.GenerationID)
	}
}

// TestReadStatus_NotFound：返回的错误必须 os.IsNotExist-friendly，让上游能区分"还没初始化"。
func TestReadStatus_NotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := ReadStatus(dir)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected errors.Is(err, os.ErrNotExist), got %v", err)
	}
}

// TestReadStatus_InvalidJSON：损坏的 status.json 应返回错误，不 panic、不静默。
func TestReadStatus_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StatusFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := ReadStatus(dir)
	if err == nil {
		t.Fatalf("expected JSON parse error, got nil")
	}
}

// TestReadStatus_FutureVersion：version > 当前 schema → 显式拒绝（A6 升级语义伏笔）。
func TestReadStatus_FutureVersion(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"version":99,"healthy":true}`)
	if err := os.WriteFile(filepath.Join(dir, StatusFileName), body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := ReadStatus(dir)
	if err == nil {
		t.Fatalf("expected unsupported version error, got nil")
	}
	if !errors.Is(err, ErrUnsupportedStatusVersion) {
		t.Errorf("expected ErrUnsupportedStatusVersion, got %v", err)
	}
}

// TestReadStatus_LegacyVersionZero：缺省 version=0 视为 schema v1（向后兼容已上线但未写 version 的旧实例）。
func TestReadStatus_LegacyVersionZero(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"healthy":true,"generation_id":"20260101-000000"}`)
	if err := os.WriteFile(filepath.Join(dir, StatusFileName), body, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, err := ReadStatus(dir)
	if err != nil {
		t.Fatalf("expected legacy version=0 to parse, got err=%v", err)
	}
	if out.GenerationID != "20260101-000000" {
		t.Errorf("legacy parse lost generation_id: %+v", out)
	}
}

// TestNewGenerationID_FormatAndMonotonic：
// (1) 基础 ID 形如 YYYYMMDD-HHMMSS；(2) 同秒多次调用 ID 严格单调递增（string compare）。
func TestNewGenerationID_FormatAndMonotonic(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	t0 := time.Date(2026, 5, 6, 14, 30, 0, 0, loc)

	id1 := NewGenerationID(t0)
	if !strings.HasPrefix(id1, "20260506-143000") {
		t.Errorf("unexpected base format: %q", id1)
	}

	// 同秒第二次必须 != 且 > id1（string 比较）
	id2 := NewGenerationID(t0)
	if id2 == id1 {
		t.Errorf("same-second collision not handled: id1=%q id2=%q", id1, id2)
	}
	if id2 <= id1 {
		t.Errorf("non-monotonic same-second ids: id1=%q id2=%q", id1, id2)
	}

	// 下一秒：依然 > id2
	id3 := NewGenerationID(t0.Add(time.Second))
	if id3 <= id2 {
		t.Errorf("non-monotonic across seconds: id2=%q id3=%q", id2, id3)
	}
}

// TestNewGenerationID_ConcurrentUnique：并发调用产生的 ID 全部唯一（线程安全锁验证）。
func TestNewGenerationID_ConcurrentUnique(t *testing.T) {
	const N = 200
	ids := make([]string, N)
	var wg sync.WaitGroup
	wg.Add(N)
	now := time.Now()
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i] = NewGenerationID(now)
		}(i)
	}
	wg.Wait()

	seen := make(map[string]struct{}, N)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate generation id under concurrency: %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestResolveGenerationDir：物理路径解析（A1：server 读 current_generation 解析路径）。
func TestResolveGenerationDir(t *testing.T) {
	workDir := filepath.Join("X:", "work")
	id := "20260506-143000"
	got := ResolveGenerationDir(workDir, id)
	want := filepath.Join(workDir, "generations", id)
	if got != want {
		t.Errorf("ResolveGenerationDir = %q, want %q", got, want)
	}
}

// TestResolveGenerationDir_EmptyID：空 id 不应回落到 workDir 根（防止误把 work_dir 当 generation 用）。
func TestResolveGenerationDir_EmptyID(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on empty id")
		}
	}()
	_ = ResolveGenerationDir("X:\\work", "")
}
