package wechat

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/pkg/util"
)

// retryOnFileLockCtx retries op when it fails with a Windows file sharing/lock violation.
// For non-lock errors, it returns immediately without retry.
// baseDelay doubles on each retry (exponential backoff).
//
// 当 ctx 被 cancel 时立即返回 ctx.Err()，避免 Stop 时被 5s × 5 次 = 25s 累积 backoff
// 卡死。这是 /plan-eng-review 的 Tension #3 "ctx plumbing 范围" 的精准加固点 ——
// 其他 blocking op 由 5s WaitGroup 超时兜底，只有这个显式的长 sleep 必须走 ctx。
func retryOnFileLockCtx(ctx context.Context, op func() error, maxAttempts int, baseDelay time.Duration) error {
	var lastErr error
	delay := baseDelay
	for i := 0; i < maxAttempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		if !util.IsFileLockError(lastErr) {
			return lastErr
		}
		if i < maxAttempts-1 {
			log.Debug().Err(lastErr).Msgf("文件被占用，%v 后重试 (%d/%d)", delay, i+1, maxAttempts)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
	}
	return lastErr
}

// retryOnFileLock 向后兼容包装，等同 retryOnFileLockCtx(context.Background(), ...)。
// 新代码应直接使用 retryOnFileLockCtx 以支持 Stop cancel。
func retryOnFileLock(op func() error, maxAttempts int, baseDelay time.Duration) error {
	return retryOnFileLockCtx(context.Background(), op, maxAttempts, baseDelay)
}
