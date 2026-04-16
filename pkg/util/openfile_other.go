//go:build !windows

package util

import "os"

// OpenFileShared opens a file for reading. On non-Windows platforms,
// this is equivalent to os.Open since POSIX doesn't have sharing violations.
func OpenFileShared(path string) (*os.File, error) {
	return os.Open(path)
}

// ReadFileShared reads a file. On non-Windows platforms, this is equivalent to os.ReadFile.
func ReadFileShared(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// IsFileLockError always returns false on non-Windows platforms.
func IsFileLockError(err error) bool {
	return false
}
