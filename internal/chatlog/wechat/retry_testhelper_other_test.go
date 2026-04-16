//go:build !windows

package wechat

import "syscall"

func fileLockErrno() error {
	return syscall.EACCES
}
