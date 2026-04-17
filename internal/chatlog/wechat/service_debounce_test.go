package wechat

import (
	"testing"
	"time"
)

// --- 默认值契约 ---

func TestDebounceTime_DefaultIs60Seconds(t *testing.T) {
	want := 60 * time.Second
	if DebounceTime != want {
		t.Errorf("DebounceTime default = %v, want %v", DebounceTime, want)
	}
}

func TestMaxWaitTime_DefaultIs10Minutes(t *testing.T) {
	want := 10 * time.Minute
	if MaxWaitTime != want {
		t.Errorf("MaxWaitTime default = %v, want %v", MaxWaitTime, want)
	}
}

// --- 配置读取行为 ---

func TestGetDebounceTime_UsesConfigValue(t *testing.T) {
	svc := NewService(&mockConfigDebounce{debounceMs: 5000})
	got := svc.getDebounceTime()
	want := 5 * time.Second
	if got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestGetDebounceTime_FallsBackToDefault(t *testing.T) {
	svc := NewService(&mockConfigDebounce{debounceMs: 0})
	got := svc.getDebounceTime()
	if got != DebounceTime {
		t.Errorf("got %v, want default %v", got, DebounceTime)
	}
}

// --- 实时 DB 特殊加速已移除 ---

func TestRealtimeDBFile_NoLongerGetsSpecialDebounce(t *testing.T) {
	svc := NewService(&mockConfigDebounce{debounceMs: 60000, walEnabled: true})

	realtimeDB := "/data/db_storage/message/message_0.db"
	otherDB := "/data/db_storage/favorite/favorite.db"

	realtimeDebounce := svc.getDebounceTimeForFile(realtimeDB)
	otherDebounce := svc.getDebounceTimeForFile(otherDB)

	if realtimeDebounce != otherDebounce {
		t.Errorf("realtime DB debounce (%v) should equal other DB debounce (%v) — special case removed",
			realtimeDebounce, otherDebounce)
	}
	if realtimeDebounce != 60*time.Second {
		t.Errorf("realtime DB debounce = %v, want 60s (config value, no cap)", realtimeDebounce)
	}
}

func TestRealtimeDBFile_NoLongerGetsSpecialMaxWait(t *testing.T) {
	svc := NewService(&mockConfigDebounce{debounceMs: 60000, walEnabled: true})

	realtimeDB := "/data/db_storage/message/message_0.db"
	otherDB := "/data/db_storage/favorite/favorite.db"

	realtimeMaxWait := svc.getMaxWaitTimeForFile(realtimeDB)
	otherMaxWait := svc.getMaxWaitTimeForFile(otherDB)

	if realtimeMaxWait != otherMaxWait {
		t.Errorf("realtime DB maxWait (%v) should equal other DB maxWait (%v) — special case removed",
			realtimeMaxWait, otherMaxWait)
	}
}

// --- maxWait 在 WAL 模式下不再被硬压到 3 秒 ---

func TestGetMaxWaitTime_WALModeNotCappedAt3s(t *testing.T) {
	svc := NewService(&mockConfigDebounce{debounceMs: 60000, walEnabled: true})
	got := svc.getMaxWaitTime()
	// 旧行为会返回 3s 上限，新行为应回退 MaxWaitTime 默认（10 分钟）
	if got <= 3*time.Second {
		t.Errorf("maxWait = %v, should not be capped at 3s anymore", got)
	}
}

func TestGetMaxWaitTime_UsesDefault(t *testing.T) {
	svc := NewService(&mockConfigDebounce{debounceMs: 60000, walEnabled: false})
	got := svc.getMaxWaitTime()
	if got != MaxWaitTime {
		t.Errorf("got %v, want default %v", got, MaxWaitTime)
	}
}

// --- mock ---

type mockConfigDebounce struct {
	debounceMs int
	walEnabled bool
}

func (m *mockConfigDebounce) GetDataKey() string          { return "" }
func (m *mockConfigDebounce) GetDataDir() string          { return "" }
func (m *mockConfigDebounce) GetWorkDir() string          { return "" }
func (m *mockConfigDebounce) GetPlatform() string         { return "windows" }
func (m *mockConfigDebounce) GetVersion() int             { return 4 }
func (m *mockConfigDebounce) GetWalEnabled() bool         { return m.walEnabled }
func (m *mockConfigDebounce) GetAutoDecryptDebounce() int { return m.debounceMs }
