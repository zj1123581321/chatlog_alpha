package wechat

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGenerationPoller_FirstReadMissingStatus(t *testing.T) {
	dir := t.TempDir()
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: time.Hour, // 不让 ticker 自跑
		OnChange: func(string) error { t.Fatalf("should not fire OnChange when status missing"); return nil },
		OnError:  func(error) { t.Fatalf("should not fire OnError when status missing (treated as not-yet-initialized)") },
	})
	if err := p.CheckOnce(); err != nil {
		t.Errorf("expected no error on missing status, got %v", err)
	}
}

func TestGenerationPoller_FirstReadFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	if err := WriteStatusAtomic(dir, Status{
		CurrentGeneration: "GEN-A",
		GenerationID:      "GEN-A",
		Healthy:           true,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var got string
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: time.Hour,
		OnChange: func(s string) error { got = s; return nil },
	})
	if err := p.CheckOnce(); err != nil {
		t.Fatalf("CheckOnce: %v", err)
	}
	if got != "GEN-A" {
		t.Errorf("expected OnChange(GEN-A), got %q", got)
	}
}

func TestGenerationPoller_NoChangeNoFire(t *testing.T) {
	dir := t.TempDir()
	if err := WriteStatusAtomic(dir, Status{CurrentGeneration: "GEN-A"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var calls int32
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: time.Hour,
		OnChange: func(string) error { atomic.AddInt32(&calls, 1); return nil },
	})
	_ = p.CheckOnce() // 第一次：fire OnChange
	_ = p.CheckOnce() // 第二次：current 没变，不该 fire
	_ = p.CheckOnce()
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected 1 OnChange call, got %d", calls)
	}
}

func TestGenerationPoller_DetectsChange(t *testing.T) {
	dir := t.TempDir()
	if err := WriteStatusAtomic(dir, Status{CurrentGeneration: "GEN-A"}); err != nil {
		t.Fatalf("seed1: %v", err)
	}
	var seq []string
	var mu sync.Mutex
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: time.Hour,
		OnChange: func(s string) error { mu.Lock(); seq = append(seq, s); mu.Unlock(); return nil },
	})
	_ = p.CheckOnce()

	if err := WriteStatusAtomic(dir, Status{CurrentGeneration: "GEN-B"}); err != nil {
		t.Fatalf("seed2: %v", err)
	}
	_ = p.CheckOnce()
	_ = p.CheckOnce() // 再来一次，不该重复 fire

	mu.Lock()
	defer mu.Unlock()
	if len(seq) != 2 || seq[0] != "GEN-A" || seq[1] != "GEN-B" {
		t.Errorf("expected fire sequence [GEN-A, GEN-B], got %v", seq)
	}
}

func TestGenerationPoller_BadStatusFiresOnError(t *testing.T) {
	dir := t.TempDir()
	// 写一个语法损坏的 status.json
	if err := os.WriteFile(filepath.Join(dir, StatusFileName), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var errs []error
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: time.Hour,
		OnChange: func(string) error { t.Fatalf("should not fire OnChange on bad json"); return nil },
		OnError:  func(e error) { errs = append(errs, e) },
	})
	_ = p.CheckOnce()
	if len(errs) != 1 {
		t.Errorf("expected 1 OnError call, got %d", len(errs))
	}
}

func TestGenerationPoller_StartStop(t *testing.T) {
	dir := t.TempDir()
	if err := WriteStatusAtomic(dir, Status{CurrentGeneration: "GEN-A"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var calls int32
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: 5 * time.Millisecond,
		OnChange: func(string) error { atomic.AddInt32(&calls, 1); return nil },
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 等若干 tick
	time.Sleep(50 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if atomic.LoadInt32(&calls) < 1 {
		t.Errorf("expected ≥1 OnChange call after start, got %d", calls)
	}
	// Stop 后状态不该再被改动
	before := atomic.LoadInt32(&calls)
	time.Sleep(20 * time.Millisecond)
	if atomic.LoadInt32(&calls) != before {
		t.Errorf("calls increased after Stop")
	}
}

func TestGenerationPoller_DoubleStartIsError(t *testing.T) {
	dir := t.TempDir()
	p := NewGenerationPoller(GenerationPollerOpts{
		WorkDir:  dir,
		Interval: time.Hour,
		OnChange: func(string) error { return nil },
	})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()
	if err := p.Start(); err == nil {
		t.Errorf("expected error on double Start")
	}
}

