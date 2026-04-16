package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenFileShared_NormalRead(t *testing.T) {
	// 创建临时文件
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	content := []byte("hello shared open")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	f, err := OpenFileShared(path)
	if err != nil {
		t.Fatalf("OpenFileShared failed: %v", err)
	}
	defer f.Close()

	buf := make([]byte, len(content))
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(buf[:n]) != string(content) {
		t.Errorf("got %q, want %q", buf[:n], content)
	}
}

func TestOpenFileShared_FileNotExist(t *testing.T) {
	_, err := OpenFileShared(filepath.Join(t.TempDir(), "nonexistent.db"))
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestReadFileShared_Content(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	content := []byte("read file shared content")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	data, err := ReadFileShared(path)
	if err != nil {
		t.Fatalf("ReadFileShared failed: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("got %q, want %q", data, content)
	}
}

func TestReadFileShared_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}

	data, err := ReadFileShared(path)
	if err != nil {
		t.Fatalf("ReadFileShared failed: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("expected empty, got %d bytes", len(data))
	}
}

func TestIsFileLockError_NilError(t *testing.T) {
	if IsFileLockError(nil) {
		t.Error("expected false for nil error")
	}
}

func TestIsFileLockError_RegularError(t *testing.T) {
	err := os.ErrNotExist
	if IsFileLockError(err) {
		t.Error("expected false for os.ErrNotExist")
	}
}

func TestIsFileLockError_PathError(t *testing.T) {
	// 一般的 PathError（非 Windows errno）不应被识别为文件锁错误
	err := &os.PathError{Op: "open", Path: "test", Err: os.ErrPermission}
	if IsFileLockError(err) {
		t.Error("expected false for permission PathError")
	}
}
