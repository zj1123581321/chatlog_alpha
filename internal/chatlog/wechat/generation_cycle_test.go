package wechat

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// fakeDataDir 在 dataDir/db_storage/<rel> 写一个最小合法 SQLite header。
// CopyDBPair + CheckWALCoherency 只看 header，不需要真 db open。
func fakeDataDir(t *testing.T, dataDir string, rels []string) {
	t.Helper()
	for _, rel := range rels {
		full := filepath.Join(dataDir, "db_storage", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		makeFakeDB(t, full)
	}
}

// fakeDecrypt 模拟 chatlog decrypt：把 rawDir/db_storage/<rel> "解密"成
// dstDir/db_storage/<rel> —— 真 SQLite 文件，含调用方期望的 schema。
func fakeDecryptWithSchema(rels []string, ddl []string, extras []string) func(rawDir, dstDir string) error {
	return func(rawDir, dstDir string) error {
		for _, rel := range rels {
			rawPath := filepath.Join(rawDir, "db_storage", rel)
			if _, err := os.Stat(rawPath); err != nil {
				return fmt.Errorf("fake decrypt: raw missing %s: %w", rel, err)
			}
			dstPath := filepath.Join(dstDir, "db_storage", rel)
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return err
			}
			db, err := sql.Open("sqlite3", dstPath)
			if err != nil {
				return err
			}
			for _, q := range ddl {
				if _, err := db.Exec(q); err != nil {
					_ = db.Close()
					return err
				}
			}
			for _, q := range extras {
				if _, err := db.Exec(q); err != nil {
					_ = db.Close()
					return err
				}
			}
			if err := db.Close(); err != nil {
				return err
			}
		}
		return nil
	}
}

func msgJob(rel string) DBJob {
	return DBJob{
		RelPath: rel,
		Schema: SchemaSpec{
			ExpectedTables: []string{"Timestamp"},
			SmokeQuery:     "SELECT count(*) FROM Timestamp",
		},
	}
}

