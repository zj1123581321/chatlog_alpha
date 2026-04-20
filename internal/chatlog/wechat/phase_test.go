package wechat

import (
	"sync"
	"testing"
	"time"
)

func TestGetPhase_InitialIsIdle(t *testing.T) {
	svc := NewService(&mockConfig{})
	if got := svc.GetPhase(); got != PhaseIdle {
		t.Errorf("initial phase = %q, want %q", got, PhaseIdle)
	}
}

func TestSetPhase_Transitions(t *testing.T) {
	svc := NewService(&mockConfig{})
	transitions := []AutoDecryptPhase{
		PhasePrecheck, PhaseFirstFull, PhaseLive, PhaseStopping, PhaseIdle,
	}
	for _, p := range transitions {
		svc.SetPhase(p)
		if got := svc.GetPhase(); got != p {
			t.Errorf("after SetPhase(%q), got %q", p, got)
		}
	}
}

func TestGetLastRun_NilInitially(t *testing.T) {
	svc := NewService(&mockConfig{})
	if got := svc.GetLastRun(); got != nil {
		t.Errorf("initial lastRun should be nil, got %+v", got)
	}
}

func TestGetLastRun_SnapshotIndependence(t *testing.T) {
	svc := NewService(&mockConfig{})
	orig := AutoDecryptLastRun{
		StartedAt:    time.Now(),
		DurationSecs: 60,
		FinalPhase:   PhaseLive,
		FilesTotal:   42,
	}
	svc.setLastRun(orig)

	// caller 修改返回的副本不应影响内部 state
	got := svc.GetLastRun()
	if got == nil {
		t.Fatal("GetLastRun returned nil after setLastRun")
	}
	got.FilesTotal = 999

	got2 := svc.GetLastRun()
	if got2.FilesTotal != 42 {
		t.Errorf("internal state mutated: got FilesTotal=%d, want 42", got2.FilesTotal)
	}
}

func TestGetPhase_ConcurrentReads_RaceClean(t *testing.T) {
	// 用 go test -race 跑：验证 phase 并发读不爆 race detector
	svc := NewService(&mockConfig{})
	svc.SetPhase(PhaseFirstFull)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = svc.GetPhase()
			}
		}()
	}
	// 同时有 writer 切 phase
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			phases := []AutoDecryptPhase{PhaseLive, PhaseIdle, PhasePrecheck}
			for j := 0; j < 50; j++ {
				svc.SetPhase(phases[j%len(phases)])
			}
		}(i)
	}
	wg.Wait()
}
