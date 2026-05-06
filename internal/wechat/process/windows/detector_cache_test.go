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

// TestGetOrProbe_EmptyDataDirCachedShortTTL 验证: DataDir 为空也缓存，但 TTL 较短。
// 主犯现场 (Weixin 主进程不持续持 session.db、子进程根本没 db_storage 路径)
// 让首轮探测得到的 DataDir 一直是空，如果这种情况不缓存就会每秒命中 gopsutil
// OpenFilesWithContext 的 HANDLE 泄漏 bug。
func TestGetOrProbe_EmptyDataDirCachedShortTTL(t *testing.T) {
	d := NewDetector()
	var probeCount int

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 1234, DataDir: ""}, nil
	}

	// 短 TTL 内连续 5 次调用应只 probe 1 次
	for i := 0; i < 5; i++ {
		if _, err := d.getOrProbe(1234, probe); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if probeCount != 1 {
		t.Errorf("empty DataDir should still cache within EmptyInfoProbeCacheTTL, got probeCount=%d want=1", probeCount)
	}

	// 把 lastProbed 倒拨到 EmptyInfoProbeCacheTTL 之前，应该 reprobe
	d.mu.Lock()
	d.cache[1234].lastProbed = time.Now().Add(-EmptyInfoProbeCacheTTL - time.Second)
	d.mu.Unlock()

	if _, err := d.getOrProbe(1234, probe); err != nil {
		t.Fatal(err)
	}
	if probeCount != 2 {
		t.Errorf("empty DataDir should reprobe after EmptyInfoProbeCacheTTL, got probeCount=%d want=2", probeCount)
	}
}

// 注: 旧测试 TestGetOrProbe_EmptyDataDirTTLShorterThanPopulated 和
// TestGetOrProbe_ExpiredTTLReprobes 在 Step 2 后语义不再适用 ——
// DataDir 非空时永久缓存, 没有 ProbeCacheTTL 概念了. 它们的关切点
// (空 DataDir 短 TTL / PID 复用重探) 由 detector_startup_only_test.go 里
// 的 TestGetOrProbe_EmptyDataDirStillRespectsShortTTL 和
// TestGetOrProbe_PrunedPIDReprobesOnReturn 覆盖.

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
