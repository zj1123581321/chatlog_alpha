package wechat

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// makeTestDB 在 path 写一个真 SQLite 数据库，含给定 schema/数据。
// tablesDDL 是要执行的 CREATE TABLE 语句切片，extras 是额外要跑的 SQL（INSERT 等）。
func makeTestDB(t *testing.T, path string, tablesDDL []string, extras ...string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, ddl := range tablesDDL {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("ddl %q: %v", ddl, err)
		}
	}
	for _, e := range extras {
		if _, err := db.Exec(e); err != nil {
			t.Fatalf("extra %q: %v", e, err)
		}
	}
}

// chatlogSpec 模拟 message_*.db 业务级 schema：必有 Timestamp 表，热表查询计数。
func chatlogSpec() SchemaSpec {
	return SchemaSpec{
		ExpectedTables: []string{"Timestamp"},
		SmokeQuery:     "SELECT count(*) FROM Timestamp",
	}
}

func TestValidateSchemaCheckMode_QuickAndFull(t *testing.T) {
	for _, in := range []string{"quick", "full"} {
		got, err := ValidateSchemaCheckMode(in)
		if err != nil {
			t.Errorf("%q: unexpected error: %v", in, err)
		}
		if string(got) != in {
			t.Errorf("%q: got %q", in, got)
		}
	}
}

func TestValidateSchemaCheckMode_Invalid(t *testing.T) {
	if _, err := ValidateSchemaCheckMode("paranoid"); err == nil {
		t.Errorf("expected error for invalid mode")
	}
	if _, err := ValidateSchemaCheckMode(""); err == nil {
		t.Errorf("expected error for empty mode")
	}
}

func TestCheckSchema_QuickValid(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "message_0.db")
	makeTestDB(t, dbPath, []string{
		`CREATE TABLE Timestamp (ts INTEGER PRIMARY KEY)`,
		`CREATE TABLE Message (id INTEGER PRIMARY KEY, body TEXT)`,
	}, `INSERT INTO Timestamp VALUES (1700000000)`)

	if err := CheckSchema(dbPath, chatlogSpec(), SchemaCheckQuick); err != nil {
		t.Errorf("expected valid db to pass, got %v", err)
	}
}

func TestCheckSchema_QuickMissingExpectedTable(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "broken.db")
	// 故意不建 Timestamp 表 —— 模拟 4/25 race 损坏
	makeTestDB(t, dbPath, []string{
		`CREATE TABLE Message (id INTEGER PRIMARY KEY)`,
	})

	err := CheckSchema(dbPath, chatlogSpec(), SchemaCheckQuick)
	if !errors.Is(err, ErrSchemaGateFailed) {
		t.Errorf("expected ErrSchemaGateFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "Timestamp") {
		t.Errorf("expected error to name missing table, got %v", err)
	}
}

func TestCheckSchema_QuickSmokeQueryFails(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	makeTestDB(t, dbPath, []string{`CREATE TABLE Timestamp (ts INTEGER)`})
	spec := SchemaSpec{
		ExpectedTables: []string{"Timestamp"},
		SmokeQuery:     "SELECT count(*) FROM TableThatDoesNotExist",
	}
	err := CheckSchema(dbPath, spec, SchemaCheckQuick)
	if !errors.Is(err, ErrSchemaGateFailed) {
		t.Errorf("expected ErrSchemaGateFailed, got %v", err)
	}
}

func TestCheckSchema_EmptySpecSkipsTableAndSmoke(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")
	makeTestDB(t, dbPath, []string{`CREATE TABLE x (a INTEGER)`})
	// 空 spec: 只跑 quick_check
	if err := CheckSchema(dbPath, SchemaSpec{}, SchemaCheckQuick); err != nil {
		t.Errorf("expected pass on empty spec + good db, got %v", err)
	}
}

func TestCheckSchema_QuickCheckDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "corrupt.db")
	makeTestDB(t, dbPath, []string{
		`CREATE TABLE Timestamp (ts INTEGER PRIMARY KEY)`,
	}, `INSERT INTO Timestamp VALUES (1), (2), (3)`)

	// 损坏 db：在第二页位置（offset 4096）开始覆盖一段乱字节
	if err := corruptPage(dbPath, 4096, 256); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	err := CheckSchema(dbPath, chatlogSpec(), SchemaCheckQuick)
	if err == nil {
		t.Errorf("expected corrupt db to fail schema gate")
	}
}

func TestCheckSchema_FullModeRunsIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	makeTestDB(t, dbPath, []string{
		`CREATE TABLE Timestamp (ts INTEGER PRIMARY KEY)`,
	}, `INSERT INTO Timestamp VALUES (1)`)
	// full 模式良性 db 应通过
	if err := CheckSchema(dbPath, chatlogSpec(), SchemaCheckFull); err != nil {
		t.Errorf("expected full mode to pass on good db, got %v", err)
	}
}

func TestCheckSchema_DBNotFound(t *testing.T) {
	dir := t.TempDir()
	err := CheckSchema(filepath.Join(dir, "nope.db"), chatlogSpec(), SchemaCheckQuick)
	if err == nil {
		t.Errorf("expected error for missing db")
	}
}

// corruptPage 覆盖文件 [offset, offset+length) 范围为乱字节，模拟数据 page 损坏。
// 用 RDWR + WriteAt 直接落盘，跳过 SQLite 层。
func corruptPage(path string, offset, length int64) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	garbage := make([]byte, length)
	for i := range garbage {
		garbage[i] = byte(0xA5 ^ (i & 0xff))
	}
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() < offset+length {
		// 文件太小没法在 offset 处损坏 → 测试 setup 错了
		return fmt.Errorf("db file size %d < %d, can't corrupt at offset %d", stat.Size(), offset+length, offset)
	}
	_, err = f.WriteAt(garbage, offset)
	return err
}
