package wechat

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeFakeDB 写一个最小的 SQLite 数据库 header（前 16 字节 magic + 后续填零到 100 字节）。
// 不需要真的能被 SQLite open，只需通过 magic 检查。
func makeFakeDB(t *testing.T, path string) {
	t.Helper()
	page1 := make([]byte, 100)
	copy(page1, []byte("SQLite format 3\x00"))
	if err := os.WriteFile(path, page1, 0o600); err != nil {
		t.Fatalf("write fake db: %v", err)
	}
}

// makeFakeWAL 写一个最小的 WAL header（32 字节，magic = 0x377f0683 big-endian）。
func makeFakeWAL(t *testing.T, path string) {
	t.Helper()
	hdr := make([]byte, 32)
	binary.BigEndian.PutUint32(hdr[:4], 0x377f0683)
	binary.BigEndian.PutUint32(hdr[4:8], 3007000)   // format version
	binary.BigEndian.PutUint32(hdr[8:12], 4096)     // page size
	binary.BigEndian.PutUint32(hdr[12:16], 0)       // checkpoint sequence
	binary.BigEndian.PutUint32(hdr[16:20], 0xCAFEF00D) // salt-1
	binary.BigEndian.PutUint32(hdr[20:24], 0xDEADBEEF) // salt-2
	if err := os.WriteFile(path, hdr, 0o600); err != nil {
		t.Fatalf("write fake wal: %v", err)
	}
}

// TestCopyDBPair_WALFirstThenDB：
// 验证 Eng A2 的复制顺序：-wal 在前、.db 在后。
// 通过比较 dst .db 的 mtime 是否 ≥ dst .db-wal 的 mtime 来判断顺序。
func TestCopyDBPair_WALFirstThenDB(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcDB := filepath.Join(srcDir, "message_0.db")
	srcWAL := srcDB + "-wal"
	makeFakeDB(t, srcDB)
	makeFakeWAL(t, srcWAL)

	dstDB := filepath.Join(dstDir, "message_0.db")
	walCopied, err := CopyDBPair(srcDB, dstDB)
	if err != nil {
		t.Fatalf("CopyDBPair: %v", err)
	}
	if !walCopied {
		t.Errorf("expected walCopied=true")
	}

	dstWAL := dstDB + "-wal"
	dstDBStat, err := os.Stat(dstDB)
	if err != nil {
		t.Fatalf("stat dst db: %v", err)
	}
	dstWALStat, err := os.Stat(dstWAL)
	if err != nil {
		t.Fatalf("stat dst wal: %v", err)
	}
	// .db 是后写的，mtime 应当 ≥ .db-wal 的 mtime
	if dstDBStat.ModTime().Before(dstWALStat.ModTime()) {
		t.Errorf("copy order violated: dst db mtime (%v) before dst wal mtime (%v)",
			dstDBStat.ModTime(), dstWALStat.ModTime())
	}
}

// TestCopyDBPair_OnlyMainDB：源目录只有 .db 没有 -wal，复制只动 .db，walCopied=false。
func TestCopyDBPair_OnlyMainDB(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcDB := filepath.Join(srcDir, "message_0.db")
	makeFakeDB(t, srcDB)

	dstDB := filepath.Join(dstDir, "message_0.db")
	walCopied, err := CopyDBPair(srcDB, dstDB)
	if err != nil {
		t.Fatalf("CopyDBPair: %v", err)
	}
	if walCopied {
		t.Errorf("expected walCopied=false when no -wal")
	}
	if _, err := os.Stat(dstDB + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no dst wal, got stat err=%v", err)
	}
}

// TestCopyDBPair_SkipsSHM：源目录有 -shm，CopyDBPair 不应碰它（A2：跳过 -shm）。
func TestCopyDBPair_SkipsSHM(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcDB := filepath.Join(srcDir, "message_0.db")
	makeFakeDB(t, srcDB)
	srcSHM := srcDB + "-shm"
	if err := os.WriteFile(srcSHM, []byte("shm content"), 0o600); err != nil {
		t.Fatalf("write shm: %v", err)
	}

	dstDB := filepath.Join(dstDir, "message_0.db")
	if _, err := CopyDBPair(srcDB, dstDB); err != nil {
		t.Fatalf("CopyDBPair: %v", err)
	}
	if _, err := os.Stat(dstDB + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected -shm to be skipped, got stat err=%v", err)
	}
}

