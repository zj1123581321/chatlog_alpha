package wechat

import (
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/pkg/util"
)

// retryOnFileLock retries op when it fails with a Windows file sharing/lock violation.
// For non-lock errors, it returns immediately without retry.
// baseDelay doubles on each retry (exponential backoff).
func retryOnFileLock(op func() error, maxAttempts int, baseDelay time.Duration) error {
	var lastErr error
	delay := baseDelay
	for i := 0; i < maxAttempts; i++ {
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		if !util.IsFileLockError(lastErr) {
			return lastErr
		}
		if i < maxAttempts-1 {
			log.Debug().Err(lastErr).Msgf("文件被占用，%v 后重试 (%d/%d)", delay, i+1, maxAttempts)
			time.Sleep(delay)
			delay *= 2
		}
	}
	return lastErr
}
