package wechat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedLegacyDBStorage 在 workDir/db_storage/<rel> 写真 SQLite 数据库，
// 模拟旧 in-place 模式留下的解密结果。
func seedLegacyDBStorage(t *testing.T, workDir string, rel string, ddl []string, extras []string) {
	t.Helper()
	full := filepath.Join(workDir, "db_storage", rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	makeTestDB(t, full, ddl, extras...)
}

func TestDetectAndMigrate_AlreadyMigrated(t *testing.T) {
	workDir := t.TempDir()
	// 已有 generations/ → 视为迁移完成
	if err := os.MkdirAll(filepath.Join(workDir, "generations", "20260506-100000"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := DetectAndMigrate(MigrationOpts{WorkDir: workDir})
	if err != nil {
		t.Fatalf("DetectAndMigrate: %v", err)
	}
	if res.Outcome != MigrationAlreadyMigrated {
		t.Errorf("expected AlreadyMigrated, got %s", res.Outcome)
	}
	// 不应触碰 status.json
	if _, err := os.Stat(filepath.Join(workDir, StatusFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no status.json change")
	}
}

func TestDetectAndMigrate_Fresh(t *testing.T) {
	workDir := t.TempDir()
	res, err := DetectAndMigrate(MigrationOpts{WorkDir: workDir})
	if err != nil {
		t.Fatalf("DetectAndMigrate: %v", err)
	}
	if res.Outcome != MigrationFresh {
		t.Errorf("expected Fresh, got %s", res.Outcome)
	}
}

func TestDetectAndMigrate_SwapsValidLegacy(t *testing.T) {
	workDir := t.TempDir()
	rel := "message/multi/message_0.db"
	seedLegacyDBStorage(t, workDir, rel,
		[]string{`CREATE TABLE Timestamp (ts INTEGER PRIMARY KEY)`},
		[]string{`INSERT INTO Timestamp VALUES (1)`})

	res, err := DetectAndMigrate(MigrationOpts{
		WorkDir: workDir,
		DBs: []DBJob{{
			RelPath: rel,
			Schema: SchemaSpec{
				ExpectedTables: []string{"Timestamp"},
				SmokeQuery:     "SELECT count(*) FROM Timestamp",
			},
		}},
	})
	if err != nil {
		t.Fatalf("DetectAndMigrate: %v", err)
	}
	if res.Outcome != MigrationSwapped {
		t.Fatalf("expected Swapped, got %s reason=%s", res.Outcome, res.Reason)
	}
	if res.GenerationID == "" {
		t.Errorf("expected gen id")
	}

	// status.json 写入并 current_generation 设为新 id
	st, err := ReadStatus(workDir)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.CurrentGeneration != res.GenerationID {
		t.Errorf("current_generation = %q, want %q", st.CurrentGeneration, res.GenerationID)
	}

	// 旧 db_storage 不在原地了
	if _, err := os.Stat(filepath.Join(workDir, "db_storage")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected legacy db_storage moved away, got stat err=%v", err)
	}
	// 新位置存在
	if _, err := os.Stat(filepath.Join(ResolveGenerationDir(workDir, res.GenerationID), "db_storage", rel)); err != nil {
		t.Errorf("expected migrated db at new location: %v", err)
	}
}

func TestDetectAndMigrate_CorruptOnSchemaFail(t *testing.T) {
	workDir := t.TempDir()
	rel := "message/multi/message_0.db"
	// 没有 Timestamp 表 → 必败
	seedLegacyDBStorage(t, workDir, rel,
		[]string{`CREATE TABLE Other (x INTEGER)`}, nil)

	res, err := DetectAndMigrate(MigrationOpts{
		WorkDir: workDir,
		DBs: []DBJob{{
			RelPath: rel,
			Schema: SchemaSpec{
				ExpectedTables: []string{"Timestamp"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("DetectAndMigrate: %v", err)
	}
	if res.Outcome != MigrationCorrupt {
		t.Errorf("expected Corrupt, got %s reason=%s", res.Outcome, res.Reason)
	}

	// status.json 不该有 current_generation（迁移失败 → 不切）
	if st, _ := ReadStatus(workDir); st.CurrentGeneration != "" {
		t.Errorf("current_generation should be empty after corrupt migration, got %q", st.CurrentGeneration)
	}

	// gen 目录已 mv 到 corrupt/
	corruptRoot := filepath.Join(workDir, "corrupt")
	ents, err := os.ReadDir(corruptRoot)
	if err != nil {
		t.Fatalf("readdir corrupt: %v", err)
	}
	if len(ents) != 1 || !strings.Contains(ents[0].Name(), "migration") {
		t.Errorf("expected single corrupt entry naming migration, got %v", namesOf(ents))
	}
}

// TestDetectAndMigrate_NoDBJobsStillSwaps：caller 没传 DBJobs（极简启动场景），
// 跳过 schema check 直接 swap 当前 generation。这给"不知道有什么 db"的极端
// 场景一个安全 fallback。
func TestDetectAndMigrate_NoDBJobsStillSwaps(t *testing.T) {
	workDir := t.TempDir()
	rel := "message/multi/message_0.db"
	seedLegacyDBStorage(t, workDir, rel, []string{`CREATE TABLE x (a INTEGER)`}, nil)

	res, err := DetectAndMigrate(MigrationOpts{WorkDir: workDir})
	if err != nil {
		t.Fatalf("DetectAndMigrate: %v", err)
	}
	if res.Outcome != MigrationSwapped {
		t.Errorf("expected Swapped (no jobs = skip schema), got %s", res.Outcome)
	}
}