func TestRunGenerationCycle_HappyPath(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	res, err := RunGenerationCycle(CycleInput{
		WorkDir: workDir,
		DataDir: dataDir,
		DBs:     []DBJob{msgJob(rels[0])},
		DecryptFunc: fakeDecryptWithSchema(rels,
			[]string{`CREATE TABLE Timestamp (ts INTEGER PRIMARY KEY)`},
			[]string{`INSERT INTO Timestamp VALUES (1)`}),
		WatcherPID: 12345,
	})
	if err != nil {
		t.Fatalf("RunGenerationCycle: %v", err)
	}
	if res.Outcome != OutcomeSwapped {
		t.Fatalf("expected swapped, got %s (reason=%s)", res.Outcome, res.Reason)
	}
	if res.GenerationID == "" {
		t.Errorf("expected non-empty generation id")
	}

	// status.json 写入且 current_generation 是新 id
	st, err := ReadStatus(workDir)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.CurrentGeneration != res.GenerationID {
		t.Errorf("status.current_generation = %q, want %q", st.CurrentGeneration, res.GenerationID)
	}
	if st.GenerationID != res.GenerationID {
		t.Errorf("status.generation_id = %q, want %q", st.GenerationID, res.GenerationID)
	}
	if st.WatcherPID != 12345 {
		t.Errorf("status.watcher_pid = %d, want 12345", st.WatcherPID)
	}
	if !st.Healthy {
		t.Errorf("expected healthy=true on success")
	}
	if st.SuccessfulCycles24h != 1 {
		t.Errorf("expected SuccessfulCycles24h=1, got %d", st.SuccessfulCycles24h)
	}

	// generation 目录存在 + db_storage 子树存在
	genDir := ResolveGenerationDir(workDir, res.GenerationID)
	if _, err := os.Stat(filepath.Join(genDir, "db_storage", rels[0])); err != nil {
		t.Errorf("decrypted db missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "raw", "db_storage", rels[0])); err != nil {
		t.Errorf("raw db missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(genDir, "manifest.json")); err != nil {
		t.Errorf("manifest missing: %v", err)
	}
}

func TestRunGenerationCycle_SchemaFailKeepsCurrent(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	// 先写一个状态：current=PRIOR
	prior := Status{
		Version:           StatusSchemaVersion,
		CurrentGeneration: "20260506-100000-prior",
		GenerationID:      "20260506-100000-prior",
		Healthy:           true,
		SuccessfulCycles24h: 5,
	}
	if err := WriteStatusAtomic(workDir, prior); err != nil {
		t.Fatalf("seed status: %v", err)
	}

	// fake decrypt 不建 Timestamp 表 → schema gate 必败
	res, err := RunGenerationCycle(CycleInput{
		WorkDir:     workDir,
		DataDir:     dataDir,
		DBs:         []DBJob{msgJob(rels[0])},
		DecryptFunc: fakeDecryptWithSchema(rels, []string{`CREATE TABLE Other (x INTEGER)`}, nil),
	})
	if err != nil {
		t.Fatalf("RunGenerationCycle: %v", err)
	}
	if res.Outcome != OutcomeCorrupt {
		t.Fatalf("expected corrupt outcome, got %s", res.Outcome)
	}

	// 现 status.current 必须仍是 PRIOR
	st, err := ReadStatus(workDir)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if st.CurrentGeneration != "20260506-100000-prior" {
		t.Errorf("current_generation should not change on failure, got %q", st.CurrentGeneration)
	}
	if st.GenerationID != res.GenerationID {
		t.Errorf("generation_id should track latest attempt: got %q want %q", st.GenerationID, res.GenerationID)
	}
	if st.CorruptCount24h != 1 {
		t.Errorf("expected CorruptCount24h=1 after fail, got %d", st.CorruptCount24h)
	}
	if st.SuccessfulCycles24h != 5 {
		t.Errorf("SuccessfulCycles24h should preserve prior=5, got %d", st.SuccessfulCycles24h)
	}

	// gen 目录已经搬到 corrupt/
	corruptRoot := filepath.Join(workDir, "corrupt")
	ents, err := os.ReadDir(corruptRoot)
	if err != nil {
		t.Fatalf("readdir corrupt: %v", err)
	}
	if len(ents) != 1 || !strings.HasPrefix(ents[0].Name(), res.GenerationID+"-schema") {
		t.Errorf("expected one corrupt entry %s-schema*, got %v", res.GenerationID, namesOf(ents))
	}

	// 原 generations 下的活动 gen 目录已不在
	gen := ResolveGenerationDir(workDir, res.GenerationID)
	if _, err := os.Stat(gen); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected gen moved away, got stat err=%v", err)
	}
}

func namesOf(ents []os.DirEntry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestRunGenerationCycle_DecryptFailGoesCorrupt(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	failingDecrypt := func(rawDir, dstDir string) error {
		return errors.New("decrypt: bad key")
	}

	res, err := RunGenerationCycle(CycleInput{
		WorkDir:     workDir,
		DataDir:     dataDir,
		DBs:         []DBJob{msgJob(rels[0])},
		DecryptFunc: failingDecrypt,
	})
	if err != nil {
		t.Fatalf("RunGenerationCycle: %v", err)
	}
	if res.Outcome != OutcomeCorrupt {
		t.Errorf("expected corrupt, got %s", res.Outcome)
	}
	if !strings.Contains(res.Reason, "decrypt") {
		t.Errorf("expected reason to mention decrypt, got %q", res.Reason)
	}
}

func TestRunGenerationCycle_EmptyDBsReturnsSkipped(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	res, err := RunGenerationCycle(CycleInput{
		WorkDir: workDir,
		DataDir: dataDir,
		DBs:     nil,
	})
	if err != nil {
		t.Fatalf("RunGenerationCycle: %v", err)
	}
	if res.Outcome != OutcomeSkipped {
		t.Errorf("expected skipped, got %s", res.Outcome)
	}
	// 不该创建 generations/ 也不该写 status.json
	if _, err := os.Stat(filepath.Join(workDir, "generations")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no generations/ dir")
	}
}

func TestRunGenerationCycle_CopyFailsWhenSrcMissing(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	// 故意不创建任何 db
	res, err := RunGenerationCycle(CycleInput{
		WorkDir: workDir,
		DataDir: dataDir,
		DBs:     []DBJob{msgJob("missing.db")},
		DecryptFunc: func(rawDir, dstDir string) error {
			t.Fatalf("decrypt should not be called when copy fails")
			return nil
		},
	})
	if err != nil {
		t.Fatalf("RunGenerationCycle: %v", err)
	}
	if res.Outcome != OutcomeCorrupt {
		t.Errorf("expected corrupt, got %s", res.Outcome)
	}
}

// TestRunGenerationCycle_TwoSequentialSwaps：连续两次成功 cycle，current_generation
// 跟随最新 id；两份 generation 目录都还在（prune 不属本步）。
func TestRunGenerationCycle_TwoSequentialSwaps(t *testing.T) {
	workDir := t.TempDir()
	dataDir := t.TempDir()
	rels := []string{"message/multi/message_0.db"}
	fakeDataDir(t, dataDir, rels)

	first, err := RunGenerationCycle(CycleInput{
		WorkDir: workDir,
		DataDir: dataDir,
		DBs:     []DBJob{msgJob(rels[0])},
		DecryptFunc: fakeDecryptWithSchema(rels,
			[]string{`CREATE TABLE Timestamp (ts INTEGER)`}, nil),
		Now: func() time.Time { return time.Date(2026, 5, 6, 14, 30, 0, 0, time.UTC) },
	})
	if err != nil || first.Outcome != OutcomeSwapped {
		t.Fatalf("first cycle: %+v err=%v", first, err)
	}

	second, err := RunGenerationCycle(CycleInput{
		WorkDir: workDir,
		DataDir: dataDir,
		DBs:     []DBJob{msgJob(rels[0])},
		DecryptFunc: fakeDecryptWithSchema(rels,
			[]string{`CREATE TABLE Timestamp (ts INTEGER)`}, nil),
		Now: func() time.Time { return time.Date(2026, 5, 6, 14, 35, 0, 0, time.UTC) },
	})
	if err != nil || second.Outcome != OutcomeSwapped {
		t.Fatalf("second cycle: %+v err=%v", second, err)
	}
	if first.GenerationID == second.GenerationID {
		t.Errorf("expected distinct gen ids")
	}

	st, err := ReadStatus(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.CurrentGeneration != second.GenerationID {
		t.Errorf("current=%q, want %q", st.CurrentGeneration, second.GenerationID)
	}
	if st.SuccessfulCycles24h != 2 {
		t.Errorf("SuccessfulCycles24h=%d, want 2", st.SuccessfulCycles24h)
	}

	// 两份 generation 目录都在
	for _, id := range []string{first.GenerationID, second.GenerationID} {
		if _, err := os.Stat(ResolveGenerationDir(workDir, id)); err != nil {
			t.Errorf("expected gen %s to remain, got %v", id, err)
		}
	}
}
