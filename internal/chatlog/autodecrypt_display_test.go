package chatlog

import (
	"strings"
	"testing"
	"time"

	"github.com/sjzar/chatlog/internal/chatlog/wechat"
)

func TestBuildAutoDecryptText_Idle_NotEnabled(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhaseIdle, nil, false, 0)
	if got != "[未开启]" {
		t.Errorf("got %q, want [未开启]", got)
	}
}

func TestBuildAutoDecryptText_Idle_EnabledShowsRecovering(t *testing.T) {
	// recovery 刚启动瞬时：phase 还没切出 Idle，但 autoDecrypt flag 已 true
	got := buildAutoDecryptText(wechat.PhaseIdle, nil, true, 0)
	if !strings.Contains(got, "恢复中") {
		t.Errorf("got %q, should contain 恢复中", got)
	}
}

func TestBuildAutoDecryptText_Precheck(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhasePrecheck, nil, true, 0)
	if !strings.Contains(got, "预检中") {
		t.Errorf("got %q, should contain 预检中", got)
	}
}

func TestBuildAutoDecryptText_FirstFull_NoProgress(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhaseFirstFull, nil, true, 0)
	if !strings.Contains(got, "数据同步") || !strings.Contains(got, "准备中") {
		t.Errorf("got %q, should show 数据同步 + 准备中", got)
	}
}

func TestBuildAutoDecryptText_FirstFull_WithProgress(t *testing.T) {
	evt := &wechat.ProgressEvent{
		Phase:       wechat.PhaseFirstFull,
		FilesDone:   12,
		FilesTotal:  42,
		BytesDone:   285 * 1024 * 1024,  // ~28.5%
		BytesTotal:  1000 * 1024 * 1024, // 1 GB
		CurrentFile: "message_3.db",
		StartedAt:   time.Now().Add(-60 * time.Second), // 过了 60s 触发 ETA
		UpdatedAt:   time.Now(),
	}
	got := buildAutoDecryptText(wechat.PhaseFirstFull, evt, true, 0)

	if !strings.Contains(got, "12/42") {
		t.Errorf("got %q, should contain 12/42", got)
	}
	if !strings.Contains(got, "28%") && !strings.Contains(got, "29%") {
		t.Errorf("got %q, should contain ~28%% pct", got)
	}
	// ETA 因为 StartedAt 60s 前 + 28% 进度 → 剩 ~150s ≈ "约 2 分钟" 或 "2 分钟"
	if !strings.Contains(got, "分钟") && !strings.Contains(got, "s") {
		t.Errorf("got %q, should contain ETA (分钟 or s)", got)
	}
}

func TestBuildAutoDecryptText_FirstFull_BelowETAWindow(t *testing.T) {
	evt := &wechat.ProgressEvent{
		Phase:      wechat.PhaseFirstFull,
		FilesDone:  1,
		FilesTotal: 42,
		BytesDone:  100,
		BytesTotal: 1000,
		StartedAt:  time.Now().Add(-5 * time.Second), // <30s, ETA 返回"计算中..."
		UpdatedAt:  time.Now(),
	}
	got := buildAutoDecryptText(wechat.PhaseFirstFull, evt, true, 0)

	if !strings.Contains(got, "1/42") {
		t.Errorf("got %q, should contain 1/42", got)
	}
	// 应包含 "计算中" ETA 文本
	if !strings.Contains(got, "计算中") {
		t.Errorf("got %q, should contain '计算中' when <30s", got)
	}
}

func TestBuildAutoDecryptText_Live_WithDebounce(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhaseLive, nil, true, 60000)
	if !strings.Contains(got, "已开启") || !strings.Contains(got, "60000ms") {
		t.Errorf("got %q, should show 已开启 + debounce", got)
	}
}

func TestBuildAutoDecryptText_Live_NoDebounce(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhaseLive, nil, true, 0)
	if !strings.Contains(got, "已开启") || strings.Contains(got, "ms") {
		t.Errorf("got %q, should show 已开启 only (no ms)", got)
	}
}

func TestBuildAutoDecryptText_Failed(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhaseFailed, nil, false, 0)
	if !strings.Contains(got, "已失败") {
		t.Errorf("got %q, should contain 已失败", got)
	}
}

func TestBuildAutoDecryptText_Stopping(t *testing.T) {
	got := buildAutoDecryptText(wechat.PhaseStopping, nil, true, 0)
	if !strings.Contains(got, "停止中") {
		t.Errorf("got %q, should contain 停止中", got)
	}
}
