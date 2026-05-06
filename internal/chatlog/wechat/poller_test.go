package wechat

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 公共 helper：建一个 poller，pattern/blacklist 与 StartAutoDecrypt 一致。
// interval=1h 避免后台 goroutine 干扰；测试都用 TickOnce 手动驱动。
func newTestPoller(t *testing.T, dir string, callback ChangeCallback) *IntervalPoller {
	t.Helper()
	p, err := NewIntervalPoller(dir, `.*\.db(-wal|-shm)?$`, []string{"fts"}, time.Hour, callback)
	if err != nil {
		t.Fatalf("NewIntervalPoller: %v", err)
	}
	return p
}

// 写一个文件并指定 mtime；返回路径。
func writeFileAt(t *testing.T, path string, content string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// callback recorder：线程安全收集触发的 path。
type recordingCallback struct {
	mu    sync.Mutex
	paths []string
}

func (r *recordingCallback) cb() ChangeCallback {
	return func(path string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.paths = append(r.paths, path)
		return nil
	}
}

func (r *recordingCallback) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.paths))
	copy(out, r.paths)
	return out
}

// TestNewIntervalPoller_NilCallback: 防御性 — nil callback 应该 NewIntervalPoller 直接报错。
func TestNewIntervalPoller_NilCallback(t *testing.T) {
	_, err := NewIntervalPoller(t.TempDir(), `.*\.db$`, nil, time.Hour, nil)
	if err == nil {
		t.Fatal("expected error for nil callback, got nil")
	}
}

// TestNewIntervalPoller_BadPattern: 正则编译错误应该 surface。
func TestNewIntervalPoller_BadPattern(t *testing.T) {
	_, err := NewIntervalPoller(t.TempDir(), `[invalid`, nil, time.Hour, func(string) error { return nil })
	if err == nil {
		t.Fatal("expected error for invalid pattern, got nil")
	}
}

// TestNewIntervalPoller_DefaultInterval: interval<=0 应回退到 default。
func TestNewIntervalPoller_DefaultInterval(t *testing.T) {
	p, err := NewIntervalPoller(t.TempDir(), `.*\.db$`, nil, 0, func(string) error { return nil })
	if err != nil {
		t.Fatalf("NewIntervalPoller: %v", err)
	}
	if p.interval != DefaultDecryptPollInterval {
		t.Errorf("interval = %v, want default %v", p.interval, DefaultDecryptPollInterval)
	}
}

// TestTickOnce_EmptyDir_NoCallback: 空目录第一次 tick 不触发回调。
func TestTickOnce_EmptyDir_NoCallback(t *testing.T) {
	rec := &recordingCallback{}
	p := newTestPoller(t, t.TempDir(), rec.cb())
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("expected no callbacks, got %v", got)
	}
}

// TestTickOnce_BaselineSuppressFirstCallbacks: 已有文件第一次 tick 仅 baseline，不触发回调
// （与 fsnotify "events only after Start" 的语义对齐，避免 startup 触发全量重解）。
func TestTickOnce_BaselineSuppressFirstCallbacks(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "message_0.db"), "x", time.Now())
	writeFileAt(t, filepath.Join(dir, "session.db"), "x", time.Now())

	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("TickOnce: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("baseline tick should not fire callbacks, got %v", got)
	}
}

// TestTickOnce_FireOnMtimeAdvance: baseline 之后某文件 mtime 前进，下次 tick 应触发回调。
func TestTickOnce_FireOnMtimeAdvance(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "message_0.db")
	t0 := time.Now().Add(-time.Hour) // 旧 mtime
	writeFileAt(t, target, "x", t0)

	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	// baseline
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline tick: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("baseline tick fired %d callbacks, want 0", len(got))
	}

	// advance mtime
	t1 := time.Now()
	if err := os.Chtimes(target, t1, t1); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// next tick should fire
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 callback, got %d: %v", len(got), got)
	}
	if got[0] != target {
		t.Errorf("callback path = %q, want %q", got[0], target)
	}
}

// TestTickOnce_FireOnNewFile: baseline 之后新增的文件下一次 tick 触发。
func TestTickOnce_FireOnNewFile(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	// baseline (空目录)
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	target := filepath.Join(dir, "message_1.db")
	writeFileAt(t, target, "x", time.Now())

	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || got[0] != target {
		t.Errorf("got %v, want [%q]", got, target)
	}
}

// TestTickOnce_NoFireOnUnchanged: baseline 之后无变化，不应再触发。
func TestTickOnce_NoFireOnUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "message_0.db"), "x", time.Now())

	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	for i := 0; i < 3; i++ {
		if err := p.TickOnce(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("no-change ticks should not fire, got %v", got)
	}
}

// TestTickOnce_PatternFilters: 不匹配的文件不被纳入。
func TestTickOnce_PatternFilters(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "ignore.txt"), "x", time.Now().Add(-time.Hour))
	writeFileAt(t, filepath.Join(dir, "message_0.db"), "x", time.Now().Add(-time.Hour))

	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	// baseline (.db 文件先记录)
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// 修改两个文件
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "ignore.txt"), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "message_0.db"), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || filepath.Base(got[0]) != "message_0.db" {
		t.Errorf("got %v, want only message_0.db", got)
	}
}

