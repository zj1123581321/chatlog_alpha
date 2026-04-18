package http

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindBackupSubFolder_Found(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "拼车群(C606ACFA)"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "其他群(AABBCCDD)"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := FindBackupSubFolder(tmp, "C606ACFA")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasSuffix(got, "拼车群(C606ACFA)") {
		t.Errorf("expected path ending with 拼车群(C606ACFA), got %q", got)
	}
}

func TestFindBackupSubFolder_CaseInsensitive(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "群(abcd1234)"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 大写查小写目录应当命中
	if _, err := FindBackupSubFolder(tmp, "ABCD1234"); err != nil {
		t.Errorf("expected case-insensitive hit, got %v", err)
	}
	// 小写查也应命中
	if _, err := FindBackupSubFolder(tmp, "abcd1234"); err != nil {
		t.Errorf("expected case-insensitive hit (lowercase), got %v", err)
	}
}

func TestFindBackupSubFolder_NotFound(t *testing.T) {
	tmp := t.TempDir()
	if _, err := FindBackupSubFolder(tmp, "DEADBEEF"); err == nil {
		t.Error("expected not-found error, got nil")
	}
}

func TestFindBackupSubFolder_MissingRoot(t *testing.T) {
	if _, err := FindBackupSubFolder(filepath.Join(t.TempDir(), "does-not-exist"), "AABBCCDD"); err == nil {
		t.Error("expected error for missing root, got nil")
	}
}

func TestFindBackupSubFolder_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 创建 symlink 需要管理员或开发者模式, 先试一下能否创建
		tmp := t.TempDir()
		target := t.TempDir()
		link := filepath.Join(tmp, "fake群(CAFEBABE)")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported in this env: %v", err)
		}
	}
	backup := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(backup, "恶意(DEADBEEF)")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	_, err := FindBackupSubFolder(backup, "DEADBEEF")
	if err == nil {
		t.Fatal("expected symlink-escape error, got nil")
	}
	if !errors.Is(err, ErrBackupSymlinkEscape) {
		t.Errorf("expected ErrBackupSymlinkEscape, got %v", err)
	}
}

func TestFindBackupSubFolder_SymlinkWithinBackup(t *testing.T) {
	if runtime.GOOS == "windows" {
		tmp := t.TempDir()
		target := t.TempDir()
		link := filepath.Join(tmp, "probe")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink not supported in this env: %v", err)
		}
	}
	backup := t.TempDir()
	realDir := filepath.Join(backup, "真目录(FEEDFACE)")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 指向 backup 内部的 symlink 应该被接受
	link := filepath.Join(backup, "别名(DEADBEEF)")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := FindBackupSubFolder(backup, "DEADBEEF")
	if err != nil {
		t.Fatalf("expected success for in-backup symlink, got %v", err)
	}
	// EvalSymlinks 后应指向真目录
	if !strings.HasSuffix(got, "真目录(FEEDFACE)") {
		t.Errorf("expected resolved path to real dir, got %q", got)
	}
}

func TestFindImagesByPrefix_SingleMatch(t *testing.T) {
	tmp := t.TempDir()
	img := filepath.Join(tmp, "202604171741320215_微信图片(Zzz).jpg")
	if err := os.WriteFile(img, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FindImagesByPrefix(tmp, "20260417174132")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d: %v", len(got), got)
	}
}

func TestFindImagesByPrefix_MultipleMatches(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{
		"202604171741320215_a.jpg",
		"202604171741320317_b.png",
		"202604171742000001_other.jpg",
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("fake"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := FindImagesByPrefix(tmp, "20260417174132")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 matches, got %d: %v", len(got), got)
	}
}

func TestFindImagesByPrefix_IgnoresNonImage(t *testing.T) {
	tmp := t.TempDir()
	for _, name := range []string{
		"20260417174132_ok.jpg",
		"20260417174132_doc.txt",       // 非图片
		"20260417174132_script.sh",     // 非图片
		"20260417174132_hidden.JPG",    // 大写扩展也应当匹配
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := FindImagesByPrefix(tmp, "20260417174132")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("expected 2 image matches (.jpg + .JPG), got %d: %v", len(got), got)
	}
}

func TestFindImagesByPrefix_NoMatch(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "202604180000000000_x.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FindImagesByPrefix(tmp, "20260417")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 matches, got %d", len(got))
	}
}

func TestFindImagesByPrefix_MissingDir(t *testing.T) {
	_, err := FindImagesByPrefix(filepath.Join(t.TempDir(), "no-such-dir"), "20260417")
	if err == nil {
		t.Error("expected error for missing dir, got nil")
	}
}
