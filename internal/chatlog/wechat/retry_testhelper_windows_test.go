package wechat

import "golang.org/x/sys/windows"

func fileLockErrno() error {
	return windows.Errno(32) // ERROR_SHARING_VIOLATION
}
