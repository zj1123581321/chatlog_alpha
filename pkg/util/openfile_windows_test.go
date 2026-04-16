package util

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestOpenFileShared_WhileWriteLocked 模拟微信正在写入文件时，chatlog 仍能读取
func TestOpenFileShared_WhileWriteLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "message_0.db")
	content := []byte("encrypted database content here")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	// 用独占写模式打开文件（模拟微信持有写锁）
	pathp, _ := windows.UTF16PtrFromString(path)
	wechatHandle, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, // 微信只允许共享读，不允许共享写
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("failed to simulate WeChat file lock: %v", err)
	}
	defer windows.CloseHandle(wechatHandle)

	// chatlog 用共享模式读取 — 应该成功
	f, err := OpenFileShared(path)
	if err != nil {
		t.Fatalf("OpenFileShared should succeed while file is write-locked, got: %v", err)
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

// TestStdOpenFails_WhileWriteLocked 验证标准 os.Open 在文件被独占时会失败
// 这确认了问题确实存在，从而证明 OpenFileShared 的必要性
func TestStdOpenFails_WhileWriteLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "message_0.db")
	if err := os.WriteFile(path, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	// 用不共享模式打开（模拟极端情况）
	pathp, _ := windows.UTF16PtrFromString(path)
	handle, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, // 不允许任何共享
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("failed to lock file: %v", err)
	}
	defer windows.CloseHandle(handle)

	// 标准 os.Open 和 OpenFileShared 都应该失败
	_, err = os.Open(path)
	if err == nil {
		t.Error("expected os.Open to fail when file is exclusively locked")
	}

	_, err = OpenFileShared(path)
	if err == nil {
		t.Error("expected OpenFileShared to fail when file is exclusively locked (no share flags)")
	}
	// 这种情况应该被识别为文件锁错误
	if !IsFileLockError(err) {
		t.Errorf("expected IsFileLockError=true for sharing violation, got false (err: %v)", err)
	}
}

// TestIsFileLockError_SharingViolation 验证 Windows 特定的 sharing violation 错误码
func TestIsFileLockError_SharingViolation(t *testing.T) {
	err := &os.PathError{
		Op:   "open",
		Path: "test.db",
		Err:  windows.Errno(32), // ERROR_SHARING_VIOLATION
	}
	if !IsFileLockError(err) {
		t.Error("expected true for ERROR_SHARING_VIOLATION")
	}
}

// TestIsFileLockError_LockViolation 验证 Windows 特定的 lock violation 错误码
func TestIsFileLockError_LockViolation(t *testing.T) {
	err := &os.PathError{
		Op:   "open",
		Path: "test.db",
		Err:  windows.Errno(33), // ERROR_LOCK_VIOLATION
	}
	if !IsFileLockError(err) {
		t.Error("expected true for ERROR_LOCK_VIOLATION")
	}
}
