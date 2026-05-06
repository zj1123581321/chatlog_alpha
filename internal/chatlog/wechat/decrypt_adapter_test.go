package wechat

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestBuildDBJobs_FindsDBsAndSkipsFTS(t *testing.T) {
	dataDir := t.TempDir()
	mustWrite := func(rel string, body string) {
		full := filepath.Join(dataDir, "db_storage", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("message/multi/message_0.db", "x")
	mustWrite("contact/contact.db", "x")
	mustWrite("session/session.db-wal", "x")               // -wal 应跳过
	mustWrite("session/session.db", "x")                   // 但 .db 仍要纳入
	mustWrite("fts/idx.db", "x")                           // fts/ 子目录应整体跳过
	mustWrite("message/multi/message_0.db-shm", "x")       // -shm 跳过

	jobs, err := BuildDBJobs(dataDir, nil)
	if err != nil {
		t.Fatalf("BuildDBJobs: %v", err)
	}
	var rels []string
	for _, j := range jobs {
		rels = append(rels, j.RelPath)
	}
	sort.Strings(rels)

	want := []string{
		filepath.Join("contact", "contact.db"),
		filepath.Join("message", "multi", "message_0.db"),
		filepath.Join("session", "session.db"),
	}
	if len(rels) != len(want) {
		t.Fatalf("expected %d jobs, got %d: %v", len(want), len(rels), rels)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Errorf("job %d: got %q, want %q", i, rels[i], want[i])
		}
	}
}

func TestBuildDBJobs_AppliesSchemaLookup(t *testing.T) {
	dataDir := t.TempDir()
	full := filepath.Join(dataDir, "db_storage", "message/multi/message_0.db")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	lookup := func(rel string) SchemaSpec {
		if filepath.Base(rel) == "message_0.db" {
			return SchemaSpec{ExpectedTables: []string{"Timestamp"}, SmokeQuery: "SELECT 1"}
		}
		return SchemaSpec{}
	}
	jobs, err := BuildDBJobs(dataDir, lookup)
	if err != nil {
		t.Fatalf("BuildDBJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if len(jobs[0].Schema.ExpectedTables) != 1 || jobs[0].Schema.ExpectedTables[0] != "Timestamp" {
		t.Errorf("schema lookup didn't apply: %+v", jobs[0].Schema)
	}
}

func TestBuildDBJobs_DBStorageMissing(t *testing.T) {
	dataDir := t.TempDir()
	// 不创建 db_storage/ → walk 应返回错误
	if _, err := BuildDBJobs(dataDir, nil); err == nil {
		t.Errorf("expected error when db_storage missing")
	}
}