// TestTickOnce_BlacklistExcludes: blacklist 子串匹配的路径被跳过。
func TestTickOnce_BlacklistExcludes(t *testing.T) {
	dir := t.TempDir()
	writeFileAt(t, filepath.Join(dir, "fts", "fts.db"), "x", time.Now().Add(-time.Hour))
	writeFileAt(t, filepath.Join(dir, "message", "message_0.db"), "x", time.Now().Add(-time.Hour))

	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// 修改两个 db
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "fts", "fts.db"), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "message", "message_0.db"), now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || filepath.Base(got[0]) != "message_0.db" {
		t.Errorf("got %v, want only message_0.db (fts blacklisted)", got)
	}
}

// TestTickOnce_DeletedFileNoCallback: baseline 之后删除文件，不触发回调（也不报错）。
func TestTickOnce_DeletedFileNoCallback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "message_0.db")
	writeFileAt(t, target, "x", time.Now().Add(-time.Hour))

	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("deletion should not fire callback, got %v", got)
	}
}

// TestTickOnce_RecursiveScan: 子目录内的匹配文件也应被发现。
func TestTickOnce_RecursiveScan(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())

	// baseline (空)
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	target := filepath.Join(dir, "sub", "deep", "message_0.db")
	writeFileAt(t, target, "x", time.Now())

	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || got[0] != target {
		t.Errorf("got %v, want [%q]", got, target)
	}
}

// TestTickOnce_MissingRootDirIsNonFatal: rootDir 不存在不应报错（微信关闭时的合理状态）。
func TestTickOnce_MissingRootDirIsNonFatal(t *testing.T) {
	rec := &recordingCallback{}
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	p := newTestPoller(t, missing, rec.cb())

	if err := p.TickOnce(context.Background()); err != nil {
		t.Errorf("missing root should not error, got %v", err)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("expected no callbacks, got %v", got)
	}
}

// TestStart_Stop_LifecycleCleansUp: Start 后 Stop 必须等待 goroutine 退出。
func TestStart_Stop_LifecycleCleansUp(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingCallback{}
	p, err := NewIntervalPoller(dir, `.*\.db$`, nil, 50*time.Millisecond, rec.cb())
	if err != nil {
		t.Fatalf("NewIntervalPoller: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// 让它跑几个 tick
	time.Sleep(180 * time.Millisecond)
	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 双 Stop 不应 panic
	if err := p.Stop(); err != nil {
		t.Errorf("second Stop should be no-op, got %v", err)
	}
}

// TestStart_DoubleStart_ReturnsError: 重复 Start 应报错而不是悄悄起两个 goroutine。
func TestStart_DoubleStart_ReturnsError(t *testing.T) {
	p, err := NewIntervalPoller(t.TempDir(), `.*\.db$`, nil, time.Hour, func(string) error { return nil })
	if err != nil {
		t.Fatalf("NewIntervalPoller: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	if err := p.Start(); err == nil {
		t.Error("second Start should error")
	}
}

// TestStart_FiresInBackground: Start 模式下能检测到运行期变化（端到端验证 goroutine 调度）。
func TestStart_FiresInBackground(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "message_0.db")
	writeFileAt(t, target, "x", time.Now().Add(-time.Hour))

	var fires int32
	cb := func(path string) error {
		if path == target {
			atomic.AddInt32(&fires, 1)
		}
		return nil
	}
	p, err := NewIntervalPoller(dir, `.*\.db$`, nil, 30*time.Millisecond, cb)
	if err != nil {
		t.Fatalf("NewIntervalPoller: %v", err)
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Stop() }()

	// 等首次 baseline 完成
	time.Sleep(60 * time.Millisecond)

	// 触发变化
	now := time.Now()
	if err := os.Chtimes(target, now, now); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	// 等下一次 tick (~30ms+overhead)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fires) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&fires) == 0 {
		t.Error("expected at least 1 fire from background goroutine, got 0")
	}
}

// TestTickOnce_ContextCancelDuringCallback: callback 阶段 ctx 取消应中断剩余 callback 派发。
func TestTickOnce_ContextCancelDuringCallback(t *testing.T) {
	dir := t.TempDir()
	rec := &recordingCallback{}
	p := newTestPoller(t, dir, rec.cb())
	if err := p.TickOnce(context.Background()); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	// 写 3 个新文件
	for _, n := range []string{"a.db", "b.db", "c.db"} {
		writeFileAt(t, filepath.Join(dir, n), "x", time.Now())
	}

	// ctx 已 cancel：TickOnce 应当 surface ctx.Err，回调可能部分执行也可能完全没执行
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := p.TickOnce(ctx)
	if err == nil {
		t.Error("expected ctx.Err from cancelled TickOnce, got nil")
	}
}
