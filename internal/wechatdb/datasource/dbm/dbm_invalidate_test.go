package dbm

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// TestDBManager_InvalidateAll_ClearsMaps：
// 给 DBManager 预先填一对 (dbs, dbPaths) 条目，调用 InvalidateAll 后两张 map 必须清空，
// 模拟 Step 5e 中 generation 切换时 server 端的 cache reset。
func TestDBManager_InvalidateAll_ClearsMaps(t *testing.T) {
	d := &DBManager{
		path:    t.TempDir(),
		id:      "test",
		dbs:     make(map[string]*sql.DB),
		dbPaths: make(map[string][]string),
	}

	// 用真 sqlite db 填一个条目（用 :memory: 避开磁盘 IO，但 Close 行为完整）
	dbPath := filepath.Join(t.TempDir(), "x.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	d.dbs[dbPath] = db
	d.dbPaths["message"] = []string{dbPath}

	d.InvalidateAll()

	if len(d.dbs) != 0 {
		t.Errorf("expected dbs empty after InvalidateAll, got %d entries", len(d.dbs))
	}
	if len(d.dbPaths) != 0 {
		t.Errorf("expected dbPaths empty, got %d entries", len(d.dbPaths))
	}
}

// TestDBManager_InvalidateAll_NoOpOnEmpty：在 dbs / dbPaths 都为空时 InvalidateAll
// 也不该 panic（首次 polling 还没建任何连接的常见场景）。
func TestDBManager_InvalidateAll_NoOpOnEmpty(t *testing.T) {
	d := &DBManager{
		path:    t.TempDir(),
		id:      "test",
		dbs:     make(map[string]*sql.DB),
		dbPaths: make(map[string][]string),
	}
	d.InvalidateAll() // 不应 panic
	if len(d.dbs) != 0 || len(d.dbPaths) != 0 {
		t.Errorf("unexpected state after no-op InvalidateAll")
	}
}
