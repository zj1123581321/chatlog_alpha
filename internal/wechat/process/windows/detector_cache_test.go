package windows

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjzar/chatlog/internal/wechat/model"
)

// TestGetOrProbe_CacheHitSkipsProbe 验证: 已缓存且 DataDir 非空、未过 TTL 时
// 不再调用 probe，等价于稳态下不再触发 gopsutil p.OpenFiles()。
func TestGetOrProbe_CacheHitSkipsProbe(t *testing.T) {
	d := NewDetector()
	var probeCount int

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 1234, DataDir: `D:\xwechat_files\wxid_x`}, nil
	}

	for i := 0; i < 10; i++ {
		info, err := d.getOrProbe(1234, probe)
		if err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
		if info == nil || info.DataDir == "" {
			t.Fatalf("expected non-empty DataDir, got %+v", info)
		}
	}

	if probeCount != 1 {
		t.Errorf("expected probe called exactly once, got %d", probeCount)
	}
}

// TestGetOrProbe_DataDirEmptyBypassesCache 验证: DataDir 为空（未登录微信）
// 每次都应重新探测，等微信登录拿到路径。
func TestGetOrProbe_DataDirEmptyBypassesCache(t *testing.T) {
	d := NewDetector()
	var probeCount int

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 1234, DataDir: ""}, nil
	}

	for i := 0; i < 5; i++ {
		if _, err := d.getOrProbe(1234, probe); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if probeCount != 5 {
		t.Errorf("empty DataDir should bypass cache, got probeCount=%d want=5", probeCount)
	}
}

// TestGetOrProbe_ExpiredTTLReprobes 验证: 缓存过期后重新探测。
func TestGetOrProbe_ExpiredTTLReprobes(t *testing.T) {
	d := NewDetector()
	var probeCount int
	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 7, DataDir: "D:/x"}, nil
	}

	if _, err := d.getOrProbe(7, probe); err != nil {
		t.Fatal(err)
	}

	// 手动把 lastProbed 倒拨到 TTL 之前
	d.mu.Lock()
	d.cache[7].lastProbed = time.Now().Add(-ProbeCacheTTL - time.Second)
	d.mu.Unlock()

	if _, err := d.getOrProbe(7, probe); err != nil {
		t.Fatal(err)
	}

	if probeCount != 2 {
		t.Errorf("expected reprobe after TTL expiry, probeCount=%d want=2", probeCount)
	}
}

// TestGetOrProbe_ProbeErrorNotCached 验证: probe 返回错误时不写缓存，下次会重试。
func TestGetOrProbe_ProbeErrorNotCached(t *testing.T) {
	d := NewDetector()
	var probeCount int
	probe := func() (*model.Process, error) {
		probeCount++
		return nil, errors.New("boom")
	}

	for i := 0; i < 3; i++ {
		if _, err := d.getOrProbe(99, probe); err == nil {
			t.Fatal("expected error")
		}
	}

	if probeCount != 3 {
		t.Errorf("error path should not cache, probeCount=%d want=3", probeCount)
	}

	d.mu.Lock()
	_, cached := d.cache[99]
	d.mu.Unlock()
	if cached {
		t.Error("expected no cache entry after probe error")
	}
}

// TestPruneStale_RemovesDeadPIDs 验证: 探测一轮中没看见的 PID 被清出缓存。
func TestPruneStale_RemovesDeadPIDs(t *testing.T) {
	d := NewDetector()
	probe := func(pid uint32) func() (*model.Process, error) {
		return func() (*model.Process, error) {
			return &model.Process{PID: pid, DataDir: "D:/x"}, nil
		}
	}

	for _, pid := range []uint32{10, 20, 30} {
		if _, err := d.getOrProbe(pid, probe(pid)); err != nil {
			t.Fatal(err)
		}
	}

	d.pruneStale(map[uint32]bool{20: true})

	d.mu.Lock()
	_, hasOld := d.cache[10]
	_, hasLive := d.cache[20]
	_, hasAnother := d.cache[30]
	d.mu.Unlock()

	if hasOld || hasAnother {
		t.Error("dead PIDs should be pruned")
	}
	if !hasLive {
		t.Error("live PID should remain cached")
	}
}

// TestGetOrProbe_ConcurrentCallsSingleProbePerHit 压测下缓存仍然抑制调用次数。
// 真实 chatlog 里 TUI 每秒 tick，HTTP handler 偶有调用；并发下缓存不能失效。
func TestGetOrProbe_ConcurrentCallsSingleProbePerHit(t *testing.T) {
	d := NewDetector()
	var (
		probeCount int
		probeMu    sync.Mutex
	)

	probe := func() (*model.Process, error) {
		probeMu.Lock()
		probeCount++
		probeMu.Unlock()
		return &model.Process{PID: 1, DataDir: "D:/x"}, nil
	}

	// 先热缓存一次
	if _, err := d.getOrProbe(1, probe); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.getOrProbe(1, probe); err != nil {
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()

	probeMu.Lock()
	count := probeCount
	probeMu.Unlock()

	if count != 1 {
		t.Errorf("after warm-up, concurrent hits should not re-probe, got count=%d want=1", count)
	}
}
