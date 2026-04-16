package util

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// OpenFileShared opens a file with full sharing flags on Windows,
// so that chatlog's reads never block WeChat's writes.
func OpenFileShared(path string) (*os.File, error) {
	pathp, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(
		pathp,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// ReadFileShared reads a file using shared mode open.
func ReadFileShared(path string) ([]byte, error) {
	f, err := OpenFileShared(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// IsFileLockError returns true if the error is a Windows file sharing/lock violation.
func IsFileLockError(err error) bool {
	if err == nil {
		return false
	}
	if pe, ok := err.(*os.PathError); ok {
		if errno, ok := pe.Err.(windows.Errno); ok {
			// ERROR_SHARING_VIOLATION (32) or ERROR_LOCK_VIOLATION (33)
			return errno == 32 || errno == 33
		}
	}
	return false
}
