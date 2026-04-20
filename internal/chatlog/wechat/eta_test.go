package wechat

import (
	"strings"
	"testing"
	"time"
)

func TestETACalculator_BelowMinWindow_ReturnsCalculating(t *testing.T) {
	// 刚启动 1 秒，数据不足
	eta := NewETACalculator(time.Now().Add(-1 * time.Second))
	got := eta.Format(100, 1000)
	if got != "计算中..." {
		t.Errorf("got %q, want '计算中...'", got)
	}
}

func TestETACalculator_ZeroBytesDone_ReturnsCalculating(t *testing.T) {
	// 即使过了 minWindow，如果 bytesDone=0 也无从外推
	eta := NewETACalculator(time.Now().Add(-60 * time.Second))
	got := eta.Format(0, 1000)
	if got != "计算中..." {
		t.Errorf("got %q, want '计算中...'", got)
	}
}

func TestETACalculator_Completed_ReturnsEmpty(t *testing.T) {
	eta := NewETACalculator(time.Now().Add(-60 * time.Second))
	if got := eta.Format(1000, 1000); got != "" {
		t.Errorf("fully done: got %q, want empty", got)
	}
	if got := eta.Format(1500, 1000); got != "" {
		t.Errorf("over done: got %q, want empty", got)
	}
}

func TestETACalculator_ZeroTotal_ReturnsEmpty(t *testing.T) {
	eta := NewETACalculator(time.Now().Add(-60 * time.Second))
	if got := eta.Format(0, 0); got != "" {
		t.Errorf("zero total: got %q, want empty", got)
	}
}

func TestETACalculator_LinearExtrapolation_Minutes(t *testing.T) {
	// 场景：过了 60s 解了 100MB / 共 1000MB → 还要 9 倍时间 = 540s ≈ 9 分钟
	eta := NewETACalculator(time.Now().Add(-60 * time.Second))
	got := eta.Format(100*1024*1024, 1000*1024*1024)
	// 应该以"约 9 分钟"类格式展示
	if !strings.HasPrefix(got, "约 ") || !strings.HasSuffix(got, " 分钟") {
		t.Errorf("got %q, want '约 N 分钟' format", got)
	}
}

func TestETACalculator_LinearExtrapolation_Seconds(t *testing.T) {
	// 场景：过了 60s 解了 900MB / 共 1000MB → 还要 1/9 时间 ≈ 6.67s ≈ "6s"~"7s"
	eta := NewETACalculator(time.Now().Add(-60 * time.Second))
	got := eta.Format(900*1024*1024, 1000*1024*1024)
	// 应该以秒级格式展示
	if !strings.HasSuffix(got, "s") || strings.Contains(got, "分") {
		t.Errorf("got %q, want 'Ns' format (remaining < 1 min)", got)
	}
}

func TestFormatETADuration_SubSecondRoundsUp(t *testing.T) {
	// <1s 应显示 1s，避免 "0s"
	if got := formatETADuration(500 * time.Millisecond); got != "1s" {
		t.Errorf("got %q, want 1s (sub-second rounds up)", got)
	}
}

func TestFormatETADuration_MinuteBoundary(t *testing.T) {
	// 恰好 1 分钟
	if got := formatETADuration(60 * time.Second); got != "约 1 分钟" {
		t.Errorf("got %q, want '约 1 分钟'", got)
	}
}

func TestFormatETADuration_LongRun(t *testing.T) {
	if got := formatETADuration(15 * time.Minute); got != "约 15 分钟" {
		t.Errorf("got %q, want '约 15 分钟'", got)
	}
}

func TestFormatETADuration_SubMinute(t *testing.T) {
	// 40 秒
	if got := formatETADuration(40 * time.Second); got != "40s" {
		t.Errorf("got %q, want '40s'", got)
	}
}
