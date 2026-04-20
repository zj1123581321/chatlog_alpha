package wechat

import (
	"fmt"
	"time"
)

// ETACalculator 基于字节进度估算剩余时间（Codex Tension T5 决策）。
//
// 设计取舍：
//   - 字节 denominator：SQLite decrypt 近似 CPU bound，解密速率相对稳定，比文件
//     数 denominator (小文件被锁 + 大文件占大头) 更准
//   - < minWindow (30s) 返回"计算中..."：冷启噪声大，过早算 ETA 会跳变误导用户
//   - 模糊分钟展示：不说"剩 2m 34s"而说"约 3 分钟"——精度要求低，时间感粗粒度
//     即可；避免用户盯着倒计时心情焦虑（Codex 指出锁/AV/fsync 让精确值本就不准）
//
// 用法：
//
//	eta := NewETACalculator(startedAt)
//	... 收到 progress event ...
//	text := eta.Format(evt.BytesDone, evt.BytesTotal)
//	// text: "计算中..." / "42s" / "约 3 分钟"
type ETACalculator struct {
	startedAt time.Time
	minWindow time.Duration // 低于此的数据量不算 ETA
}

// NewETACalculator 构造一个 calculator。startedAt 通常来自 ProgressEvent.StartedAt。
func NewETACalculator(startedAt time.Time) *ETACalculator {
	return &ETACalculator{startedAt: startedAt, minWindow: 30 * time.Second}
}

// Format 返回模糊化的 ETA 字符串。
//
// 语义：
//   - ""             任务已完成 / 无效输入（bytesTotal<=0 或 done>=total）
//   - "计算中..."    数据不足（< minWindow 或 bytesDone=0）
//   - "Xs"           <1 分钟
//   - "约 Xm"        >=1 分钟，分钟取整
func (c *ETACalculator) Format(bytesDone, bytesTotal int64) string {
	if bytesTotal <= 0 {
		return ""
	}
	if bytesDone >= bytesTotal {
		return ""
	}
	if bytesDone <= 0 {
		return "计算中..."
	}
	elapsed := time.Since(c.startedAt)
	if elapsed < c.minWindow {
		return "计算中..."
	}
	// remaining = (total - done) * elapsed / done
	remaining := time.Duration(float64(bytesTotal-bytesDone) * float64(elapsed) / float64(bytesDone))
	return formatETADuration(remaining)
}

// formatETADuration 把 duration 模糊化成用户友好字符串。
// < 60s  → "Xs"
// >= 60s → "约 Xm"（分钟取整）
func formatETADuration(d time.Duration) string {
	if d < time.Minute {
		secs := int(d.Seconds())
		if secs < 1 {
			secs = 1 // 避免 "0s"
		}
		return fmt.Sprintf("%ds", secs)
	}
	mins := int(d.Minutes())
	if mins < 1 {
		mins = 1
	}
	return fmt.Sprintf("约 %d 分钟", mins)
}