// TestCopyDBPair_DstNotExist：dst 目录不存在 → 返回错误（CopyDBPair 不替 caller mkdir）。
func TestCopyDBPair_DstNotExist(t *testing.T) {
	srcDir := t.TempDir()
	srcDB := filepath.Join(srcDir, "message_0.db")
	makeFakeDB(t, srcDB)

	dstDB := filepath.Join(t.TempDir(), "missing-subdir", "message_0.db")
	if _, err := CopyDBPair(srcDB, dstDB); err == nil {
		t.Errorf("expected error when dst dir missing")
	}
}

// TestCheckWALCoherency_ValidPair：正常 .db + .db-wal mtime 接近，通过。
func TestCheckWALCoherency_ValidPair(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	walPath := dbPath + "-wal"
	makeFakeDB(t, dbPath)
	makeFakeWAL(t, walPath)

	if err := CheckWALCoherency(dbPath, walPath, 2*time.Second); err != nil {
		t.Errorf("expected coherent, got %v", err)
	}
}

// TestCheckWALCoherency_NoWAL：walPath="" 跳过 wal 检查，只验 .db magic。
func TestCheckWALCoherency_NoWAL(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	makeFakeDB(t, dbPath)

	if err := CheckWALCoherency(dbPath, "", 2*time.Second); err != nil {
		t.Errorf("expected pass when no wal, got %v", err)
	}
}

// TestCheckWALCoherency_InvalidDBMagic：.db 不是合法 SQLite header → 失败。
func TestCheckWALCoherency_InvalidDBMagic(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	if err := os.WriteFile(dbPath, []byte("not-a-sqlite-file-at-all"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckWALCoherency(dbPath, "", 2*time.Second)
	if !errors.Is(err, ErrWALIncoherent) {
		t.Errorf("expected ErrWALIncoherent, got %v", err)
	}
}

// TestCheckWALCoherency_InvalidWALMagic：.db-wal magic 不对 → 失败。
func TestCheckWALCoherency_InvalidWALMagic(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	walPath := dbPath + "-wal"
	makeFakeDB(t, dbPath)
	bogus := make([]byte, 32)
	binary.BigEndian.PutUint32(bogus[:4], 0xdeadbeef)
	if err := os.WriteFile(walPath, bogus, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckWALCoherency(dbPath, walPath, 2*time.Second)
	if !errors.Is(err, ErrWALIncoherent) {
		t.Errorf("expected ErrWALIncoherent, got %v", err)
	}
}

// TestCheckWALCoherency_MtimeOutOfWindow：mtime 偏差 > window → 失败。
func TestCheckWALCoherency_MtimeOutOfWindow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	walPath := dbPath + "-wal"
	makeFakeDB(t, dbPath)
	makeFakeWAL(t, walPath)

	// 把 .db-wal mtime 拨到 10s 前
	old := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(walPath, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	err := CheckWALCoherency(dbPath, walPath, 2*time.Second)
	if !errors.Is(err, ErrWALIncoherent) {
		t.Errorf("expected ErrWALIncoherent, got %v", err)
	}
}

// TestCheckWALCoherency_DBMissing：.db 不存在 → 返回错误（不静默通过）。
func TestCheckWALCoherency_DBMissing(t *testing.T) {
	dir := t.TempDir()
	err := CheckWALCoherency(filepath.Join(dir, "nope.db"), "", 2*time.Second)
	if err == nil {
		t.Errorf("expected error for missing db")
	}
}

// TestCheckWALCoherency_DBTruncated：.db 短于 16 字节 → 失败。
func TestCheckWALCoherency_DBTruncated(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "x.db")
	if err := os.WriteFile(dbPath, []byte("SQLite f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckWALCoherency(dbPath, "", 2*time.Second); err == nil {
		t.Errorf("expected truncated db to fail")
	}
}
