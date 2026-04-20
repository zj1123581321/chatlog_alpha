package ctx

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestRefresh_DataUsageWalk_NoConcurrentSpawn 关键 invariant：
// 当 dataUsage walk 还在进行中时，反复调 Refresh 必须只有 1 个 walk goroutine 在跑，
// 不能 spawn 新的并发 walk —— 否则 10w+ 文件的 msg/attach 会被 N 个 walk 同时扫，
// 与微信 IO 严重争抢，导致用户打开图片卡顿（实测 3-5s）。
func TestRefresh_DataUsageWalk_NoConcurrentSpawn(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})

	origDataFn := getDataDirSizeFn
	origWorkFn := getWorkDirSizeFn
	getDataDirSizeFn = func(dir string) string {
		calls.Add(1)
		<-block // 阻塞模拟 walk 慢
		return "1 GB"
	}
	getWorkDirSizeFn = func(dir string) string { return "" } // 不阻塞 work 路径
	t.Cleanup(func() {
		getDataDirSizeFn = origDataFn
		getWorkDirSizeFn = origWorkFn
		close(block) // 让阻塞中的 goroutine 退出
	})

	c := newTestContextWithDataDir(t, "/tmp/dummy")

	// 快速调 5 次 Refresh（模拟 TUI 每秒 tick + 用户操作触发的额外 refresh）
	for i := 0; i < 5; i++ {
		c.Refresh()
		time.Sleep(2 * time.Millisecond)
	}

	// 给 walk goroutine 启动的时间
	time.Sleep(50 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("期望同时只有 1 个 walk 在 inflight，实际 spawn 了 %d 个", got)
	}
}

// TestRefresh_DataUsage_AfterWalkCompletes_NoFurtherSpawn：walk 完成后 dataUsage 已设值，
// 后续 Refresh 不应再 spawn（这是已有行为，但要确保 inflight guard 不破坏它）。
func TestRefresh_DataUsage_AfterWalkCompletes_NoFurtherSpawn(t *testing.T) {
	var calls atomic.Int32

	origDataFn := getDataDirSizeFn
	origWorkFn := getWorkDirSizeFn
	getDataDirSizeFn = func(dir string) string {
		calls.Add(1)
		return "2 GB"
	}
	getWorkDirSizeFn = func(dir string) string { return "" }
	t.Cleanup(func() {
		getDataDirSizeFn = origDataFn
		getWorkDirSizeFn = origWorkFn
	})

	c := newTestContextWithDataDir(t, "/tmp/dummy")

	c.Refresh()
	// 等 walk 完成（上面 stub 立即返回，5ms 足够）
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if c.GetDataUsage() != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c.GetDataUsage() != "2 GB" {
		t.Fatalf("walk 应完成并设值，实际 dataUsage=%q", c.GetDataUsage())
	}

	for i := 0; i < 3; i++ {
		c.Refresh()
	}
	time.Sleep(20 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("walk 完成后不应再 spawn，实际总 spawn 数 %d", got)
	}
}

// TestRefresh_WorkUsageWalk_NoConcurrentSpawn：workUsage 同样有 inflight guard。
// 这条路径会跑 GetDirSize(workDir) + GetDirSize(cacheDir) 两次 walk，更需要 guard。
func TestRefresh_WorkUsageWalk_NoConcurrentSpawn(t *testing.T) {
	var calls atomic.Int32
	block := make(chan struct{})

	origDataFn := getDataDirSizeFn
	origWorkFn := getWorkDirSizeFn
	getDataDirSizeFn = func(dir string) string { return "" }
	getWorkDirSizeFn = func(dir string) string {
		calls.Add(1)
		<-block
		return "500 MB"
	}
	t.Cleanup(func() {
		getDataDirSizeFn = origDataFn
		getWorkDirSizeFn = origWorkFn
		close(block)
	})

	c := newTestContextWithWorkDir(t, "/tmp/work")

	for i := 0; i < 5; i++ {
		c.Refresh()
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Errorf("workUsage 路径同样应只 1 个 walk inflight，实际 %d", got)
	}
}

// newTestContextWithDataDir 构造一个最小可用的 Context，dataDir 已设值。
// 故意绕开 New() 的 config 加载，避免测试依赖文件系统。
func newTestContextWithDataDir(t *testing.T, dataDir string) *Context {
	t.Helper()
	return &Context{
		dataDir: dataDir,
	}
}

func newTestContextWithWorkDir(t *testing.T, workDir string) *Context {
	t.Helper()
	return &Context{
		workDir: workDir,
	}
}
