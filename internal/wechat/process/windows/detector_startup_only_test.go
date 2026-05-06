package windows

import (
	"testing"
	"time"

	"github.com/sjzar/chatlog/internal/wechat/model"
)

// TestGetOrProbe_PopulatedDataDirCachedForever 是 Step 2 的核心契约:
// DataDir 一旦识别出来, 在 PID 生命周期内绝不重探 (除非 prune).
// PID 进程内 DataDir 是不变量, 重探只会泄漏 gopsutil HANDLE 没有任何收益.
//
// 这把 OpenFiles 的调用频率从 "每秒一次 / 每 60s 重探" 降到
// "每个 PID 生命周期一次", 等价于 startup-only 语义 ——
// supervisor restart 时探一次, 之后整个 watcher 寿命内 (天/周量级) 都不再调.
func TestGetOrProbe_PopulatedDataDirCachedForever(t *testing.T) {
	d := NewDetector()
	var probeCount int

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 42, DataDir: `D:\xwechat_files\wxid_x`}, nil
	}

	// 第一次必须 probe
	if _, err := d.getOrProbe(42, probe); err != nil {
		t.Fatal(err)
	}
	if probeCount != 1 {
		t.Fatalf("first call must probe, got count=%d", probeCount)
	}

	// 模拟"过去很久" —— 把 lastProbed 倒拨 24h
	d.mu.Lock()
	d.cache[42].lastProbed = time.Now().Add(-24 * time.Hour)
	d.mu.Unlock()

	// 24h 后再调 999 次, 一次都不能 reprobe
	for i := 0; i < 999; i++ {
		info, err := d.getOrProbe(42, probe)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if info.DataDir != `D:\xwechat_files\wxid_x` {
			t.Fatalf("call %d: cache must hold DataDir, got %q", i, info.DataDir)
		}
	}

	if probeCount != 1 {
		t.Errorf("populated DataDir must NEVER reprobe (PID生命周期内 DataDir 不变), got probeCount=%d want=1", probeCount)
	}
}

// TestGetOrProbe_EmptyDataDirStillRespectsShortTTL: 空 DataDir 仍然走原来的
// EmptyInfoProbeCacheTTL 短 TTL 兜底 —— 这是给"chatlog 启动时微信还没登录"
// 这种场景留窗口, 让登录后能在合理时间内被发现.
//
// 这个 case 保留旧行为, 不能因为 Step 2 改造误伤.
func TestGetOrProbe_EmptyDataDirStillRespectsShortTTL(t *testing.T) {
	d := NewDetector()
	var probeCount int

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 7, DataDir: ""}, nil
	}

	// 短 TTL 内连续 5 次只 probe 1 次
	for i := 0; i < 5; i++ {
		if _, err := d.getOrProbe(7, probe); err != nil {
			t.Fatal(err)
		}
	}
	if probeCount != 1 {
		t.Errorf("empty DataDir must still cache within EmptyInfoProbeCacheTTL, got %d want 1", probeCount)
	}

	// 倒拨到 EmptyInfoProbeCacheTTL 之前, 应该 reprobe
	d.mu.Lock()
	d.cache[7].lastProbed = time.Now().Add(-EmptyInfoProbeCacheTTL - time.Second)
	d.mu.Unlock()

	if _, err := d.getOrProbe(7, probe); err != nil {
		t.Fatal(err)
	}
	if probeCount != 2 {
		t.Errorf("empty DataDir must reprobe after EmptyInfoProbeCacheTTL, got %d want 2", probeCount)
	}
}

// TestGetOrProbe_EmptyToPopulatedTransition: 微信登录瞬间 DataDir 从空变成有值,
// 转换那次 probe 之后就锁定 DataDir, 永远不再重探.
func TestGetOrProbe_EmptyToPopulatedTransition(t *testing.T) {
	d := NewDetector()
	var probeCount int
	var dataDir string

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 99, DataDir: dataDir}, nil
	}

	// 阶段 1: DataDir 还没识别出来 (微信未登录)
	dataDir = ""
	if _, err := d.getOrProbe(99, probe); err != nil {
		t.Fatal(err)
	}
	if probeCount != 1 {
		t.Fatalf("phase 1: probe count=%d want=1", probeCount)
	}

	// 倒拨 TTL 让下次 reprobe (此时微信已登录)
	d.mu.Lock()
	d.cache[99].lastProbed = time.Now().Add(-EmptyInfoProbeCacheTTL - time.Second)
	d.mu.Unlock()

	dataDir = `D:\xwechat_files\wxid_y` // 微信登录了
	if _, err := d.getOrProbe(99, probe); err != nil {
		t.Fatal(err)
	}
	if probeCount != 2 {
		t.Fatalf("phase 2: probe count=%d want=2", probeCount)
	}

	// 阶段 3: DataDir 锁定后 N 次 reprobe attempts 都应该走缓存
	d.mu.Lock()
	d.cache[99].lastProbed = time.Now().Add(-24 * time.Hour) // 倒拨极远
	d.mu.Unlock()

	dataDir = "" // 即使 probe 现在会返回空 (不该被调用)
	for i := 0; i < 100; i++ {
		info, err := d.getOrProbe(99, probe)
		if err != nil {
			t.Fatal(err)
		}
		if info.DataDir != `D:\xwechat_files\wxid_y` {
			t.Fatalf("phase 3 call %d: DataDir lost, got %q", i, info.DataDir)
		}
	}
	if probeCount != 2 {
		t.Errorf("after DataDir locked, must NEVER reprobe, got count=%d want=2", probeCount)
	}
}

// TestGetOrProbe_PrunedPIDReprobesOnReturn: PID 被 prune 后如果同 PID 再出现
// (PID 复用场景, 微信关闭重启可能拿到相同 PID), 必须走完整 probe 路径.
func TestGetOrProbe_PrunedPIDReprobesOnReturn(t *testing.T) {
	d := NewDetector()
	var probeCount int

	probe := func() (*model.Process, error) {
		probeCount++
		return &model.Process{PID: 1234, DataDir: `D:\x`}, nil
	}

	if _, err := d.getOrProbe(1234, probe); err != nil {
		t.Fatal(err)
	}

	// PID 死了被 prune
	d.pruneStale(map[uint32]bool{})

	// 同 PID 再出现 (PID 复用), 必须重 probe
	if _, err := d.getOrProbe(1234, probe); err != nil {
		t.Fatal(err)
	}

	if probeCount != 2 {
		t.Errorf("after prune, returning PID must reprobe, got %d want 2", probeCount)
	}
}
